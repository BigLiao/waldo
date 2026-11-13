package serve

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"waldo/internal/fswatch"
	"waldo/internal/proto"
)

func cacheTestPeer(t *testing.T, h *cacheHub) net.Conn {
	t.Helper()
	a, b := net.Pipe()
	b.SetDeadline(time.Now().Add(5 * time.Second))
	go h.Serve(a)
	t.Cleanup(func() { b.Close(); h.Close() })
	return b
}

func cacheTestEvent(t *testing.T, c net.Conn) proto.CacheEvent {
	t.Helper()
	var ev proto.CacheEvent
	if err := proto.ReadMsg(c, &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

func cacheTestAck(t *testing.T, c net.Conn, seq uint64) {
	t.Helper()
	if err := proto.WriteMsg(c, proto.CacheAck{Seq: seq}); err != nil {
		t.Fatal(err)
	}
}

func TestCommandExitWaitsForCacheInvalidation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "source")
	if err := os.WriteFile(p, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	// No watcher here: the command barrier must work even when the watcher
	// hasn't yet delivered any of the command's filesystem events.
	h := newCacheHub(filepath.Join(dir, "missing-root"))
	<-h.ready
	watch := cacheTestPeer(t, h)
	initial := cacheTestEvent(t, watch)
	cacheTestAck(t, watch, initial.Seq)
	a, b := net.Pipe()
	defer b.Close()
	go runExec(a, &proto.ExecReq{Path: "/bin/sh", Argv: []string{"sh", "-c", `printf new > "$1"`, "sh", p}, Cwd: dir}, nil, h.InvalidateAndWait)
	exit := make(chan proto.Exit, 1)
	errs := make(chan error, 1)
	go func() {
		for {
			typ, body, err := proto.ReadFrame(b)
			if err != nil {
				errs <- err
				return
			}
			if typ == proto.FExit {
				var ex proto.Exit
				if err := proto.DecodeJSON(body, &ex); err != nil {
					errs <- err
					return
				}
				exit <- ex
				return
			}
		}
	}()
	ev := cacheTestEvent(t, watch)
	if ev.Seq <= initial.Seq {
		t.Fatalf("missing command invalidation: %+v", ev)
	}
	if data, err := os.ReadFile(p); err != nil || string(data) != "new" {
		t.Fatalf("command write: %q %v", data, err)
	}
	select {
	case ex := <-exit:
		t.Fatalf("exit before cache acknowledgement: %+v", ex)
	case err := <-errs:
		t.Fatal(err)
	case <-time.After(30 * time.Millisecond):
	}
	cacheTestAck(t, watch, ev.Seq)
	select {
	case ex := <-exit:
		if ex.Code != 0 {
			t.Fatalf("exit: %+v", ex)
		}
	case err := <-errs:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("exit still blocked after acknowledgement")
	}
}

func TestExternalWritePublishesInvalidation(t *testing.T) {
	dir := t.TempDir()
	h := newCacheHub(dir)
	<-h.ready
	c := cacheTestPeer(t, h)
	initial := cacheTestEvent(t, c)
	if !initial.Enabled {
		t.Skip("filesystem monitoring unavailable")
	}
	cacheTestAck(t, c, initial.Seq)
	if err := os.WriteFile(filepath.Join(dir, "external.tmp"), []byte("saved"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "external.tmp"), filepath.Join(dir, "external")); err != nil {
		t.Fatal(err)
	}
	for {
		ev := cacheTestEvent(t, c)
		cacheTestAck(t, c, ev.Seq)
		if ev.Seq > initial.Seq {
			return
		}
	}
}

func TestCacheBarrierReleasedWhenSubscriberDisconnects(t *testing.T) {
	h := newCacheHub(filepath.Join(t.TempDir(), "missing"))
	<-h.ready
	c := cacheTestPeer(t, h)
	initial := cacheTestEvent(t, c)
	cacheTestAck(t, c, initial.Seq)
	done := make(chan struct{})
	go func() { h.InvalidateAndWait(); close(done) }()
	cacheTestEvent(t, c)
	c.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("disconnected subscriber kept command blocked")
	}
}

