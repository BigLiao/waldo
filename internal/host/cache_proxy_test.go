package host

import (
	"errors"
	"io"
	"net"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"waldo/internal/proto"
)

func TestCachePipeDoesNotHoldProcessWaitOpen(t *testing.T) {
	stdin, stdout, proxy, err := cacheProcessPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	defer stdout.Close()
	defer proxy.Close()
	cmd := exec.Command("sh", "-c", "printf ready")
	cmd.Stdin, cmd.Stdout = stdin, stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stdin.Close()
	stdout.Close()
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		cmd.Process.Kill()
		t.Fatal("process Wait depends on closing the open SFTP proxy")
	}
	data, err := io.ReadAll(proxy)
	if err != nil || string(data) != "ready" {
		t.Fatalf("process output: %q, %v", data, err)
	}
}

type recordingCache struct {
	mu            sync.Mutex
	enabled       bool
	everEnabled   bool
	invalidations int
	changed       chan struct{}
}

func newRecordingCache() *recordingCache {
	return &recordingCache{changed: make(chan struct{}, 32)}
}

func (c *recordingCache) Invalidate() {
	c.mu.Lock()
	c.invalidations++
	c.mu.Unlock()
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

func (c *recordingCache) SetEnabled(enabled bool) {
	c.mu.Lock()
	c.enabled = enabled
	c.everEnabled = c.everEnabled || enabled
	c.mu.Unlock()
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

func (c *recordingCache) state() (bool, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enabled, c.invalidations
}

func watchForTest(t *testing.T, initial proto.CacheEvent) (*cacheWatch, net.Conn, *recordingCache) {
	t.Helper()
	client, server := net.Pipe()
	client.SetDeadline(time.Now().Add(5 * time.Second))
	server.SetDeadline(time.Now().Add(5 * time.Second))
	cache := newRecordingCache()
	watch := (&host{}).startCacheWatch(cache, "/project", singleWatchStream(client))
	t.Cleanup(func() { watch.Close(); server.Close(); waitCacheWatchStopped(t, watch) })
	var header proto.Header
	if err := proto.ReadMsg(server, &header); err != nil || header.Kind != proto.KindFSWatch || header.Path != "/project" {
		t.Fatalf("cache watch request: %+v, %v", header, err)
	}
	if err := proto.WriteMsg(server, initial); err != nil {
		t.Fatal(err)
	}
	var ack proto.CacheAck
	if err := proto.ReadMsg(server, &ack); err != nil || ack.Seq != initial.Seq {
		t.Fatalf("initial ack: %+v, %v", ack, err)
	}
	return watch, server, cache
}

func singleWatchStream(conn io.ReadWriteCloser) func() (io.ReadWriteCloser, error) {
	var used atomic.Bool
	return func() (io.ReadWriteCloser, error) {
		if used.Swap(true) {
			return nil, io.ErrClosedPipe
		}
		return conn, nil
	}
}

func waitCacheWatchStopped(t *testing.T, watch *cacheWatch) {
	t.Helper()
	select {
	case <-watch.stopped:
	case <-time.After(2 * time.Second):
		t.Error("cache subscription did not stop after Close")
	}
}

func awaitCacheEnabled(t *testing.T, cache *recordingCache, want bool) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		if enabled, _ := cache.state(); enabled == want {
			return
		}
		select {
		case <-cache.changed:
		case <-timer.C:
			t.Fatalf("cache enabled did not become %t", want)
		}
	}
}

func sendCacheEvent(t *testing.T, server net.Conn, event proto.CacheEvent) {
	t.Helper()
	if err := proto.WriteMsg(server, event); err != nil {
		t.Fatal(err)
	}
	var ack proto.CacheAck
	if err := proto.ReadMsg(server, &ack); err != nil || ack.Seq != event.Seq {
		t.Fatalf("ack: %+v, %v", ack, err)
	}
}

func TestCacheWatchAppliesInvalidationBeforeAck(t *testing.T) {
	_, server, cache := watchForTest(t, proto.CacheEvent{Seq: 7, Enabled: true})
	_, initialInvalidations := cache.state()
	if enabled, n := cache.state(); !enabled || n < 1 {
		t.Fatalf("initial cache state: enabled=%t invalidations=%d", enabled, n)
	}
	// A heartbeat renews the lease without invalidating a warmed cache.
	sendCacheEvent(t, server, proto.CacheEvent{Seq: 7, Enabled: true})
	if enabled, n := cache.state(); !enabled || n != initialInvalidations {
		t.Fatalf("heartbeat cache state: enabled=%t invalidations=%d", enabled, n)
	}
	sendCacheEvent(t, server, proto.CacheEvent{Seq: 8, Enabled: true})
	if enabled, n := cache.state(); !enabled || n != initialInvalidations+1 {
		t.Fatalf("invalidated cache state before ack: enabled=%t invalidations=%d", enabled, n)
	}
	sendCacheEvent(t, server, proto.CacheEvent{Seq: 8, Enabled: false})
	if enabled, _ := cache.state(); enabled {
		t.Fatal("watcher failure left cache enabled")
	}
}

