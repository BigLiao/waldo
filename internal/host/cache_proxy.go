package host

import (
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"waldo/internal/fscache"
	"waldo/internal/proto"
	"waldo/internal/util"
)

// Giving os/exec real file descriptors avoids stdin-copy goroutines that
// could keep Cmd.Wait blocked after sshfs exits while the SFTP link is open.
func cacheProcessPipe() (stdin, stdout *os.File, proxy io.ReadWriteCloser, err error) {
	stdin, toChild, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, err
	}
	fromChild, stdout, err := os.Pipe()
	if err != nil {
		stdin.Close()
		toChild.Close()
		return nil, nil, nil, err
	}
	proxy = util.RWC{R: fromChild, W: toChild, C: []io.Closer{fromChild, toChild}}
	return stdin, stdout, proxy, nil
}

type cacheControl interface {
	Invalidate()
	SetEnabled(bool)
}

const (
	cacheWatchRetryInitial = 100 * time.Millisecond
	cacheWatchRetryMax     = 5 * time.Second
)

// A stream is closed at most once. yamux's graceful Close can wait on a
// disconnected session's write timeout, so cancellation must not wait for it.
type cacheWatchStream struct {
	conn io.ReadWriteCloser
	once sync.Once
}

func (s *cacheWatchStream) close() {
	s.once.Do(func() {
		if d, ok := s.conn.(interface{ SetDeadline(time.Time) error }); ok {
			d.SetDeadline(time.Now())
		}
		go s.conn.Close()
	})
}

// cacheWatch supervises one subscription at a time. Replacing its stream
// resets the cache and sequence; failed attempts never register another watch.
type cacheWatch struct {
	mu        sync.Mutex
	cache     cacheControl
	root      string
	open      func() (io.ReadWriteCloser, error)
	stream    *cacheWatchStream
	seq       uint64
	hasSeq    bool
	connected bool
	closed    bool
	degraded  bool
	done      chan struct{}
	stopped   chan struct{}
}

func (h *host) startProjectCache(root string) (*fscache.Cache, *cacheWatch, error) {
	cache := fscache.New(root, time.Duration(h.hello.MetadataCacheTTLSeconds)*time.Second,
		time.Duration(h.hello.ContentCacheTTLSeconds)*time.Second)
	watch := h.startCacheWatch(cache, root, func() (io.ReadWriteCloser, error) {
		return h.sess.OpenStream()
	})
	// Mount immediately in disabled/pass-through mode. Notification startup
	// failures are retried in the background rather than fixing this session
	// permanently to the direct path.
	return cache, watch, nil
}

func (h *host) startCacheWatch(cache cacheControl, root string, open func() (io.ReadWriteCloser, error)) *cacheWatch {
	// Register before any handshake I/O can block. Reading a connection-state
	// snapshot and registering later would miss a disconnect in between.
	cache.SetEnabled(false)
	h.mu.Lock()
	w := &cacheWatch{cache: cache, root: root, open: open, connected: !h.cacheLinkDown,
		done: make(chan struct{}), stopped: make(chan struct{})}
	h.cacheWatches = append(h.cacheWatches, w)
	h.mu.Unlock()
	go w.run()
	return w
}

func (w *cacheWatch) run() {
	defer close(w.stopped)
	delay := cacheWatchRetryInitial
	for {
		select {
		case <-w.done:
			return
		default:
		}
		established, err := w.subscribe()
		w.markDegraded(err)
		if established {
			delay = cacheWatchRetryInitial
		}
		timer := time.NewTimer(delay)
		select {
		case <-w.done:
			timer.Stop()
			return
		case <-timer.C:
		}
		delay = min(2*delay, cacheWatchRetryMax)
	}
}

// openStream lets Close cancel even while yamux is waiting to open a stream.
// At most one opener remains pending; a late result is closed, never applied.
func (w *cacheWatch) openStream() (io.ReadWriteCloser, error) {
	type result struct {
		conn io.ReadWriteCloser
		err  error
	}
	opened := make(chan result)
	go func() {
		conn, err := w.open()
		select {
		case opened <- result{conn, err}:
		case <-w.done:
			if conn != nil {
				(&cacheWatchStream{conn: conn}).close()
			}
		}
	}()
	select {
	case r := <-opened:
		return r.conn, r.err
	case <-w.done:
		return nil, io.ErrClosedPipe
	}
}

