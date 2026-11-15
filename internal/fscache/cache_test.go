package fscache

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pkg/sftp"

	"waldo/internal/fileversion"
	"waldo/internal/projection"
)

type observedConn struct {
	net.Conn
	mu     sync.Mutex
	counts map[string]int
}

func (c *observedConn) Write(b []byte) (int, error) {
	if len(b) >= 5 && int(binary.BigEndian.Uint32(b)) == len(b)-4 {
		key := string([]byte{b[4]})
		if b[4] == extendedPacket {
			if ext, _, ok := packetString(b[9:]); ok {
				key = ext
			}
		}
		c.mu.Lock()
		c.counts[key]++
		c.mu.Unlock()
	}
	return c.Conn.Write(b)
}
func (c *observedConn) count(kind byte) int     { return c.countKey(string([]byte{kind})) }
func (c *observedConn) countExt(ext string) int { return c.countKey(ext) }
func (c *observedConn) countKey(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[key]
}
func (c *observedConn) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.counts {
		n += v
	}
	return n
}

type fixture struct {
	root   string
	cache  *Cache
	client *sftp.Client
	wire   *observedConn
	done   chan error
}

func setup(t *testing.T, ttl time.Duration, plain bool) *fixture {
	t.Helper()
	root := t.TempDir()
	client, front := net.Pipe()
	upstream, server := net.Pipe()
	for _, c := range []net.Conn{client, front, upstream, server} {
		c.SetDeadline(time.Now().Add(20 * time.Second))
	}
	wire := &observedConn{Conn: upstream, counts: map[string]int{}}
	if plain {
		s, err := sftp.NewServer(server)
		if err != nil {
			t.Fatal(err)
		}
		go func() { defer s.Close(); s.Serve() }()
	} else {
		go func() { var v *projection.View; v.Serve(server) }()
	}
	cache := New(root, ttl)
	done := make(chan error, 1)
	go func() { done <- cache.Serve(front, wire) }()
	sftpClient, err := sftp.NewClientPipe(client, client)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{root, cache, sftpClient, wire, done}
	t.Cleanup(func() {
		sftpClient.Close()
		client.Close()
		front.Close()
		upstream.Close()
		server.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("cache did not close")
		}
		cache.mu.Lock()
		defer cache.mu.Unlock()
		if cache.used != 0 {
			t.Errorf("retained %d bytes after disconnect", cache.used)
		}
	})
	return f
}
func (f *fixture) file(t *testing.T, name, data string) string {
	t.Helper()
	p := filepath.Join(f.root, name)
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
func (f *fixture) read(t *testing.T, path string) string {
	t.Helper()
	h, err := f.client.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Stat(); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(h)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEveryOpenValidatesButWarmReadTransfersNoContent(t *testing.T) {
	f := setup(t, time.Minute, false)
	path := f.file(t, "source", "hello world")
	if f.read(t, path) != "hello world" {
		t.Fatal("cold read")
	}
	reads := f.wire.count(readPacket)
	opens := f.wire.countExt(fileversion.Open)
	if f.read(t, path) != "hello world" {
		t.Fatal("warm read")
	}
	if f.wire.count(readPacket) != reads || f.wire.countExt(fileversion.Open) != opens+1 || f.wire.count(fstatPacket) != 0 {
		t.Fatalf("warm read did not use one OPEN validation: %+v", f.wire.counts)
	}
}

func TestUnrelatedChangesAndIdleSendNoRequests(t *testing.T) {
	f := setup(t, time.Minute, false)
	path := f.file(t, "source", "stable")
	f.read(t, path)
	before, reads := f.wire.total(), f.wire.count(readPacket)
	for i := 0; i < 100; i++ {
		f.file(t, "busy.log", strings.Repeat("x", i))
	}
	time.Sleep(50 * time.Millisecond)
	if f.wire.total() != before {
		t.Fatal("background notifications or refresh traffic")
	}
	if f.read(t, path) != "stable" || f.wire.count(readPacket) != reads {
		t.Fatal("unrelated log changes discarded source cache")
	}
}

func TestSameSecondSameSizeEditUsesNanosecondVersion(t *testing.T) {
	f := setup(t, time.Minute, false)
	path := f.file(t, "source", "old")
	first := time.Unix(1700000000, 123456100)
	second := first.Add(100 * time.Nanosecond)
	if err := os.Chtimes(path, first, first); err != nil {
		t.Fatal(err)
	}
	f.read(t, path)
	before := f.wire.count(readPacket)
	f.file(t, "source", "new")
	if err := os.Chtimes(path, second, second); err != nil {
		t.Fatal(err)
	}
	if f.read(t, path) != "new" || f.wire.count(readPacket) <= before {
		t.Fatal("same-second edit was missed")
	}
}

func TestAtomicReplacementDeletionAndOpenHandleIdentity(t *testing.T) {
	f := setup(t, time.Minute, false)
	path := f.file(t, "source", "old")
	st, _ := os.Stat(path)
	f.read(t, path)
	old, err := f.client.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := f.file(t, "replacement", "new")
	os.Chtimes(replacement, st.ModTime(), st.ModTime())
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if f.read(t, path) != "new" {
		t.Fatal("atomic replacement reused old inode")
	}
	b, err := io.ReadAll(old)
	if err != nil || string(b) != "old" {
		t.Fatalf("changed an already open snapshot: %q %v", b, err)
	}
	old.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if h, err := f.client.Open(path); err == nil {
		h.Close()
		t.Fatal("opened deleted cached path")
	}
}

func TestWriteThroughAndRenameRefreshWithoutNotification(t *testing.T) {
	f := setup(t, time.Minute, false)
	path := f.file(t, "source", "old")
	f.read(t, path)
	h, err := f.client.OpenFile(path, os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Write([]byte("updated")); err != nil {
		t.Fatal(err)
	}
	h.Close()
	b, _ := os.ReadFile(path)
	if string(b) != "updated" || f.read(t, path) != "updated" {
		t.Fatal("write-through failed")
	}
	target := filepath.Join(f.root, "renamed")
	if err := f.client.PosixRename(path, target); err != nil {
		t.Fatal(err)
	}
	if f.read(t, target) != "updated" {
		t.Fatal("rename failed")
	}
	if _, err := f.client.Stat(path); err == nil {
		t.Fatal("path metadata was stale")
	}
}

func TestOpeningOrPartialReadDoesNotFetchRemainingContent(t *testing.T) {
	f := setup(t, time.Minute, false)
	path := f.file(t, "source", "0123456789")
	h, err := f.client.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	h.Stat()
	h.Close()
	if f.wire.count(readPacket) != 0 {
		t.Fatal("OPEN prefetched file contents")
	}
	h, _ = f.client.Open(path)
	buf := make([]byte, 3)
	h.ReadAt(buf, 0)
	h.Close()
	f.cache.mu.Lock()
	cached := f.cache.files[path] != nil
	f.cache.mu.Unlock()
	if cached {
		t.Fatal("partial data published as complete")
	}
	if f.read(t, path) != "0123456789" {
		t.Fatal("cold full read failed")
	}
	before := f.wire.count(readPacket)
	f.read(t, path)
	if f.wire.count(readPacket) != before {
		t.Fatal("full read did not populate cache")
	}
}

func TestOutOfOrderRangesOnlyPublishCompleteUnchangedFiles(t *testing.T) {
	f := setup(t, time.Minute, false)
	path := f.file(t, "source", "0123456789")
	h, _ := f.client.Open(path)
	h.ReadAt(make([]byte, 4), 6)
	h.ReadAt(make([]byte, 6), 0)
	h.Close()
	before := f.wire.count(readPacket)
	if f.read(t, path) != "0123456789" || f.wire.count(readPacket) != before {
		t.Fatal("complete ranged read was not cached")
	}
	path = f.file(t, "changing", "before")
	h, _ = f.client.Open(path)
	data := make([]byte, 6)
	h.ReadAt(data, 0)
	beforeStat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	f.file(t, "changing", "after!")
	changedTime := beforeStat.ModTime().Add(time.Millisecond)
	if err := os.Chtimes(path, changedTime, changedTime); err != nil {
		t.Fatal(err)
	}
	h.Close()
	f.cache.mu.Lock()
	cached := f.cache.files[path] != nil
	f.cache.mu.Unlock()
	if cached {
		t.Fatal("cached content changed between open and close")
	}
	if f.read(t, path) != "after!" {
		t.Fatal("did not refresh changed source")
	}
}

func TestVanillaServerFallbackAndCacheDisabled(t *testing.T) {
	for _, tc := range []struct {
		plain bool
		ttl   time.Duration
	}{{true, time.Minute}, {false, 0}} {
		f := setup(t, tc.ttl, tc.plain)
		path := f.file(t, "source", "data")
		f.read(t, path)
		before := f.wire.count(readPacket)
		if f.read(t, path) != "data" || f.wire.count(readPacket) <= before {
			t.Fatal("unsupported/disabled cache did not read through")
		}
	}
}

func TestExpiryLargeFilesEmptyFilesAndDirectoryListing(t *testing.T) {
	f := setup(t, 10*time.Millisecond, false)
	path := f.file(t, "source", "data")
	f.read(t, path)
	before := f.wire.count(readPacket)
	time.Sleep(15 * time.Millisecond)
	f.read(t, path)
	if f.wire.count(readPacket) <= before {
		t.Fatal("idle retention did not expire")
	}
	f.cache.mu.Lock()
	f.cache.contentTTL = time.Minute
	f.cache.mu.Unlock()
	large := f.file(t, "large", strings.Repeat("x", maxFileSize+1))
	if len(f.read(t, large)) != maxFileSize+1 {
		t.Fatal("large read truncated")
	}
	before = f.wire.count(readPacket)
	f.read(t, large)
	if f.wire.count(readPacket) <= before {
		t.Fatal("oversized file was cached")
	}
	empty := f.file(t, "empty", "")
	if f.read(t, empty) != "" {
		t.Fatal("empty file")
	}
	before = f.wire.count(readPacket)
	f.read(t, empty)
	if f.wire.count(readPacket) != before {
		t.Fatal("empty file cache miss")
	}
	if _, err := f.client.ReadDir(f.root); err != nil {
		t.Fatal(err)
	}
	f.file(t, "new-entry", "new")
	files, err := f.client.ReadDir(f.root)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range files {
		found = found || e.Name() == "new-entry"
	}
	if !found {
		t.Fatal("directory listing was cached")
	}
}

func TestConcurrentReadersAndBoundedCapacity(t *testing.T) {
	f := setup(t, time.Minute, false)
	path := f.file(t, "source", strings.Repeat("x", 4096))
	f.read(t, path)
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := f.client.Open(path)
			if err != nil {
				t.Error(err)
				return
			}
			b, err := io.ReadAll(h)
			h.Close()
			if err != nil || len(b) != 4096 {
				t.Errorf("concurrent read: %d %v", len(b), err)
			}
		}()
	}
	wg.Wait()
	for i := 0; i < 36; i++ {
		p := f.file(t, string(rune('A'+i)), strings.Repeat("x", maxFileSize))
		if len(f.read(t, p)) != maxFileSize {
			t.Fatal("budget truncated data")
		}
	}
	f.cache.mu.Lock()
	used, n := f.cache.used, len(f.cache.files)
	f.cache.mu.Unlock()
	if used > maxBytes || n > maxFiles {
		t.Fatalf("cache budget exceeded: %d bytes, %d files", used, n)
	}
}

func TestNonCanonicalAndOutsidePathsBypassContentCache(t *testing.T) {
	c := New("/project", time.Minute)
	for _, p := range []string{"relative", "/other/file", "/project/../project/a", "/project2/a"} {
		if c.cachePath(p) {
			t.Fatalf("cached %q", p)
		}
	}
}

func TestCachedOpenStillChecksCurrentPermissions(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses normal read permission checks")
	}
	f := setup(t, time.Minute, false)
	path := f.file(t, "source", "cached")
	f.read(t, path)
	if err := os.Chmod(path, 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0600)
	if h, err := f.client.Open(path); err == nil {
		h.Close()
		t.Fatal("cached open bypassed changed permissions")
	}
}

func TestProtocolRejectsOversizedPacket(t *testing.T) {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint32(16<<20+1))
	if _, err := readWirePacket(&b); err == nil {
		t.Fatal("oversized packet accepted")
	}
}