func TestCacheReconnectWaitsForNextWatchEvent(t *testing.T) {
	watch, server, cache := watchForTest(t, proto.CacheEvent{Seq: 1, Enabled: true})
	h := &host{cacheWatches: []*cacheWatch{watch}}
	h.cacheConnectionState(false)
	if enabled, _ := cache.state(); enabled {
		t.Fatal("disconnected cache remains enabled")
	}
	// Even a notification already buffered below yamux cannot re-enable the
	// cache while the physical connection is known to be down.
	sendCacheEvent(t, server, proto.CacheEvent{Seq: 2, Enabled: true})
	if enabled, _ := cache.state(); enabled {
		t.Fatal("offline notification enabled cache")
	}
	h.cacheConnectionState(true)
	if enabled, _ := cache.state(); enabled {
		t.Fatal("reconnect enabled cache without a watch event")
	}
	sendCacheEvent(t, server, proto.CacheEvent{Seq: 2, Enabled: true})
	if enabled, _ := cache.state(); !enabled {
		t.Fatal("next watch event failed to renew cache lease")
	}
}

func TestCacheWatchStartupTracksConnectionChanges(t *testing.T) {
	for _, tc := range []struct {
		name          string
		initiallyDown bool
		changes       []bool
		wantEnabled   bool
	}{
		{name: "disconnect during handshake", changes: []bool{false}},
		{name: "subscribe while disconnected", initiallyDown: true},
		{name: "reconnect during handshake", initiallyDown: true, changes: []bool{true}, wantEnabled: true},
		{name: "disconnect reconnect disconnect", changes: []bool{false, true, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := net.Pipe()
			client.SetDeadline(time.Now().Add(5 * time.Second))
			server.SetDeadline(time.Now().Add(5 * time.Second))
			t.Cleanup(func() { client.Close(); server.Close() })
			cache := newRecordingCache()
			h := &host{cacheLinkDown: tc.initiallyDown}
			watch := h.startCacheWatch(cache, "/project", singleWatchStream(client))
			t.Cleanup(func() { watch.Close(); waitCacheWatchStopped(t, watch) })
			var header proto.Header
			if err := proto.ReadMsg(server, &header); err != nil {
				t.Fatal(err)
			}
			// The first response remains blocked here. State callbacks must
			// already be able to reach this watch and must not wait on the
			// handshake's network I/O while holding the host lock.
			h.mu.Lock()
			registered := len(h.cacheWatches)
			h.mu.Unlock()
			if registered != 1 {
				t.Fatalf("watch not registered before handshake: %d", registered)
			}
			for _, up := range tc.changes {
				done := make(chan struct{})
				go func() {
					h.cacheConnectionState(up)
					close(done)
				}()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("connection callback blocked behind watch handshake")
				}
			}
			if enabled, _ := cache.state(); enabled {
				t.Fatal("cache enabled before first notification")
			}
			initial := proto.CacheEvent{Seq: 1, Enabled: true}
			sendCacheEvent(t, server, initial)
			if enabled, _ := cache.state(); enabled != tc.wantEnabled {
				t.Fatalf("cache enabled=%t after startup, want %t", enabled, tc.wantEnabled)
			}
			cache.mu.Lock()
			everEnabled := cache.everEnabled
			cache.mu.Unlock()
			if !tc.wantEnabled && everEnabled {
				t.Fatal("initial event transiently enabled cache while disconnected")
			}
			if !tc.wantEnabled {
				h.cacheConnectionState(true)
				if enabled, _ := cache.state(); enabled {
					t.Fatal("reconnect enabled cache without a subsequent notification")
				}
				sendCacheEvent(t, server, initial)
				if enabled, _ := cache.state(); !enabled {
					t.Fatal("notification after reconnect did not enable cache")
				}
			}
		})
	}
}

func TestCacheWatchFailureDisablesCache(t *testing.T) {
	_, server, cache := watchForTest(t, proto.CacheEvent{Seq: 5, Enabled: true})
	server.Close()
	awaitCacheEnabled(t, cache, false)
}