func (w *cacheWatch) subscribe() (established bool, retErr error) {
	conn, err := w.openStream()
	if err != nil {
		return false, err
	}
	s := &cacheWatchStream{conn: conn}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		s.close()
		return false, io.ErrClosedPipe
	}
	w.stream, w.seq, w.hasSeq = s, 0, false
	w.cache.Invalidate()
	w.cache.SetEnabled(false)
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		if w.stream == s {
			w.stream = nil
			w.cache.SetEnabled(false)
		}
		w.mu.Unlock()
		s.close()
	}()
	if d, ok := conn.(interface{ SetDeadline(time.Time) error }); ok {
		d.SetDeadline(time.Now().Add(10 * time.Second))
	}
	if err := proto.WriteMsg(conn, proto.Header{Kind: proto.KindFSWatch, Path: w.root}); err != nil {
		return false, err
	}
	var initial proto.CacheEvent
	if err := proto.ReadMsg(conn, &initial); err != nil {
		return false, fmt.Errorf("cache watch handshake: %w", err)
	}
	if err := w.apply(s, initial); err != nil {
		return false, err
	}
	if d, ok := conn.(interface{ SetDeadline(time.Time) error }); ok {
		d.SetDeadline(time.Time{})
	}
	for {
		var event proto.CacheEvent
		if err := proto.ReadMsg(conn, &event); err != nil {
			return true, err
		}
		if err := w.apply(s, event); err != nil {
			return true, err
		}
	}
}

func (w *cacheWatch) apply(s *cacheWatchStream, event proto.CacheEvent) error {
	w.mu.Lock()
	if w.closed || w.stream != s {
		w.mu.Unlock()
		return io.ErrClosedPipe
	}
	if w.hasSeq && event.Seq < w.seq {
		w.cache.SetEnabled(false)
		w.mu.Unlock()
		return fmt.Errorf("cache watch sequence decreased")
	}
	if !w.hasSeq || event.Seq != w.seq {
		w.cache.Invalidate()
	}
	w.seq, w.hasSeq = event.Seq, true
	w.cache.SetEnabled(event.Enabled && w.connected)
	degraded := !w.degraded && !event.Enabled
	if degraded {
		w.degraded = true
	}
	w.mu.Unlock()
	if degraded {
		log.Printf("mount %s: content cache disabled while project monitoring recovers", w.root)
	}
	// A may only complete a command barrier once invalidation has been
	// applied. Acknowledging the bytes before clearing the cache is unsafe.
	if err := proto.WriteMsg(s.conn, proto.CacheAck{Seq: event.Seq}); err != nil {
		return err
	}
	// A peer that sends one event and immediately drops every ACK must not
	// produce a restored/degraded log pair on every retry attempt.
	w.mu.Lock()
	recovered := !w.closed && w.stream == s && w.degraded && event.Enabled && w.connected
	if recovered {
		w.degraded = false
	}
	w.mu.Unlock()
	if recovered {
		log.Printf("mount %s: content cache notifications restored", w.root)
	}
	return nil
}

func (w *cacheWatch) markDegraded(err error) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.cache.SetEnabled(false)
	changed := !w.degraded
	w.degraded = true
	w.mu.Unlock()
	if changed {
		log.Printf("mount %s: content cache notifications unavailable; using direct reads and retrying: %v", w.root, err)
	}
}

func (w *cacheWatch) setConnected(up bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.connected == up {
		return
	}
	w.connected = up
	w.cache.Invalidate()
	w.cache.SetEnabled(false)
}

func (w *cacheWatch) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	if w.done != nil {
		close(w.done)
	}
	w.cache.SetEnabled(false)
	stream := w.stream
	w.stream = nil
	w.mu.Unlock()
	if stream != nil {
		stream.close()
	}
	return nil
}

// The transport resumes the byte stream, but cached filesystem snapshots
// must not survive a lost connection. Enabling waits for a subsequent event.
func (h *host) cacheConnectionState(up bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cacheLinkDown = !up
	// setConnected only changes memory/cache state; holding the host lock
	// keeps state changes ordered with registration, never across network I/O.
	for _, watch := range h.cacheWatches {
		watch.setConnected(up)
	}
}
