//go:build linux

package serve

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/sftp"

	"waldo/internal/fscache"
	"waldo/internal/proto"
)

type countedCacheWire struct {
	net.Conn
	written atomic.Int64
}

func (c *countedCacheWire) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.written.Add(int64(n))
	return n, err
}

type integratedCache struct {
	client  *sftp.Client
	hub     *cacheHub
	wire    *countedCacheWire
	seq     atomic.Uint64
	enabled atomic.Bool
	updated chan struct{}
}

func newIntegratedCache(t *testing.T, root string) *integratedCache {
	t.Helper()
	h := newCacheHub(root)
	cache := fscache.New(root, time.Minute, time.Minute)
	clientSide, proxyFront := net.Pipe()
	proxyUpstream, serverSide := net.Pipe()
	for _, c := range []net.Conn{clientSide, proxyFront, proxyUpstream, serverSide} {
		c.SetDeadline(time.Now().Add(10 * time.Second))
	}
	s, err := sftp.NewServer(serverSide)
	if err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan struct{})
	go func() { defer close(serverDone); defer s.Close(); s.Serve() }()
	result := &integratedCache{hub: h, wire: &countedCacheWire{Conn: proxyUpstream}, updated: make(chan struct{}, 1)}
	proxyDone := make(chan struct{})
	go func() { defer close(proxyDone); cache.Serve(proxyFront, result.wire) }()
	watchClient, watchServer := net.Pipe()
	watchClient.SetDeadline(time.Now().Add(10 * time.Second))
	watchServer.SetDeadline(time.Now().Add(10 * time.Second))
	hubDone := make(chan struct{})
	go func() { defer close(hubDone); h.Serve(watchServer) }()
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		defer cache.SetEnabled(false)
		var previous uint64
		for {
			var ev proto.CacheEvent
			if err := proto.ReadMsg(watchClient, &ev); err != nil {
				return
			}
			if ev.Seq != previous {
				cache.Invalidate()
			}
			cache.SetEnabled(ev.Enabled)
			previous = ev.Seq
			result.seq.Store(ev.Seq)
			result.enabled.Store(ev.Enabled)
			if err := proto.WriteMsg(watchClient, proto.CacheAck{Seq: ev.Seq}); err != nil {
				return
			}
			select {
			case result.updated <- struct{}{}:
			default:
			}
		}
	}()
	t.Cleanup(func() {
		watchClient.Close()
		h.Close()
		clientSide.Close()
		proxyFront.Close()
		proxyUpstream.Close()
		serverSide.Close()
		for _, done := range []chan struct{}{proxyDone, serverDone, hubDone, watchDone} {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("cache integration goroutine did not stop")
			}
		}
	})
	result.client, err = sftp.NewClientPipe(clientSide, clientSide)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { result.client.Close() })
	result.wait(t, func() bool { return result.enabled.Load() })
	return result
}

func (c *integratedCache) wait(t *testing.T, condition func() bool) {
	t.Helper()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	for !condition() {
		select {
		case <-c.updated:
		case <-timeout.C:
			t.Fatalf("cache notification not applied (enabled=%v sequence=%d)", c.enabled.Load(), c.seq.Load())
		}
	}
}

func (c *integratedCache) read(t *testing.T, path string) string {
	t.Helper()
	f, err := c.client.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Like SSHFS, identify the file type before reading. Untyped streams and
	// devices must remain uncached even when they return a short read or EOF.
	if _, err := f.Stat(); err != nil {
		f.Close()
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(f)
	closeErr := f.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read %s: %v, close: %v", path, readErr, closeErr)
	}
	return string(data)
}

func (c *integratedCache) warm(t *testing.T, path, want string) int64 {
	t.Helper()
	if got := c.read(t, path); got != want {
		t.Fatalf("cold read: got %q, want %q", got, want)
	}
	before := c.wire.written.Load()
	if got := c.read(t, path); got != want {
		t.Fatalf("warm read: got %q, want %q", got, want)
	}
	if after := c.wire.written.Load(); after != before {
		t.Fatalf("warm reopen still sent upstream SFTP bytes: %d -> %d", before, after)
	}
	return before
}

