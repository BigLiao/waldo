// Package reconnect provides an ordered, bounded byte stream that survives
// replacement of its underlying SSH connection. It sits below yamux so open
// SFTP handles, exec streams and terminal state do not need to be recreated.
package reconnect

import (
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"time"
)

const (
	Window        = 90 * time.Second
	maxPending    = 4 << 20
	chunkSize     = 32 << 10
	closePacket   = ^uint32(0)
	timeoutPacket = closePacket - 1
	heartbeat     = time.Second
	deadAfter     = 5 * time.Second
)

var ErrTimeout = errors.New("reconnect window expired")

// Stream buffers at most 4 MiB per direction; writers block under pressure.
// Acknowledgements refer to consumed bytes, while handshakes also report
// received bytes so unconsumed data is neither lost nor replayed on attach.
type Stream struct {
	mu             sync.Mutex
	cond           *sync.Cond
	serveMu        sync.Mutex
	tx, rx         []byte
	txBase, rxBase uint64
	err            error
	done           chan struct{}
	wake           chan struct{}
	active         io.ReadWriteCloser
	connected      bool
	offline        time.Time
	window         time.Duration
	// State is called on successful attachment and disconnection. Set before use.
	State func(bool)
}

func New(window time.Duration) *Stream {
	s := &Stream{done: make(chan struct{}), wake: make(chan struct{}, 1), window: window, offline: time.Now()}
	s.cond = sync.NewCond(&s.mu)
	go func() {
		t := time.NewTicker(20 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-s.done:
				return
			case <-t.C:
				s.mu.Lock()
				expired := s.err == nil && !s.offline.IsZero() && time.Since(s.offline) >= s.window
				if expired {
					s.err = ErrTimeout
					close(s.done)
					s.cond.Broadcast()
				}
				s.mu.Unlock()
				if expired {
					s.notify()
					return
				}
			}
		}
	}()
	return s
}

func (s *Stream) Done() <-chan struct{} { return s.done }
func (s *Stream) Err() error            { s.mu.Lock(); defer s.mu.Unlock(); return s.err }
func (s *Stream) Connected() bool       { s.mu.Lock(); defer s.mu.Unlock(); return s.connected }
func (s *Stream) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Stream) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
		close(s.done)
		s.cond.Broadcast()
	}
	s.mu.Unlock()
	s.notify()
}

func (s *Stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.rx) == 0 && s.err == nil {
		s.cond.Wait()
	}
	if s.err != nil && !errors.Is(s.err, io.EOF) {
		return 0, s.err
	}
	if len(s.rx) == 0 {
		return 0, s.err
	}
	n := copy(p, s.rx)
	s.rx = s.rx[n:]
	s.rxBase += uint64(n)
	s.notify()
	return n, nil
}

func (s *Stream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for len(p) > 0 {
		for len(s.tx) == maxPending && s.err == nil {
			s.cond.Wait()
		}
		if s.err != nil {
			return n, s.err
		}
		k := min(len(p), maxPending-len(s.tx))
		s.tx = append(s.tx, p[:k]...)
		p = p[k:]
		n += k
		s.notify()
	}
	return n, nil
}

// Drain waits for the peer to consume all pending bytes, including an exit
// status produced while disconnected. The reconnect window still applies.
func (s *Stream) Drain() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.tx) > 0 && s.err == nil {
		s.cond.Wait()
	}
}

func (s *Stream) Close() error {
	s.fail(io.EOF)
	s.serveMu.Lock()
	s.serveMu.Unlock()
	return nil
}

