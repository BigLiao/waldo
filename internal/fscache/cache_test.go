package fscache

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pkg/sftp"
)

type observation struct {
	mu           sync.Mutex
	counts       map[byte]int
	responseHook func([]byte)
}

func (o *observation) count(kind byte) int  { o.mu.Lock(); defer o.mu.Unlock(); return o.counts[kind] }
func (o *observation) hook(fn func([]byte)) { o.mu.Lock(); o.responseHook = fn; o.mu.Unlock() }

type fixture struct {
	cache   *Cache
	client  *sftp.Client
	observe *observation
	root    string
	raw     net.Conn
}

func setup(t *testing.T, metadataTTL, contentTTL time.Duration) *fixture {
	return setupConnection(t, metadataTTL, contentTTL, false)
}

func setupConnection(t *testing.T, metadataTTL, contentTTL time.Duration, raw bool) *fixture {
	t.Helper()
	root := t.TempDir()
	c := New(root, metadataTTL, contentTTL)
	clientConn, front := net.Pipe()
	upstream, observerIn := net.Pipe()
	observerOut, serverConn := net.Pipe()
	connections := []net.Conn{clientConn, front, upstream, observerIn, observerOut, serverConn}
	for _, conn := range connections {
		conn.SetDeadline(time.Now().Add(20 * time.Second))
	}
	o := &observation{counts: make(map[byte]int)}
	go func() {
		defer observerOut.Close()
		for {
			packet, err := readWirePacket(observerIn)
			if err != nil {
				return
			}
			o.mu.Lock()
			o.counts[packet[0]]++
			o.mu.Unlock()
			if writeWirePacket(observerOut, packet) != nil {
				return
			}
		}
	}()
	go func() {
		defer observerIn.Close()
		for {
			packet, err := readWirePacket(observerOut)
			if err != nil {
				return
			}
			o.mu.Lock()
			hook := o.responseHook
			o.mu.Unlock()
			if hook != nil {
				hook(packet)
			}
			if writeWirePacket(observerIn, packet) != nil {
				return
			}
		}
	}()
	server, err := sftp.NewServer(serverConn)
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve()
	done := make(chan error, 1)
	go func() { done <- c.Serve(front, upstream) }()
	var client *sftp.Client
	if !raw {
		client, err = sftp.NewClientPipe(clientConn, clientConn)
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if client != nil {
			client.Close()
		}
		for _, conn := range connections {
			conn.Close()
		}
		server.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("cache proxy did not stop")
		}
	})
	return &fixture{cache: c, client: client, observe: o, root: root, raw: clientConn}
}