func TestUnacknowledgedSubscriberCannotBlockCommandForever(t *testing.T) {
	h := newCacheHub(filepath.Join(t.TempDir(), "missing"))
	<-h.ready
	c := cacheTestPeer(t, h)
	initial := cacheTestEvent(t, c)
	cacheTestAck(t, c, initial.Seq)
	cacheTestWaitHub(t, h, func() bool {
		for subscriber := range h.clients {
			return subscriber.ack == initial.Seq
		}
		return false
	})
	done := make(chan struct{})
	go func() { h.InvalidateAndWait(); close(done) }()
	cacheTestEvent(t, c)
	// Simulate an expired acknowledgement deadline without making the suite
	// wait for the reconnect grace period. No more ACKs or reads arrive.
	h.mu.Lock()
	for subscriber := range h.clients {
		subscriber.ackAt = time.Now().Add(-time.Hour)
	}
	h.mu.Unlock()
	// Repeated ACKs behind the latest sent invalidation make no progress.
	cacheTestAck(t, c, initial.Seq)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("unresponsive cache subscriber kept command blocked")
	}
}

// condition runs while hub.mu is held, then waits on the hub's own changes.
func cacheTestWaitHub(t *testing.T, h *cacheHub, condition func() bool) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		h.mu.Lock()
		if condition() {
			h.mu.Unlock()
			return
		}
		changed := h.changed
		h.mu.Unlock()
		select {
		case <-changed:
		case <-timer.C:
			t.Fatal("cache hub did not reach the expected state")
		}
	}
}

func TestAdvancingCacheAcknowledgementsDoNotStarve(t *testing.T) {
	h := &cacheHub{changed: make(chan struct{})}
	base := time.Unix(100, 0)
	c := &cacheSubscriber{ack: 10, sent: 20, ackAt: base}
	// Keep the sender ahead at all times, as continuous filesystem changes do.
	// Progress must renew liveness even though no ACK equals sent.
	for seq := uint64(11); seq < 20; seq++ {
		at := base.Add(time.Duration(seq-10) * time.Minute)
		c.sent = seq + 10
		if !h.acknowledge(c, seq, at) || c.ack != seq || c.ackAt != at {
			t.Fatalf("advancing but lagging ACK %d did not renew liveness: %+v", seq, c)
		}
		if c.ack >= c.sent {
			t.Fatal("test no longer models a continuously lagging subscriber")
		}
		// Replaying the same lagging sequence is not evidence of more work.
		if !h.acknowledge(c, seq, at.Add(time.Minute)) || c.ackAt != at {
			t.Fatalf("repeated lagging ACK %d renewed liveness", seq)
		}
	}
	at := base.Add(time.Hour)
	if !h.acknowledge(c, c.sent, at) || c.ackAt != at {
		t.Fatal("catching up failed to renew liveness")
	}
	if !h.acknowledge(c, c.sent, at.Add(time.Minute)) || c.ackAt != at.Add(time.Minute) {
		t.Fatal("same-generation idle heartbeat failed to renew liveness")
	}
	beforeAck, beforeTime := c.ack, c.ackAt
	if h.acknowledge(c, c.sent+1, at.Add(2*time.Minute)) || c.ack != beforeAck || c.ackAt != beforeTime {
		t.Fatal("acknowledgement of an unsent sequence was accepted")
	}
}

type fakeProjectWatcher struct {
	events    chan fswatch.Event
	closed    chan struct{}
	closeOnce sync.Once
	eventOnce sync.Once
}

func newFakeProjectWatcher() *fakeProjectWatcher {
	return &fakeProjectWatcher{events: make(chan fswatch.Event, 2), closed: make(chan struct{})}
}

func (w *fakeProjectWatcher) Events() <-chan fswatch.Event { return w.events }
func (w *fakeProjectWatcher) Close() error {
	w.closeOnce.Do(func() {
		w.eventOnce.Do(func() { close(w.events) })
		close(w.closed)
	})
	return nil
}

func cacheTestStopped(t *testing.T, h *cacheHub) {
	t.Helper()
	select {
	case <-h.stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("cache watcher supervisor did not stop")
	}
}

