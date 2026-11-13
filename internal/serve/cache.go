package serve

import (
	"errors"
	"io"
	"log"
	"sync"
	"time"

	"waldo/internal/fswatch"
	"waldo/internal/proto"
)

// Keep the subscription alive through the normal 90-second reconnect window.
// Cache hits expire independently after a few seconds without heartbeats.
const cacheAckTimeout = 2 * time.Minute

const cacheBarrierTimeout = 3 * time.Second

// cacheHub owns the source of truth for a sandbox's cache notifications. The
// first implementation invalidates the whole project: coalescing events is
// safe, and a missed rename pair cannot leave an old subtree cached.
type cacheHub struct {
	mu        sync.Mutex
	seq       uint64
	enabled   bool
	clients   map[*cacheSubscriber]struct{}
	changed   chan struct{}
	done      chan struct{}
	ready     chan struct{}
	stopped   chan struct{}
	watch     projectWatcher
	readyOnce sync.Once
	once      sync.Once
}

type projectWatcher interface {
	Events() <-chan fswatch.Event
	Close() error
}

type projectWatcherFactory func(string) (projectWatcher, error)

type cacheSubscriber struct {
	wake      chan struct{}
	done      chan struct{}
	closing   chan struct{}
	stream    io.ReadWriteCloser
	closeOnce sync.Once
	ack       uint64 // protected by hub.mu
	sent      uint64
	ackAt     time.Time
}

func (c *cacheSubscriber) closeAsync() {
	c.closeOnce.Do(func() {
		close(c.closing)
		go func() {
			// A yamux close can wait on a congested or reconnecting transport.
			// Neither a command barrier nor the hub mutex may wait for it.
			if st, ok := c.stream.(interface{ SetDeadline(time.Time) error }); ok {
				st.SetDeadline(time.Now())
			}
			c.stream.Close()
		}()
	})
}

func newCacheHub(root string) *cacheHub {
	return newCacheHubWithWatcher(root, func(root string) (projectWatcher, error) {
		w, err := fswatch.New(root)
		if err != nil {
			return nil, err
		}
		return w, nil
	}, cacheWatchRetryDelay)
}

func cacheWatchRetryDelay(attempt int) time.Duration {
	if attempt >= 5 {
		return 30 * time.Second
	}
	return time.Second << attempt
}

func newCacheHubWithWatcher(root string, factory projectWatcherFactory, retryDelay func(int) time.Duration) *cacheHub {
	h := &cacheHub{seq: 1, clients: make(map[*cacheSubscriber]struct{}), changed: make(chan struct{}), done: make(chan struct{}), ready: make(chan struct{}), stopped: make(chan struct{})}
	// Large dependency trees must not delay the sandbox's startup handshake.
	// Requests pass through until initial monitoring coverage is complete.
	go h.watchProject(root, factory, retryDelay)
	return h
}

func (h *cacheHub) watchProject(root string, factory projectWatcherFactory, retryDelay func(int) time.Duration) {
	defer close(h.stopped)
	defer h.readyOnce.Do(func() { close(h.ready) })
	attempt := 0
	unavailable := false
	for {
		select {
		case <-h.done:
			return
		default:
		}
		w, err := factory(root)
		if err == nil && w == nil {
			err = errors.New("project watcher returned no coverage")
		}
		if err == nil {
			h.mu.Lock()
			select {
			case <-h.done:
				h.mu.Unlock()
				w.Close()
				return
			default:
			}
			// Publish coverage and the new generation together. Re-enabling with
			// an old sequence could retain snapshots from the monitoring gap.
			h.watch, h.enabled = w, true
			h.invalidateLocked(false)
			h.mu.Unlock()
			h.readyOnce.Do(func() { close(h.ready) })
			if unavailable {
				log.Printf("file cache monitoring restored")
			}
			unavailable, attempt = false, 0
			err = h.monitorProject(w)
		}
		h.mu.Lock()
		h.watch = nil
		select {
		case <-h.done:
			h.mu.Unlock()
			if w != nil {
				w.Close()
			}
			return
		default:
		}
		if !unavailable {
			h.invalidateLocked(true)
		}
		h.mu.Unlock()
		// Stop the previous instance before attempting another full scan. Never
		// run multiple watcher installations or leave stale descriptors alive.
		if w != nil {
			w.Close()
		}
		if !unavailable {
			log.Printf("file cache disabled: project watcher: %v; will retry", err)
		}
		unavailable = true
		h.readyOnce.Do(func() { close(h.ready) })
		timer := time.NewTimer(retryDelay(attempt))
		select {
		case <-h.done:
			timer.Stop()
			return
		case <-timer.C:
		}
		if attempt < 5 {
			attempt++
		}
	}
}