func (f *fixture) file(t *testing.T, name, data string) string {
	t.Helper()
	p := filepath.Join(f.root, name)
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
func (f *fixture) read(t *testing.T, p string) string {
	t.Helper()
	h, err := f.client.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Stat(); err != nil {
		h.Close()
		t.Fatal(err)
	}
	data, err := io.ReadAll(h)
	if err != nil {
		h.Close()
		t.Fatal(err)
	}
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestWarmReadAvoidsUpstreamOpenReadCloseAndStat(t *testing.T) {
	f := setup(t, time.Minute, time.Minute)
	p := f.file(t, "small", "hello world")
	f.cache.SetEnabled(true)
	if got := f.read(t, p); got != "hello world" {
		t.Fatal(got)
	}
	if _, err := f.client.Stat(p); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.Lstat(p); err != nil {
		t.Fatal(err)
	}
	before := map[byte]int{}
	for _, kind := range []byte{openPacket, readPacket, closePacket, fstatPacket, statPacket, lstatPacket} {
		before[kind] = f.observe.count(kind)
	}
	for i := 0; i < 3; i++ {
		if got := f.read(t, p); got != "hello world" {
			t.Fatal(got)
		}
		if _, err := f.client.Stat(p); err != nil {
			t.Fatal(err)
		}
		if _, err := f.client.Lstat(p); err != nil {
			t.Fatal(err)
		}
	}
	for kind, want := range before {
		if got := f.observe.count(kind); got != want {
			t.Errorf("packet kind %d: upstream count grew %d -> %d", kind, want, got)
		}
	}
}

func TestDisabledAndExpiredLeaseReadThrough(t *testing.T) {
	f := setup(t, time.Minute, time.Minute)
	p := f.file(t, "small", "before")
	f.read(t, p)
	f.read(t, p)
	if got := f.observe.count(openPacket); got != 2 {
		t.Fatalf("disabled cache forwarded %d opens", got)
	}
	f.cache.SetEnabled(true)
	f.read(t, p)
	before := f.observe.count(openPacket)
	f.cache.mu.Lock()
	f.cache.leaseUntil = time.Now().Add(-time.Millisecond)
	f.cache.mu.Unlock()
	if err := os.WriteFile(p, []byte("after!"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := f.read(t, p); got != "after!" {
		t.Fatal(got)
	}
	if got := f.observe.count(openPacket); got != before+1 {
		t.Fatalf("expired lease served cached open: %d -> %d", before, got)
	}
	f.cache.SetEnabled(true)
	f.read(t, p)
	f.cache.SetEnabled(false)
	f.cache.mu.Lock()
	defer f.cache.mu.Unlock()
	if len(f.cache.files) != 0 || len(f.cache.metadata) != 0 || f.cache.used != 0 {
		t.Fatal("disable retained cached data")
	}
}

func TestZeroMetadataTTLStillLearnsExactSizedRead(t *testing.T) {
	f := setup(t, 0, time.Minute)
	p := f.file(t, "small", "hello world")
	f.cache.SetEnabled(true)
	info, err := f.client.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	h, err := f.client.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, info.Size())
	if _, err := h.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	before := f.observe.count(openPacket)
	h, err = f.client.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	h.Close()
	if f.observe.count(openPacket) != before {
		t.Fatal("size hint did not enable cached open")
	}
	before = f.observe.count(statPacket)
	if _, err := f.client.Stat(p); err != nil {
		t.Fatal(err)
	}
	if f.observe.count(statPacket) != before+1 {
		t.Fatal("zero metadata TTL served cached STAT")
	}
}

func TestStalePathSizeDoesNotTruncateCapture(t *testing.T) {
	f := setup(t, time.Minute, time.Minute)
	p := f.file(t, "grown", "old")
	f.cache.SetEnabled(true)
	if _, err := f.client.Stat(p); err != nil {
		t.Fatal(err)
	}
	// The watch event has not arrived yet. The path STAT is deliberately stale
	// when OPEN obtains the descriptor for the now-larger file.
	if err := os.WriteFile(p, []byte("old-and-new"), 0600); err != nil {
		t.Fatal(err)
	}
	h, err := f.client.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	prefix := make([]byte, 3)
	if _, err := h.ReadAt(prefix, 0); err != nil {
		t.Fatal(err)
	}
	suffix := make([]byte, 8)
	if _, err := h.ReadAt(suffix, 3); err != nil {
		t.Fatalf("stale path size synthesized EOF for real file data: %v", err)
	}
	if string(prefix)+string(suffix) != "old-and-new" {
		t.Fatalf("truncated read: %q%q", prefix, suffix)
	}
}

func TestStalePathSizeCannotPublishPartialFileOnClose(t *testing.T) {
	f := setup(t, time.Minute, time.Minute)
	p := f.file(t, "grown", "old")
	f.cache.SetEnabled(true)
	if _, err := f.client.Stat(p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("old-and-new"), 0600); err != nil {
		t.Fatal(err)
	}
	h, err := f.client.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.ReadAt(make([]byte, 3), 0); err != nil {
		t.Fatal(err)
	}
	before := f.observe.count(fstatPacket)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if f.observe.count(fstatPacket) != before+1 {
		t.Fatal("exact read close did not confirm descriptor size")
	}
	f.cache.mu.Lock()
	cached := len(f.cache.files)
	f.cache.mu.Unlock()
	if cached != 0 {
		t.Fatal("published truncated contents using stale path size")
	}
	if got := f.read(t, p); got != "old-and-new" {
		t.Fatal(got)
	}
}

func TestFileCanGrowAfterDescriptorStatWithoutSyntheticEOF(t *testing.T) {
	f := setup(t, time.Minute, time.Minute)
	p := f.file(t, "grown", "old")
	f.cache.SetEnabled(true)
	h, err := f.client.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Stat(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("old-and-new"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ReadAt(make([]byte, 3), 0); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	if _, err := h.ReadAt(buf, 3); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "-and-new" {
		t.Fatalf("wrong suffix %q", buf)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	before := f.observe.count(openPacket)
	if got := f.read(t, p); got != "old-and-new" {
		t.Fatal(got)
	}
	if f.observe.count(openPacket) != before {
		t.Fatal("confirmed complete file was not retained")
	}
}

func TestInvalidationDuringCloseConfirmationCannotPublish(t *testing.T) {
	f := setup(t, time.Minute, time.Minute)
	p := f.file(t, "small", "hello")
	f.cache.SetEnabled(true)
	h, err := f.client.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.ReadAt(make([]byte, 5), 0); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.observe.hook(func(packet []byte) {
		if packet[0] == attrsPacket {
			once.Do(func() { close(entered); <-release })
		}
	})
	done := make(chan error, 1)
	go func() { done <- h.Close() }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("close did not request descriptor confirmation")
	}
	f.cache.Invalidate()
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close blocked after invalidation")
	}
	f.cache.mu.Lock()
	cached, used := len(f.cache.files), f.cache.used
	f.cache.mu.Unlock()
	if cached != 0 || used != 0 {
		t.Fatalf("invalidated confirmation published data: files=%d bytes=%d", cached, used)
	}
}

func TestItemExpirationDoesNotDependOnGlobalSweep(t *testing.T) {
	f := setup(t, time.Minute, time.Minute)
	p := f.file(t, "small", "before")
	f.cache.SetEnabled(true)
	f.read(t, p)
	h, err := f.client.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.Stat(p); err != nil {
		t.Fatal(err)
	}
	f.cache.mu.Lock()
	f.cache.nextSweep = time.Now().Add(time.Hour)
	f.cache.files[p].expires = time.Now().Add(-time.Second)
	f.cache.metadata[metaKey{statPacket, p}].expires = time.Now().Add(-time.Second)
	f.cache.files[p].remote.attrs.expires = time.Now().Add(-time.Second)
	f.cache.mu.Unlock()
	if err := os.WriteFile(p, []byte("after-and-longer"), 0600); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := h.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "after" {
		t.Fatalf("expired content served without global sweep: %q", buf)
	}
	info, err := h.Stat()
	if err != nil || info.Size() != 16 {
		t.Fatalf("expired descriptor metadata served: %v %v", info, err)
	}
	info, err = f.client.Stat(p)
	if err != nil || info.Size() != 16 {
		t.Fatalf("expired path metadata served: %v %v", info, err)
	}
	before := f.observe.count(openPacket)
	if got := f.read(t, p); got != "after-and-longer" {
		t.Fatal(got)
	}
	if f.observe.count(openPacket) != before+1 {
		t.Fatal("expired OPEN hit without global sweep")
	}
	h.Close()
}

func TestCaptureGrowthIsGeometricAndBudgetUsesCapacity(t *testing.T) {
	c := New("/project", time.Minute, time.Minute)
	c.SetEnabled(true)
	rf := &remoteFile{path: "/project/small", capture: &capture{generation: c.generation, expires: time.Now().Add(time.Minute), regular: true}}
	chunk := bytes.Repeat([]byte{'x'}, 256)
	growths, lastCapacity := 0, 0
	for offset := 0; offset < maxFileSize; offset += len(chunk) {
		c.learnLocked(rf, uint64(offset), chunk, false, c.generation)
		if rf.capture == nil {
			t.Fatal("capture unexpectedly discarded")
		}
		capacity := cap(rf.capture.data)
		if capacity != lastCapacity {
			growths++
			lastCapacity = capacity
		}
		if c.used != capacity {
			t.Fatalf("charged len rather than cap: used=%d cap=%d", c.used, capacity)
		}
	}
	if growths > 9 {
		t.Fatalf("reallocated for individual chunks: %d buffer growths", growths)
	}
	if !bytes.Equal(rf.capture.data, bytes.Repeat([]byte{'x'}, maxFileSize)) {
		t.Fatal("geometric growth lost data")
	}
	c.learnLocked(rf, maxFileSize+1, chunk, false, c.generation)
	if c.used != 0 {
		t.Fatalf("discard failed to release capacity budget: %d", c.used)
	}
}

func TestConcurrentCapturesCannotExceedCapacityBudget(t *testing.T) {
	c := New("/project", time.Minute, time.Minute)
	c.SetEnabled(true)
	data := make([]byte, maxFileSize)
	var charged int
	var captures []*remoteFile
	for i := 0; i < 40; i++ {
		rf := &remoteFile{path: fmt.Sprintf("/project/%d", i), capture: &capture{generation: c.generation, expires: time.Now().Add(time.Minute)}}
		c.learnLocked(rf, 0, data, false, c.generation)
		if rf.capture != nil {
			charged += cap(rf.capture.data)
			captures = append(captures, rf)
		}
	}
	if charged != c.used || c.used > maxBytes-maxMetadataBytes {
		t.Fatalf("capture capacity exceeds budget: charged=%d used=%d", charged, c.used)
	}
	if len(captures) != 30 {
		t.Fatalf("expected fixed 30 MiB content budget, retained %d MiB", len(captures))
	}
	for _, rf := range captures {
		c.learnLocked(rf, maxFileSize+1, []byte{0}, false, c.generation)
	}
	if c.used != 0 {
		t.Fatalf("budget leaked after discard: %d", c.used)
	}
}

func BenchmarkMetadataSweep(b *testing.B) {
	for _, full := range []bool{false, true} {
		name := "throttled"
		if full {
			name = "forced_full_scan"
		}
		b.Run(name, func(b *testing.B) {
			c := New("/project", time.Minute, time.Minute)
			now := time.Now()
			for i := 0; i < maxMetadata; i++ {
				c.metadata[metaKey{statPacket, fmt.Sprintf("/project/%d", i)}] = &metadata{expires: now.Add(time.Minute)}
			}
			c.sweepLocked(now)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if full {
					c.nextSweep = time.Time{}
				}
				c.sweepLocked(now)
			}
		})
	}
}

func TestContentTTLIsAbsoluteAcrossHitsAndLeaseRenewal(t *testing.T) {
	f := setup(t, time.Minute, time.Minute)
	p := f.file(t, "small", "before")
	f.cache.SetEnabled(true)
	f.read(t, p)
	f.cache.mu.Lock()
	expires := time.Now().Add(120 * time.Millisecond)
	f.cache.files[p].expires = expires
	f.cache.mu.Unlock()
	before := f.observe.count(openPacket)
	f.cache.SetEnabled(true)
	f.read(t, p)
	f.cache.mu.Lock()
	gotExpires := f.cache.files[p].expires
	f.cache.mu.Unlock()
	if !gotExpires.Equal(expires) {
		t.Fatal("hit extended absolute TTL")
	}
	if got := f.observe.count(openPacket); got != before {
		t.Fatal("expected warm open")
	}
	if err := os.WriteFile(p, []byte("after!"), 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(expires) + 10*time.Millisecond)
	f.cache.SetEnabled(true)
	if got := f.read(t, p); got != "after!" {
		t.Fatal(got)
	}
	if got := f.observe.count(openPacket); got != before+1 {
		t.Fatal("expired content served warm open")
	}
}

func TestWritesAreUpstreamAndDisableHitsWhileHandleOpen(t *testing.T) {
	f := setup(t, time.Minute, time.Minute)
	p := f.file(t, "small", "before")
	f.cache.SetEnabled(true)
	f.read(t, p)
	old, err := f.client.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	w, err := f.client.OpenFile(p, os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.WriteAt([]byte("after!"), 0); err != nil {
		t.Fatal(err)
	}
	before := f.observe.count(openPacket)
	if got := f.read(t, p); got != "after!" {
		t.Fatal(got)
	}
	if got := f.observe.count(openPacket); got != before+1 {
		t.Fatal("cached while writable descriptor was open")
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 6)
	if _, err = old.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "after!" {
		t.Fatalf("invalidated virtual descriptor did not fall back: %q", buf)
	}
	old.Close()
	actual, err := os.ReadFile(p)
	if err != nil || string(actual) != "after!" {
		t.Fatalf("write did not reach OS: %q %v", actual, err)
	}
	f.cache.mu.Lock()
	busy := f.cache.busy
	f.cache.mu.Unlock()
	if busy != 0 {
		t.Fatalf("writer close left cache disabled: %d", busy)
	}
}

func TestRetainedHandleSurvivesRenameAndUnlink(t *testing.T) {
	f := setup(t, time.Minute, time.Minute)
	p := f.file(t, "small", "original")
	f.cache.SetEnabled(true)
	f.read(t, p)
	old, err := f.client.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.client.PosixRename(p, p+".moved"); err != nil {
		t.Fatal(err)
	}
	w, err := f.client.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write([]byte("replacement")); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	if err = f.client.Remove(p + ".moved"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	if _, err = old.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "original" {
		t.Fatalf("descriptor reopened replaced path: %q", buf)
	}
	if info, err := old.Stat(); err != nil || info.Size() != 8 {
		t.Fatalf("fstat after unlink: %v %v", info, err)
	}
	if err = old.Close(); err != nil {
		t.Fatal(err)
	}
	if got := f.read(t, p); got != "replacement" {
		t.Fatal(got)
	}
}

func TestFstatNeverUsesAttributesOfReplacementPath(t *testing.T) {
	f := setup(t, time.Minute, time.Minute)
	p := f.file(t, "small", "old")
	f.cache.SetEnabled(true)
	f.read(t, p)
	old, err := f.client.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p, p+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("new longer file"), 0600); err != nil {
		t.Fatal(err)
	}
	// Reproduce the window before a change notification arrives: a path STAT
	// refreshes, while the original descriptor must still name the old inode.
	f.cache.mu.Lock()
	m := f.cache.metadata[metaKey{statPacket, p}]
	m.expires = time.Now().Add(-time.Millisecond)
	f.cache.mu.Unlock()
	info, err := f.client.Stat(p)
	if err != nil || info.Size() != 15 {
		t.Fatalf("replacement path stat: %v %v", info, err)
	}
	info, err = old.Stat()
	if err != nil || info.Size() != 3 {
		t.Fatalf("old descriptor used replacement attributes: %v %v", info, err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataAndMutatingExtensionsPassThrough(t *testing.T) {
	f := setup(t, time.Minute, time.Minute)
	p := f.file(t, "small", "abcdef")
	f.cache.SetEnabled(true)
	f.read(t, p)
	h, err := f.client.OpenFile(p, os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.Truncate(3); err != nil {
		t.Fatal(err)
	}
	if err = h.Chmod(0640); err != nil {
		t.Fatal(err)
	}
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	if err = f.client.Link(p, p+".link"); err != nil {
		t.Fatal(err)
	}
	if err = f.client.PosixRename(p, p+".moved"); err != nil {
		t.Fatal(err)
	}
	if err = f.client.Symlink(p+".moved", p+".symlink"); err != nil {
		t.Fatal(err)
	}
	if err = f.client.Mkdir(p + ".dir"); err != nil {
		t.Fatal(err)
	}
	if err = f.client.RemoveDirectory(p + ".dir"); err != nil {
		t.Fatal(err)
	}
	if got := f.read(t, p+".link"); got != "abc" {
		t.Fatal(got)
	}
	info, err := os.Stat(p + ".moved")
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatalf("metadata mutation lost: %v %v", info, err)
	}
}

func TestInvalidateDiscardsInflightFill(t *testing.T) {
	f := setup(t, time.Minute, time.Minute)
	p := f.file(t, "small", "old-content")
	f.cache.SetEnabled(true)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.observe.hook(func(packet []byte) {
		if packet[0] == dataPacket {
			once.Do(func() { close(entered); <-release })
		}
	})
	done := make(chan string, 1)
	go func() {
		h, err := f.client.Open(p)
		if err != nil {
			done <- err.Error()
			return
		}
		h.Stat()
		data, err := io.ReadAll(h)
		h.Close()
		if err != nil {
			done <- err.Error()
			return
		}
		done <- string(data)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("read did not reach server")
	}
	if err := os.WriteFile(p, []byte("new-content"), 0600); err != nil {
		t.Fatal(err)
	}
	f.cache.Invalidate()
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("read blocked")
	}
	f.cache.mu.Lock()
	cached := len(f.cache.files)
	f.cache.mu.Unlock()
	if cached != 0 {
		t.Fatal("old in-flight data repopulated cache")
	}
	if got := f.read(t, p); got != "new-content" {
		t.Fatal(got)
	}
}

func TestOpenCachedHandleFallsBackAfterLeaseExpires(t *testing.T) {
	f := setup(t, time.Minute, time.Minute)
	p := f.file(t, "small", "before")
	f.cache.SetEnabled(true)
	f.read(t, p)
	h, err := f.client.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	f.cache.mu.Lock()
	f.cache.leaseUntil = time.Now().Add(-time.Millisecond)
	f.cache.mu.Unlock()
	if err := os.WriteFile(p, []byte("after!"), 0600); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 6)
	if _, err = h.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "after!" {
		t.Fatalf("lease-expired handle served old bytes %q", buf)
	}
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSizeAndMetadataBudgets(t *testing.T) {
	f := setup(t, time.Minute, time.Minute)
	p := f.file(t, "large", string(bytes.Repeat([]byte{'x'}, maxFileSize+1)))
	f.cache.SetEnabled(true)
	f.read(t, p)
	before := f.observe.count(openPacket)
	f.read(t, p)
	if got := f.observe.count(openPacket); got != before+1 {
		t.Fatal("oversized file was cached")
	}
	f.cache.mu.Lock()
	for i := 0; i < maxMetadata; i++ {
		f.cache.storeMetaLocked(statPacket, fmt.Sprintf("%s/%d", f.root, i), attrsPacket, make([]byte, 4096), time.Now())
	}
	used, metadataUsed := f.cache.used, f.cache.metadataBytes
	f.cache.mu.Unlock()
	if used > maxBytes || metadataUsed > maxMetadataBytes {
		t.Fatalf("unbounded cache: %d %d", used, metadataUsed)
	}
}

func TestNonCanonicalAndOutsidePathsAreNeverCacheKeys(t *testing.T) {
	c := New("/project", time.Minute, time.Minute)
	for _, p := range []string{"/project/link/../x", "/project//x", "/project/./x", "/project/../outside", "/outside/x", "relative"} {
		if _, ok := c.cachePath(p); ok {
			t.Errorf("cache accepted ambiguous/outside path %q", p)
		}
	}
}

func TestOutOfOrderReadsOnlyCacheCompleteFiles(t *testing.T) {
	f := setup(t, time.Minute, time.Minute)
	p := f.file(t, "ranges", "0123456789abcdef")
	f.cache.SetEnabled(true)
	if _, err := f.client.Stat(p); err != nil {
		t.Fatal(err)
	}
	h, err := f.client.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	for _, s := range []span{{4, 8}, {0, 2}, {2, 4}, {12, 16}} {
		if _, err := h.ReadAt(buf[s.start:s.end], int64(s.start)); err != nil {
			t.Fatal(err)
		}
	}
	f.cache.mu.Lock()
	cached := len(f.cache.files)
	f.cache.mu.Unlock()
	if cached != 0 {
		t.Fatal("cached file with an unread hole")
	}
	if _, err := h.ReadAt(buf[8:12], 8); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	before := f.observe.count(openPacket)
	if got := f.read(t, p); got != "0123456789abcdef" {
		t.Fatal(got)
	}
	if f.observe.count(openPacket) != before {
		t.Fatal("complete out-of-order read was not cached")
	}
}

func TestOnlyRegularFileAttributesPermitContentCaching(t *testing.T) {
	for _, mode := range []uint32{0010000 | 0600, 0020000 | 0600, 0040000 | 0700, 0120000 | 0777, 0140000 | 0600, 0600} {
		body := binary.BigEndian.AppendUint32(nil, 1|2|4)
		body = binary.BigEndian.AppendUint64(body, 0)
		body = binary.BigEndian.AppendUint32(body, 1000)
		body = binary.BigEndian.AppendUint32(body, 1000)
		body = binary.BigEndian.AppendUint32(body, mode)
		_, _, regular := attributeInfo(body)
		if regular {
			t.Errorf("special/untyped mode %o treated as regular", mode)
		}
		c := New("/project", time.Minute, time.Minute)
		rf := &remoteFile{path: "/project/special", capture: &capture{knownSize: true, regular: regular}}
		c.completeLocked(rf)
		if len(c.files) != 0 {
			t.Fatalf("cached special/untyped mode %o", mode)
		}
	}
}

func TestFailedWriteOpenReleasesDirtyStateAndRetainedDescriptors(t *testing.T) {
	f := setup(t, time.Minute, time.Minute)
	p := f.file(t, "small", "hello")
	f.cache.SetEnabled(true)
	f.read(t, p)
	if _, err := f.client.OpenFile(filepath.Join(f.root, "missing", "file"), os.O_WRONLY|os.O_CREATE); err == nil {
		t.Fatal("expected failing open")
	}
	f.cache.mu.Lock()
	busy := f.cache.busy
	f.cache.mu.Unlock()
	if busy != 0 {
		t.Fatalf("failed mutation left busy=%d", busy)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.cache.mu.Lock()
		retained := len(f.cache.proxy.remotes)
		f.cache.mu.Unlock()
		if retained == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("retained descriptors not closed: %d", retained)
		}
		time.Sleep(time.Millisecond)
	}
	if got := f.read(t, p); got != "hello" {
		t.Fatal(got)
	}
}

func TestPipelinedReadThenCloseNeverRetainsClosedDescriptor(t *testing.T) {
	f := setupConnection(t, time.Minute, time.Minute, true)
	p := f.file(t, "small", "hello")
	f.cache.SetEnabled(true)
	if err := writeWirePacket(f.raw, binary.BigEndian.AppendUint32([]byte{initPacket}, 3)); err != nil {
		t.Fatal(err)
	}
	if _, err := readWirePacket(f.raw); err != nil {
		t.Fatal(err)
	}
	if err := writeWirePacket(f.raw, reply(statPacket, 1, appendString(nil, p))); err != nil {
		t.Fatal(err)
	}
	if _, err := readWirePacket(f.raw); err != nil {
		t.Fatal(err)
	}
	openBody := appendString(nil, p)
	openBody = binary.BigEndian.AppendUint32(openBody, 1)
	openBody = binary.BigEndian.AppendUint32(openBody, 0)
	if err := writeWirePacket(f.raw, reply(openPacket, 2, openBody)); err != nil {
		t.Fatal(err)
	}
	packet, err := readWirePacket(f.raw)
	if err != nil {
		t.Fatal(err)
	}
	alias, _, ok := packetString(packet[5:])
	if !ok {
		t.Fatal("missing handle")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	f.observe.hook(func(packet []byte) {
		if packet[0] == dataPacket {
			close(entered)
			<-release
		}
	})
	readBody := appendString(nil, alias)
	readBody = binary.BigEndian.AppendUint64(readBody, 0)
	readBody = binary.BigEndian.AppendUint32(readBody, 5)
	sent := make(chan error, 1)
	go func() {
		if err := writeWirePacket(f.raw, reply(readPacket, 3, readBody)); err != nil {
			sent <- err
			return
		}
		sent <- writeWirePacket(f.raw, reply(closePacket, 4, appendString(nil, alias)))
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("READ response was not gated")
	}
	select {
	case err := <-sent:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CLOSE was not sent")
	}
	deadline := time.Now().Add(2 * time.Second)
	for f.observe.count(closePacket) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("CLOSE did not reach upstream")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	for i := 0; i < 2; i++ {
		if _, err := readWirePacket(f.raw); err != nil {
			t.Fatal(err)
		}
	}
	f.cache.mu.Lock()
	cached, used := len(f.cache.files), f.cache.used
	f.cache.mu.Unlock()
	if cached != 0 || used != 0 {
		t.Fatalf("retained descriptor closed by pipelined CLOSE: entries=%d bytes=%d", cached, used)
	}
}

func TestProtocolRejectsOversizedPacket(t *testing.T) {
	packet := binary.BigEndian.AppendUint32(nil, 16<<20+1)
	if _, err := readWirePacket(bytes.NewReader(packet)); err == nil {
		t.Fatal("accepted oversized packet")
	}
}