func TestWatcherRecoversInitialFailureAfterFullCoverage(t *testing.T) {
	w := newFakeProjectWatcher()
	started := make(chan struct{})
	finishScan := make(chan struct{})
	var finishOnce sync.Once
	releaseScan := func() { finishOnce.Do(func() { close(finishScan) }) }
	var attempts atomic.Int32
	h := newCacheHubWithWatcher("project", func(string) (projectWatcher, error) {
		if attempts.Add(1) == 1 {
			return nil, os.ErrPermission
		}
		close(started)
		<-finishScan
		return w, nil
	}, func(int) time.Duration { return 0 })
	t.Cleanup(func() { h.Close(); releaseScan(); cacheTestStopped(t, h) })
	<-h.ready
	<-started
	h.mu.Lock()
	disabledSeq := h.seq
	enabled := h.enabled
	h.mu.Unlock()
	if enabled {
		t.Fatal("failed or incomplete watcher installation enabled caching")
	}
	releaseScan()
	cacheTestWaitHub(t, h, func() bool { return h.enabled })
	h.mu.Lock()
	if h.seq <= disabledSeq || h.watch != w {
		t.Error("recovery did not replace the monitoring-gap generation")
	}
	h.mu.Unlock()
	if attempts.Load() != 2 {
		t.Fatalf("unexpected watcher attempts: %d", attempts.Load())
	}
}

func TestWatcherFaultDisablesBeforeRebuildingCoverage(t *testing.T) {
	for _, closed := range []bool{false, true} {
		name := "error"
		if closed {
			name = "closed-stream"
		}
		t.Run(name, func(t *testing.T) {
			first, replacement := newFakeProjectWatcher(), newFakeProjectWatcher()
			rebuilding := make(chan struct{})
			finishScan := make(chan struct{})
			var finishOnce sync.Once
			releaseScan := func() { finishOnce.Do(func() { close(finishScan) }) }
			var attempts atomic.Int32
			h := newCacheHubWithWatcher("project", func(string) (projectWatcher, error) {
				if attempts.Add(1) == 1 {
					return first, nil
				}
				close(rebuilding)
				<-finishScan
				return replacement, nil
			}, func(int) time.Duration { return 0 })
			t.Cleanup(func() { h.Close(); releaseScan(); cacheTestStopped(t, h) })
			<-h.ready
			h.mu.Lock()
			initialSeq := h.seq
			h.mu.Unlock()
			if closed {
				first.eventOnce.Do(func() { close(first.events) })
			} else {
				first.events <- fswatch.Event{Err: errors.New("simulated notification overflow")}
			}
			<-rebuilding
			select {
			case <-first.closed:
			default:
				t.Fatal("new watcher started before old watcher descriptors were closed")
			}
			h.mu.Lock()
			disabledSeq, enabled := h.seq, h.enabled
			h.mu.Unlock()
			if enabled || disabledSeq <= initialSeq {
				t.Fatal("watcher failure did not invalidate and disable caching")
			}
			releaseScan()
			cacheTestWaitHub(t, h, func() bool { return h.enabled && h.seq > disabledSeq })
			h.mu.Lock()
			restoredSeq := h.seq
			h.mu.Unlock()
			replacement.events <- fswatch.Event{}
			cacheTestWaitHub(t, h, func() bool { return h.seq > restoredSeq })
		})
	}
}

func TestWatcherRetryBackoffIsBoundedAndCloseCancelsIt(t *testing.T) {
	for attempt, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second} {
		if got := cacheWatchRetryDelay(attempt); got != want {
			t.Fatalf("retry %d: got %s, want %s", attempt, got, want)
		}
	}
	backoff := make(chan struct{}, 1)
	var attempts atomic.Int32
	h := newCacheHubWithWatcher("project", func(string) (projectWatcher, error) {
		attempts.Add(1)
		return nil, os.ErrPermission
	}, func(int) time.Duration {
		backoff <- struct{}{}
		return time.Hour
	})
	<-backoff
	h.Close()
	cacheTestStopped(t, h)
	if attempts.Load() != 1 {
		t.Fatalf("canceled retry installed another watcher: %d attempts", attempts.Load())
	}
	h.mu.Lock()
	enabled := h.enabled
	h.mu.Unlock()
	if enabled {
		t.Fatal("unavailable watcher enabled caching")
	}
}