func (h *cacheHub) monitorProject(w projectWatcher) error {
	for {
		select {
		case <-h.done:
			return nil
		case ev, ok := <-w.Events():
			if !ok {
				return errors.New("project watcher stopped")
			}
			if ev.Err != nil {
				return ev.Err
			}
			h.invalidate(false)
		}
	}
}

func (h *cacheHub) changedLocked() {
	close(h.changed)
	h.changed = make(chan struct{})
}

func (h *cacheHub) invalidate(disable bool) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.invalidateLocked(disable)
}

func (h *cacheHub) invalidateLocked(disable bool) uint64 {
	h.seq++
	if disable {
		h.enabled = false
	}
	for c := range h.clients {
		select {
		case c.wake <- struct{}{}:
		default:
		}
	}
	h.changedLocked()
	return h.seq
}

func (h *cacheHub) acknowledge(c *cacheSubscriber, seq uint64, at time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if seq > c.sent {
		return false
	}
	if seq > c.ack {
		// Advancing ACKs prove the subscriber is making progress even when
		// continuous writes mean it never catches the latest sent generation.
		c.ack, c.ackAt = seq, at
		h.changedLocked()
	} else if seq == c.sent {
		// An idle subscriber may repeatedly acknowledge the same heartbeat.
		c.ackAt = at
	}
	return true
}

// InvalidateAndWait is deliberately conservative, independent of watcher
// delivery timing. The subscriber must apply a new generation before FExit.
// B renews its lease when a heartbeat arrives, not when A sends it: waiting
// one lease duration on A cannot prove that queued heartbeats have expired.
// Closing a notification stream alone is therefore insufficient to release
// FExit safely. The bounded fallback relies on B's frame relay synchronously
// invalidating every cache before delivering FExit to the stub. The exact-build
// handshake ensures both peers implement that contract.
func (h *cacheHub) InvalidateAndWait() {
	h.invalidateAndWait(cacheBarrierTimeout)
}

func (h *cacheHub) invalidateAndWait(timeout time.Duration) {
	seq := h.invalidate(false)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	h.mu.Lock()
	clients := make([]*cacheSubscriber, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	for {
		pending := false
		for _, c := range clients {
			if _, active := h.clients[c]; active && c.ack < seq {
				pending = true
				break
			}
		}
		if !pending {
			h.mu.Unlock()
			return
		}
		changed := h.changed
		h.mu.Unlock()
		select {
		case <-changed:
		case <-h.done:
			return
		case <-timer.C:
			h.mu.Lock()
			var stalled []*cacheSubscriber
			for _, c := range clients {
				if _, active := h.clients[c]; active && c.ack < seq {
					delete(h.clients, c)
					stalled = append(stalled, c)
				}
			}
			if len(stalled) > 0 {
				h.changedLocked()
			}
			h.mu.Unlock()
			for _, c := range stalled {
				c.closeAsync()
			}
			return
		}
		h.mu.Lock()
	}
}

func (h *cacheHub) Serve(st io.ReadWriteCloser) {
	c := &cacheSubscriber{wake: make(chan struct{}, 1), done: make(chan struct{}), closing: make(chan struct{}), stream: st, ackAt: time.Now()}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	defer func() {
		c.closeAsync()
		h.mu.Lock()
		delete(h.clients, c)
		h.changedLocked()
		h.mu.Unlock()
	}()
	go func() {
		defer close(c.done)
		defer c.closeAsync()
		for {
			var ack proto.CacheAck
			if err := proto.ReadMsg(st, &ack); err != nil {
				return
			}
			if !h.acknowledge(c, ack.Seq, time.Now()) {
				return
			}
		}
	}()
	go func() {
		// A broken subscriber must not hold every later command's exit status
		// forever. Closing this stream makes B disable its cache; during a
		// transport outage B also loses its short cache lease independently.
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-h.done:
				c.closeAsync()
				return
			case <-c.done:
				return
			case <-c.closing:
				return
			case <-tick.C:
				h.mu.Lock()
				stale := time.Since(c.ackAt) > cacheAckTimeout
				h.mu.Unlock()
				if stale {
					c.closeAsync()
					return
				}
			}
		}
	}()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		h.mu.Lock()
		ev := proto.CacheEvent{Seq: h.seq, Enabled: h.enabled}
		c.sent = ev.Seq
		h.mu.Unlock()
		if err := proto.WriteMsg(st, ev); err != nil {
			return
		}
		select {
		case <-c.wake:
		case <-tick.C:
		case <-c.done:
			return
		case <-c.closing:
			return
		case <-h.done:
			return
		}
	}
}

func (h *cacheHub) Close() {
	h.once.Do(func() {
		close(h.done)
		h.readyOnce.Do(func() { close(h.ready) })
		h.mu.Lock()
		watch := h.watch
		h.watch = nil
		h.invalidateLocked(true)
		h.mu.Unlock()
		if watch != nil {
			watch.Close()
		}
	})
}