func TestCacheWatchRejectsSequenceRegression(t *testing.T) {
	_, server, cache := watchForTest(t, proto.CacheEvent{Seq: 5, Enabled: true})
	if err := proto.WriteMsg(server, proto.CacheEvent{Seq: 4, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	var ack proto.CacheAck
	if err := proto.ReadMsg(server, &ack); err == nil {
		t.Fatal("regressing notification was acknowledged")
	}
	if enabled, _ := cache.state(); enabled {
		t.Fatal("regressing notification left cache enabled")
	}
}

func TestCacheWatchHandshakeFailureDisablesCache(t *testing.T) {
	client, server := net.Pipe()
	server.Close()
	cache := newRecordingCache()
	cache.SetEnabled(true)
	watch := (&host{}).startCacheWatch(cache, "/project", singleWatchStream(client))
	t.Cleanup(func() { watch.Close(); waitCacheWatchStopped(t, watch) })
	if enabled, _ := cache.state(); enabled {
		t.Fatal("failed handshake left cache enabled")
	}
}

func TestCacheWatchRetriesInitialFailures(t *testing.T) {
	client, server := net.Pipe()
	server.SetDeadline(time.Now().Add(5 * time.Second))
	defer server.Close()
	var mu sync.Mutex
	var attempts []time.Time
	cache := newRecordingCache()
	h := &host{}
	watch := h.startCacheWatch(cache, "/project", func() (io.ReadWriteCloser, error) {
		mu.Lock()
		attempts = append(attempts, time.Now())
		n := len(attempts)
		mu.Unlock()
		if n < 3 {
			return nil, errors.New("temporary subscription refusal")
		}
		if n == 3 {
			return client, nil
		}
		return nil, io.ErrClosedPipe
	})
	t.Cleanup(func() { watch.Close(); waitCacheWatchStopped(t, watch) })
	if enabled, _ := cache.state(); enabled {
		t.Fatal("cache enabled before an initial successful subscription")
	}
	var header proto.Header
	if err := proto.ReadMsg(server, &header); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	times := append([]time.Time(nil), attempts...)
	mu.Unlock()
	if len(times) != 3 {
		t.Fatalf("opener attempts=%d, want 3", len(times))
	}
	for i := 1; i < len(times); i++ {
		minimum := cacheWatchRetryInitial * time.Duration(1<<(i-1))
		if gap := times[i].Sub(times[i-1]); gap < minimum {
			t.Errorf("retry %d waited %s, want at least %s", i, gap, minimum)
		}
	}
	h.mu.Lock()
	registered := len(h.cacheWatches)
	h.mu.Unlock()
	if registered != 1 {
		t.Fatalf("retries registered %d watch objects, want one", registered)
	}
	sendCacheEvent(t, server, proto.CacheEvent{Seq: 20, Enabled: true})
	awaitCacheEnabled(t, cache, true)
}

func TestCacheWatchResubscribesWithFreshSequence(t *testing.T) {
	client1, server1 := net.Pipe()
	client2, server2 := net.Pipe()
	server1.SetDeadline(time.Now().Add(5 * time.Second))
	server2.SetDeadline(time.Now().Add(5 * time.Second))
	defer server1.Close()
	defer server2.Close()
	var attempts atomic.Int32
	cache := newRecordingCache()
	h := &host{}
	watch := h.startCacheWatch(cache, "/project", func() (io.ReadWriteCloser, error) {
		switch attempts.Add(1) {
		case 1:
			return client1, nil
		case 2:
			return client2, nil
		default:
			return nil, io.ErrClosedPipe
		}
	})
	t.Cleanup(func() { watch.Close(); waitCacheWatchStopped(t, watch) })
	var header proto.Header
	if err := proto.ReadMsg(server1, &header); err != nil {
		t.Fatal(err)
	}
	sendCacheEvent(t, server1, proto.CacheEvent{Seq: 50, Enabled: true})
	watch.mu.Lock()
	oldStream := watch.stream
	watch.mu.Unlock()
	_, before := cache.state()
	server1.Close()
	awaitCacheEnabled(t, cache, false)
	if err := proto.ReadMsg(server2, &header); err != nil {
		t.Fatal(err)
	}
	if enabled, _ := cache.state(); enabled {
		t.Fatal("resubscription enabled cache before a fresh initial event")
	}
	// A lower initial sequence is valid on a newly established stream, and
	// that stream's startup must participate in physical connection state.
	h.cacheConnectionState(false)
	sendCacheEvent(t, server2, proto.CacheEvent{Seq: 1, Enabled: true})
	if enabled, _ := cache.state(); enabled {
		t.Fatal("new attempt enabled cache while physical connection was down")
	}
	h.cacheConnectionState(true)
	if enabled, _ := cache.state(); enabled {
		t.Fatal("reconnect enabled cache without another fresh event")
	}
	sendCacheEvent(t, server2, proto.CacheEvent{Seq: 1, Enabled: true})
	if enabled, n := cache.state(); !enabled || n <= before {
		t.Fatalf("resubscription did not clear and restore cache: enabled=%t invalidations=%d before=%d", enabled, n, before)
	}
	// A delayed heartbeat belonging to the previous attempt must neither
	// renew the shared cache nor disable the healthy replacement.
	_, current := cache.state()
	if err := watch.apply(oldStream, proto.CacheEvent{Seq: 51, Enabled: false}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("obsolete attempt was accepted: %v", err)
	}
	if enabled, n := cache.state(); !enabled || n != current {
		t.Fatalf("obsolete attempt changed replacement cache: enabled=%t invalidations=%d", enabled, n)
	}
	h.mu.Lock()
	registered := len(h.cacheWatches)
	h.mu.Unlock()
	if registered != 1 {
		t.Fatalf("resubscription registered %d watches, want one", registered)
	}
}

func TestCacheWatchCloseCancelsBackoff(t *testing.T) {
	firstAttempt := make(chan struct{})
	var attempts atomic.Int32
	watch := (&host{}).startCacheWatch(newRecordingCache(), "/project", func() (io.ReadWriteCloser, error) {
		if attempts.Add(1) == 1 {
			close(firstAttempt)
		}
		return nil, errors.New("unavailable")
	})
	select {
	case <-firstAttempt:
	case <-time.After(2 * time.Second):
		t.Fatal("initial subscription did not start")
	}
	watch.Close()
	waitCacheWatchStopped(t, watch)
	if n := attempts.Load(); n != 1 {
		t.Errorf("Close allowed extra retry attempts: %d", n)
	}
}

func TestCacheWatchCloseCancelsPendingOpen(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	server.SetDeadline(time.Now().Add(2 * time.Second))
	opening, release := make(chan struct{}), make(chan struct{})
	cache := newRecordingCache()
	watch := (&host{}).startCacheWatch(cache, "/project", func() (io.ReadWriteCloser, error) {
		close(opening)
		<-release
		return client, nil
	})
	<-opening
	watch.Close()
	waitCacheWatchStopped(t, watch)
	close(release)
	var header proto.Header
	if err := proto.ReadMsg(server, &header); err == nil {
		t.Fatal("a late opener started a subscription after Close")
	}
	if enabled, _ := cache.state(); enabled {
		t.Fatal("closed subscription left cache enabled")
	}
}

func TestCacheWatchCloseCancelsHandshake(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	server.SetDeadline(time.Now().Add(2 * time.Second))
	cache := newRecordingCache()
	watch := (&host{}).startCacheWatch(cache, "/project", singleWatchStream(client))
	var header proto.Header
	if err := proto.ReadMsg(server, &header); err != nil {
		t.Fatal(err)
	}
	watch.Close()
	waitCacheWatchStopped(t, watch)
	if err := proto.WriteMsg(server, proto.CacheEvent{Seq: 1, Enabled: true}); err == nil {
		t.Fatal("closed handshake accepted an initial event")
	}
	if enabled, _ := cache.state(); enabled {
		t.Fatal("handshake cancellation left cache enabled")
	}
}

func TestCacheWatchCloseCancelsBlockedAck(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	server.SetDeadline(time.Now().Add(2 * time.Second))
	cache := newRecordingCache()
	watch := (&host{}).startCacheWatch(cache, "/project", singleWatchStream(client))
	var header proto.Header
	if err := proto.ReadMsg(server, &header); err != nil {
		t.Fatal(err)
	}
	if err := proto.WriteMsg(server, proto.CacheEvent{Seq: 1, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// Do not read the acknowledgement: net.Pipe leaves that write blocked.
	awaitCacheEnabled(t, cache, true)
	watch.Close()
	waitCacheWatchStopped(t, watch)
	if enabled, _ := cache.state(); enabled {
		t.Fatal("blocked acknowledgement renewed the cache after Close")
	}
	var ack proto.CacheAck
	if err := proto.ReadMsg(server, &ack); err == nil {
		t.Fatal("closed subscription completed a blocked acknowledgement")
	}
}

func TestProxyMountHasNoIndependentKernelCache(t *testing.T) {
	options := sshfsProxyCacheOptions()
	want := map[string]bool{
		"dir_cache=no": true, "attr_timeout=0": true, "entry_timeout=0": true,
		"negative_timeout=0": true, "direct_io": true,
	}
	for _, option := range options {
		if !want[option] {
			t.Errorf("unexpected independent cache option %q", option)
		}
		delete(want, option)
	}
	if len(want) != 0 {
		t.Errorf("missing options that prevent stale kernel results: %v", want)
	}
}