func TestCloseDisposesWatcherThatFinishesInstallingLate(t *testing.T) {
	w := newFakeProjectWatcher()
	started, finishScan := make(chan struct{}), make(chan struct{})
	var finishOnce sync.Once
	releaseScan := func() { finishOnce.Do(func() { close(finishScan) }) }
	h := newCacheHubWithWatcher("project", func(string) (projectWatcher, error) {
		close(started)
		<-finishScan
		return w, nil
	}, cacheWatchRetryDelay)
	t.Cleanup(func() { h.Close(); releaseScan(); cacheTestStopped(t, h) })
	<-started
	closed := make(chan struct{})
	go func() { h.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close waited on an incomplete watcher scan")
	}
	releaseScan()
	cacheTestStopped(t, h)
	select {
	case <-w.closed:
	default:
		t.Fatal("late watcher installation leaked its descriptors")
	}
	h.mu.Lock()
	enabled, watch := h.enabled, h.watch
	h.mu.Unlock()
	if enabled || watch != nil {
		t.Fatal("late watcher installation resurrected a closed cache hub")
	}
}

func TestCacheBarrierTimeoutDisconnectsUnresponsiveSubscription(t *testing.T) {
	h := newCacheHub(filepath.Join(t.TempDir(), "missing"))
	<-h.ready
	c := cacheTestPeer(t, h)
	initial := cacheTestEvent(t, c)
	cacheTestAck(t, c, initial.Seq)
	done := make(chan struct{})
	go func() { h.invalidateAndWait(25 * time.Millisecond); close(done) }()
	ev := cacheTestEvent(t, c)
	if ev.Seq <= initial.Seq {
		t.Fatal("barrier did not send an invalidation")
	}
	// No acknowledgement follows. The ordinary two-minute liveness watchdog
	// cannot release this command; its own bounded barrier must do so.
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("command barrier ignored its timeout")
	}
	h.mu.Lock()
	remaining := len(h.clients)
	h.mu.Unlock()
	if remaining != 0 {
		t.Fatal("timed-out subscriber will stall the next command again")
	}
	c.SetReadDeadline(time.Now().Add(time.Second))
	for {
		var next proto.CacheEvent
		if err := proto.ReadMsg(c, &next); err != nil {
			var timeout net.Error
			if errors.As(err, &timeout) && timeout.Timeout() {
				t.Fatal("timed-out subscription remained connected")
			}
			break
		}
	}
}

type blockingSubscriberClose struct {
	started     chan struct{}
	release     chan struct{}
	deadlineSet atomic.Bool
}

func (*blockingSubscriberClose) Read([]byte) (int, error)    { return 0, io.EOF }
func (*blockingSubscriberClose) Write(p []byte) (int, error) { return len(p), nil }
func (s *blockingSubscriberClose) SetDeadline(time.Time) error {
	s.deadlineSet.Store(true)
	return nil
}
func (s *blockingSubscriberClose) Close() error {
	close(s.started)
	<-s.release
	return nil
}

func TestCacheBarrierDoesNotWaitForBlockedStreamClose(t *testing.T) {
	stream := &blockingSubscriberClose{started: make(chan struct{}), release: make(chan struct{})}
	defer close(stream.release)
	c := &cacheSubscriber{wake: make(chan struct{}, 1), closing: make(chan struct{}), stream: stream}
	h := &cacheHub{seq: 1, clients: map[*cacheSubscriber]struct{}{c: {}}, changed: make(chan struct{}), done: make(chan struct{})}
	done := make(chan struct{})
	go func() { h.invalidateAndWait(time.Millisecond); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("blocking stream Close held up command completion")
	}
	select {
	case <-stream.started:
	case <-time.After(time.Second):
		t.Fatal("stalled subscriber was never closed")
	}
	if !stream.deadlineSet.Load() {
		t.Fatal("stream close did not interrupt pending reads/writes with a deadline")
	}
	h.mu.Lock()
	remaining := len(h.clients)
	h.mu.Unlock()
	if remaining != 0 {
		t.Fatal("stream Close held up unsubscription")
	}
}