func TestCachedReadRefreshesAfterExternalWrite(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "source")
	if err := os.WriteFile(p, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	c := newIntegratedCache(t, root)
	before := c.warm(t, p, "old")
	seq := c.seq.Load()
	if err := os.WriteFile(p, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	c.wait(t, func() bool { return c.seq.Load() > seq })
	if got := c.read(t, p); got != "new" {
		t.Fatalf("stale content after external write notification: %q", got)
	}
	if c.wire.written.Load() <= before {
		t.Fatal("invalidated read did not consult the upstream file")
	}
}

func TestCachedReadRefreshesAfterAtomicReplaceAndRename(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "source")
	if err := os.WriteFile(p, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	c := newIntegratedCache(t, root)
	c.warm(t, p, "old")
	replacement := filepath.Join(t.TempDir(), "replacement")
	if err := os.WriteFile(replacement, []byte("new inode with longer content"), 0600); err != nil {
		t.Fatal(err)
	}
	seq := c.seq.Load()
	if err := os.Rename(replacement, p); err != nil {
		t.Fatal(err)
	}
	c.wait(t, func() bool { return c.seq.Load() > seq })
	if got := c.read(t, p); got != "new inode with longer content" {
		t.Fatalf("atomic replace retained cached inode: %q", got)
	}
	if _, err := c.client.Stat(p); err != nil {
		t.Fatal(err)
	}
	seq = c.seq.Load()
	renamed := filepath.Join(root, "renamed")
	if err := os.Rename(p, renamed); err != nil {
		t.Fatal(err)
	}
	c.wait(t, func() bool { return c.seq.Load() > seq })
	if _, err := c.client.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("old path retained cached metadata after rename: %v", err)
	}
	if got := c.read(t, renamed); got != "new inode with longer content" {
		t.Fatalf("renamed content: %q", got)
	}
}

func TestSFTPWriteThroughRefreshesCacheAndLocalFile(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "source")
	if err := os.WriteFile(p, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	c := newIntegratedCache(t, root)
	c.warm(t, p, "old")
	seq := c.seq.Load()
	f, err := c.client.OpenFile(p, os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("remote edit")); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(p); err != nil || string(data) != "remote edit" {
		t.Fatalf("write did not reach local source: %q, %v", data, err)
	}
	if got := c.read(t, p); got != "remote edit" {
		t.Fatalf("read after successful remote write: %q", got)
	}
	c.wait(t, func() bool { return c.seq.Load() > seq })
}

func TestCommandCompletionRefreshesRealSFTPCache(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "source")
	if err := os.WriteFile(p, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	c := newIntegratedCache(t, root)
	c.warm(t, p, "old")
	seq := c.seq.Load()
	command, caller := net.Pipe()
	defer caller.Close()
	caller.SetDeadline(time.Now().Add(5 * time.Second))
	go runExec(command, &proto.ExecReq{Path: "/bin/sh", Argv: []string{"sh", "-c", `printf updated > "$1"`, "sh", p}, Cwd: root}, nil, c.hub.InvalidateAndWait)
	for {
		kind, data, err := proto.ReadFrame(caller)
		if err != nil {
			t.Fatal(err)
		}
		if kind != proto.FExit {
			continue
		}
		var exit proto.Exit
		if err := proto.DecodeJSON(data, &exit); err != nil || exit.Code != 0 {
			t.Fatalf("command exit: %+v, %v", exit, err)
		}
		break
	}
	if c.seq.Load() <= seq {
		t.Fatal("command exited before a new generation was applied")
	}
	if got := c.read(t, p); got != "updated" {
		t.Fatalf("command completed but SFTP read returned stale cache: %q", got)
	}
}

func TestCachedReadResumesOnlyAfterWatcherRecovery(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "source")
	if err := os.WriteFile(p, []byte("old cached inode"), 0600); err != nil {
		t.Fatal(err)
	}
	c := newIntegratedCache(t, root)
	c.warm(t, p, "old cached inode")
	initialSeq := c.seq.Load()
	// Moving the watched root loses coverage. Recreating the same pathname
	// must not resurrect the old generation or its retained SFTP handles.
	if err := os.Rename(root, filepath.Join(parent, "old-project")); err != nil {
		t.Fatal(err)
	}
	c.wait(t, func() bool { return !c.enabled.Load() && c.seq.Load() > initialSeq })
	disabledSeq := c.seq.Load()
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("new inode created during monitoring gap"), 0600); err != nil {
		t.Fatal(err)
	}
	c.wait(t, func() bool { return c.enabled.Load() && c.seq.Load() > disabledSeq })
	c.warm(t, p, "new inode created during monitoring gap")
}