// Serve attaches one physical connection until it fails. A new attach takes
// over a stale connection, but never creates a new logical session.
func (s *Stream) Serve(raw io.ReadWriteCloser) error {
	s.mu.Lock()
	old := s.active
	s.mu.Unlock()
	if old != nil {
		old.Close()
	}
	s.serveMu.Lock()
	defer s.serveMu.Unlock()
	defer raw.Close()
	s.mu.Lock()
	if s.err != nil {
		err := s.err
		s.mu.Unlock()
		return err
	}
	s.active = raw
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		wasConnected := s.connected
		s.connected = false
		s.active = nil
		if s.offline.IsZero() {
			s.offline = time.Now()
		}
		s.mu.Unlock()
		if wasConnected && s.State != nil {
			s.State(false)
		}
	}()

	// Every physical connection must make progress, including the handshake.
	progress := make(chan struct{}, 1)
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		t := time.NewTimer(deadAfter)
		defer t.Stop()
		for {
			select {
			case <-finished:
				return
			case <-s.done:
				// Give the sender a bounded opportunity to send a close packet.
				time.Sleep(100 * time.Millisecond)
				raw.Close()
				return
			case <-progress:
				if !t.Stop() {
					select {
					case <-t.C:
					default:
					}
				}
				t.Reset(deadAfter)
			case <-t.C:
				raw.Close()
				return
			}
		}
	}()

	var hello [24]byte
	copy(hello[:8], "WALDOR01")
	s.mu.Lock()
	binary.BigEndian.PutUint64(hello[8:16], s.rxBase)
	binary.BigEndian.PutUint64(hello[16:24], s.rxBase+uint64(len(s.rx)))
	s.mu.Unlock()
	written := make(chan error, 1)
	go func() { written <- writeFull(raw, hello[:]) }()
	var peer [24]byte
	if _, err := io.ReadFull(raw, peer[:]); err != nil {
		return err
	}
	if err := <-written; err != nil {
		return err
	}
	if string(peer[:8]) != "WALDOR01" {
		s.fail(errors.New("invalid reconnect handshake"))
		return s.Err()
	}
	ack, cursor := binary.BigEndian.Uint64(peer[8:16]), binary.BigEndian.Uint64(peer[16:24])
	s.mu.Lock()
	if s.err != nil {
		err := s.err
		s.mu.Unlock()
		return err
	}
	if ack < s.txBase || cursor < ack || cursor > s.txBase+uint64(len(s.tx)) {
		s.mu.Unlock()
		s.fail(errors.New("invalid reconnect offsets"))
		return s.Err()
	}
	s.tx = s.tx[ack-s.txBase:]
	s.txBase = ack
	s.connected = true
	s.offline = time.Time{}
	s.cond.Broadcast()
	s.mu.Unlock()
	if s.State != nil {
		s.State(true)
	}

	received := make(chan error, 1)
	recvDone := make(chan struct{})
	go func() { defer close(recvDone); received <- s.receive(raw, progress) }()
	defer func() { raw.Close(); <-recvDone }()
	tick := time.NewTicker(heartbeat)
	defer tick.Stop()
	for {
		s.mu.Lock()
		if s.err != nil {
			err := s.err
			s.mu.Unlock()
			writeEnd(raw, err)
			return err
		}
		n := min(chunkSize, int(s.txBase+uint64(len(s.tx))-cursor))
		packet := make([]byte, 20+n)
		binary.BigEndian.PutUint64(packet[:8], s.rxBase)
		binary.BigEndian.PutUint64(packet[8:16], cursor)
		binary.BigEndian.PutUint32(packet[16:20], uint32(n))
		copy(packet[20:], s.tx[cursor-s.txBase:cursor-s.txBase+uint64(n)])
		s.mu.Unlock()
		if err := writeFull(raw, packet); err != nil {
			return err
		}
		cursor += uint64(n)
		if n > 0 {
			continue
		}
		select {
		case err := <-received:
			return err
		case <-s.done:
			// Explicit termination releases the remote immediately when online.
			writeEnd(raw, s.Err())
			return s.Err()
		case <-s.wake:
		case <-tick.C:
		}
	}
}

func (s *Stream) receive(raw io.Reader, progress chan<- struct{}) error {
	for {
		var h [20]byte
		if _, err := io.ReadFull(raw, h[:]); err != nil {
			return err
		}
		select {
		case progress <- struct{}{}:
		default:
		}
		ack, seq, n := binary.BigEndian.Uint64(h[:8]), binary.BigEndian.Uint64(h[8:16]), binary.BigEndian.Uint32(h[16:])
		if n == closePacket {
			s.fail(io.EOF)
			return io.EOF
		}
		if n == timeoutPacket {
			s.fail(ErrTimeout)
			return ErrTimeout
		}
		if n > chunkSize {
			s.fail(errors.New("reconnect packet too large"))
			return s.Err()
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(raw, p); err != nil {
			return err
		}
		s.mu.Lock()
		if ack < s.txBase || ack > s.txBase+uint64(len(s.tx)) || seq != s.rxBase+uint64(len(s.rx)) || len(s.rx)+int(n) > maxPending {
			s.mu.Unlock()
			s.fail(errors.New("invalid reconnect sequence or acknowledgement"))
			return s.Err()
		}
		s.tx = s.tx[ack-s.txBase:]
		s.txBase = ack
		s.rx = append(s.rx, p...)
		s.cond.Broadcast()
		s.mu.Unlock()
	}
}

func writeEnd(w io.Writer, err error) {
	var p [20]byte
	kind := closePacket
	if errors.Is(err, ErrTimeout) {
		kind = timeoutPacket
	}
	binary.BigEndian.PutUint32(p[16:], kind)
	writeFull(w, p[:])
}

func writeFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}
