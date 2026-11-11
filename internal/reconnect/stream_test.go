package reconnect

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func pair(t *testing.T, window time.Duration) (*Stream, *Stream) {
	t.Helper()
	a, b := New(window), New(window)
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

func wait(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("condition timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

func attach(t *testing.T, a, b *Stream) (net.Conn, net.Conn) {
	t.Helper()
	x, y := net.Pipe()
	go a.Serve(x)
	go b.Serve(y)
	wait(t, func() bool { return a.Connected() && b.Connected() })
	return x, y
}

func TestResumeUnconsumedBytes(t *testing.T) {
	a, b := pair(t, 3*time.Second)
	x, _ := attach(t, a, b)
	first := bytes.Repeat([]byte("first payload"), 8000)
	a.Write(first)
	wait(t, func() bool { b.mu.Lock(); defer b.mu.Unlock(); return len(b.rx) == len(first) })
	// The receiver has accepted the bytes but the application has not read
	// them. Reattaching must preserve that buffer without replaying it.
	x.Close()
	wait(t, func() bool { return !a.Connected() && !b.Connected() })
	second := []byte("second payload")
	a.Write(second)
	attach(t, a, b)
	got := make([]byte, len(first)+len(second))
	if _, err := io.ReadFull(b, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, append(first, second...)) {
		t.Fatal("data lost or duplicated across reconnect")
	}
	b.Write([]byte("reply"))
	got = make([]byte, 5)
	if _, err := io.ReadFull(a, got); err != nil || string(got) != "reply" {
		t.Fatalf("reverse stream: %q, %v", got, err)
	}
}

// Cut a physical write halfway through a data packet, after its header.
type cutWriter struct {
	net.Conn
	left int
}

func (c *cutWriter) Write(p []byte) (int, error) {
	if c.left == 0 {
		c.Conn.Close()
		return 0, io.ErrClosedPipe
	}
	n, err := c.Conn.Write(p[:min(len(p), c.left)])
	c.left -= n
	return n, err
}

func TestResumePartialPacket(t *testing.T) {
	a, b := pair(t, 3*time.Second)
	x, y := net.Pipe()
	// 24-byte handshake + 20-byte empty packet + one data header and 100 bytes.
	go a.Serve(&cutWriter{Conn: x, left: 24 + 20 + 20 + 100})
	go b.Serve(y)
	wait(t, func() bool { return a.Connected() && b.Connected() })
	want := bytes.Repeat([]byte("partial packet"), 4000)
	a.Write(want)
	wait(t, func() bool { return !a.Connected() && !b.Connected() })
	attach(t, a, b)
	got := make([]byte, len(want))
	if _, err := io.ReadFull(b, got); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("partial packet recovery: %v", err)
	}
}

func TestBackpressureAndRepeatedReconnect(t *testing.T) {
	a, b := pair(t, 3*time.Second)
	x, _ := attach(t, a, b)
	want := bytes.Repeat([]byte("0123456789abcdef"), (maxPending*2)/16)
	wrote := make(chan error, 1)
	go func() { _, err := a.Write(want); wrote <- err }()
	wait(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return len(a.tx) == maxPending })
	select {
	case <-wrote:
		t.Fatal("writer did not apply backpressure")
	default:
	}
	x.Close()
	wait(t, func() bool { return !a.Connected() && !b.Connected() })
	x, _ = attach(t, a, b)
	x.Close()
	wait(t, func() bool { return !a.Connected() && !b.Connected() })
	attach(t, a, b)
	got := make([]byte, len(want))
	if _, err := io.ReadFull(b, got); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("large transfer recovery: %v", err)
	}
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
}

func TestWindowExpiresAndUnblocksIO(t *testing.T) {
	s := New(100 * time.Millisecond)
	defer s.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := s.Read(make([]byte, 1))
		if !errors.Is(err, ErrTimeout) {
			t.Errorf("read: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		_, err := s.Write(make([]byte, maxPending+1))
		if !errors.Is(err, ErrTimeout) {
			t.Errorf("write: %v", err)
		}
	}()
	wg.Wait()
	if !errors.Is(s.Err(), ErrTimeout) {
		t.Fatal(s.Err())
	}
	x, y := net.Pipe()
	defer y.Close()
	if err := s.Serve(x); !errors.Is(err, ErrTimeout) {
		t.Fatalf("expired session reattached: %v", err)
	}
}

func TestCloseNotifiesPeer(t *testing.T) {
	a, b := pair(t, 3*time.Second)
	attach(t, a, b)
	a.Close()
	select {
	case <-b.Done():
	case <-time.After(time.Second):
		t.Fatal("explicit close left the peer waiting for the reconnect window")
	}
}

func TestTimeoutDiscardsBufferedData(t *testing.T) {
	a, b := pair(t, 100*time.Millisecond)
	x, _ := attach(t, a, b)
	a.Write([]byte("pending"))
	wait(t, func() bool { b.mu.Lock(); defer b.mu.Unlock(); return len(b.rx) > 0 })
	x.Close()
	select {
	case <-b.Done():
	case <-time.After(time.Second):
		t.Fatal("disconnected session did not expire")
	}
	if _, err := b.Read(make([]byte, 10)); !errors.Is(err, ErrTimeout) {
		t.Fatalf("expired session delivered stale data: %v", err)
	}
}

func TestTimeoutReasonReachesPeer(t *testing.T) {
	a, b := pair(t, 3*time.Second)
	attach(t, a, b)
	a.fail(ErrTimeout)
	select {
	case <-b.Done():
	case <-time.After(time.Second):
		t.Fatal("peer did not receive terminal timeout")
	}
	if !errors.Is(b.Err(), ErrTimeout) {
		t.Fatalf("lost timeout reason: %v", b.Err())
	}
}

func TestTakeoverStaleConnection(t *testing.T) {
	a, b := pair(t, 3*time.Second)
	attach(t, a, b)
	// A new SSH attachment must replace a half-open old one on B.
	x, y := net.Pipe()
	go a.Serve(x)
	go b.Serve(y)
	wait(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.active == x && a.connected })
	a.Write([]byte("new"))
	got := make([]byte, 3)
	if _, err := io.ReadFull(b, got); err != nil || string(got) != "new" {
		t.Fatalf("takeover: %q %v", got, err)
	}
}
