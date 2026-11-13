package host

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/sftp"

	"waldo/internal/fscache"
	"waldo/internal/proto"
)

type execRelayFixture struct {
	stub, remote net.Conn
	done         chan struct{}
}

func newExecRelayFixture(t *testing.T, beforeExit func()) *execRelayFixture {
	t.Helper()
	stub, relayStub := net.Pipe()
	remote, relayRemote := net.Pipe()
	for _, c := range []net.Conn{stub, relayStub, remote, relayRemote} {
		c.SetDeadline(time.Now().Add(5 * time.Second))
	}
	done := make(chan struct{})
	go func() { relayExec(relayRemote, relayStub, beforeExit); close(done) }()
	t.Cleanup(func() {
		stub.Close()
		remote.Close()
		relayStub.Close()
		relayRemote.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("exec relay did not stop")
		}
	})
	return &execRelayFixture{stub: stub, remote: remote, done: done}
}

func TestExecRelayPreservesFramesAndInvalidatesBeforeExit(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var completed atomic.Bool
	f := newExecRelayFixture(t, func() { close(entered); <-release; completed.Store(true) })
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	frames := []struct {
		kind byte
		body []byte
	}{
		{proto.FStdout, []byte("stdout\x00\xff\n")},
		{proto.FStderr, []byte("stderr\n")},
		{proto.FError, []byte("explanation")},
		{99, []byte("future frame")},
	}
	exit := []byte("{ \"code\" : 17, \"signal\" : 0 }")
	sent := make(chan error, 1)
	go func() {
		writer := proto.NewFrameWriter(f.remote)
		for _, frame := range frames {
			if err := writer.Write(frame.kind, frame.body); err != nil {
				sent <- err
				return
			}
		}
		sent <- writer.Write(proto.FExit, exit)
	}()
	for _, frame := range frames {
		kind, body, err := proto.ReadFrame(f.stub)
		if err != nil || kind != frame.kind || !bytes.Equal(body, frame.body) {
			t.Fatalf("changed frame: kind=%d body=%q err=%v", kind, body, err)
		}
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("exit did not reach local barrier")
	}
	observed := make(chan struct{}, 1)
	var kind byte
	var body []byte
	var err error
	go func() { kind, body, err = proto.ReadFrame(f.stub); observed <- struct{}{} }()
	select {
	case <-observed:
		t.Fatal("FExit reached stub before barrier completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("FExit did not reach stub")
	}
	if err != nil || kind != proto.FExit || !bytes.Equal(body, exit) || !completed.Load() {
		t.Fatalf("exit changed or barrier incomplete: kind=%d body=%q err=%v done=%t", kind, body, err, completed.Load())
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.done:
	case <-time.After(time.Second):
		t.Fatal("terminal FExit did not stop relay")
	}
}

func TestExecRelayPreservesInputAndControlFrames(t *testing.T) {
	f := newExecRelayFixture(t, nil)
	frames := []struct {
		kind byte
		body []byte
	}{
		{proto.FStdin, []byte("input\x00\xff")},
		{proto.FStdinEOF, nil},
		{proto.FSignal, []byte{2}},
		{proto.FResize, []byte(`{"rows":30,"cols":100}`)},
	}
	sent := make(chan error, 1)
	go func() {
		writer := proto.NewFrameWriter(f.stub)
		for _, frame := range frames {
			if err := writer.Write(frame.kind, frame.body); err != nil {
				sent <- err
				return
			}
		}
		sent <- nil
	}()
	for _, frame := range frames {
		kind, body, err := proto.ReadFrame(f.remote)
		if err != nil || kind != frame.kind || !bytes.Equal(body, frame.body) {
			t.Fatalf("changed control frame: kind=%d body=%q err=%v", kind, body, err)
		}
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	go proto.NewFrameWriter(f.remote).WriteJSON(proto.FExit, proto.Exit{})
	if kind, _, err := proto.ReadFrame(f.stub); err != nil || kind != proto.FExit {
		t.Fatalf("stdin EOF stopped output: %d %v", kind, err)
	}
}

func TestExecRelayDisconnectClosesOtherSide(t *testing.T) {
	for _, side := range []string{"stub", "remote"} {
		t.Run(side, func(t *testing.T) {
			f := newExecRelayFixture(t, nil)
			other := f.remote
			if side == "stub" {
				f.stub.Close()
			} else {
				f.remote.Close()
				other = f.stub
			}
			if _, err := other.Read(make([]byte, 1)); err == nil {
				t.Fatal("disconnected relay left peer open")
			}
			select {
			case <-f.done:
			case <-time.After(time.Second):
				t.Fatal("disconnected relay did not finish")
			}
		})
	}
}

type execHalfClose struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *execHalfClose) Close() error { c.once.Do(func() { close(c.closed) }); return nil }

func TestExecRelayStubDisconnectInterruptsHalfClosedUpstreamRead(t *testing.T) {
	stub, relayStub := net.Pipe()
	remote, relayRemote := net.Pipe()
	defer remote.Close()
	defer relayRemote.Close()
	defer relayStub.Close()
	defer stub.Close()
	half := &execHalfClose{Conn: relayRemote, closed: make(chan struct{})}
	done := make(chan struct{})
	go func() { relayExec(half, relayStub, nil); close(done) }()
	stub.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("upstream local half-close left frame reader blocked")
	}
	select {
	case <-half.closed:
	case <-time.After(time.Second):
		t.Fatal("stub disconnect did not close upstream")
	}
}

type execBlockingClose struct {
	net.Conn
	entered, release, closed chan struct{}
}

func (c *execBlockingClose) Close() error {
	close(c.entered)
	<-c.release
	err := c.Conn.Close()
	close(c.closed)
	return err
}

func TestExecRelayBlockedCloseDoesNotDelayShutdown(t *testing.T) {
	for _, reason := range []string{"stub disconnect", "exit frame"} {
		t.Run(reason, func(t *testing.T) {
			stub, relayStub := net.Pipe()
			remote, relayRemote := net.Pipe()
			blocked := &execBlockingClose{Conn: relayRemote, entered: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
			defer stub.Close()
			defer relayStub.Close()
			defer remote.Close()
			defer relayRemote.Close()
			defer func() {
				select {
				case <-blocked.release:
				default:
					close(blocked.release)
				}
			}()
			done := make(chan struct{})
			go func() { relayExec(blocked, relayStub, nil); close(done) }()
			if reason == "stub disconnect" {
				stub.Close()
			} else {
				go proto.NewFrameWriter(remote).WriteJSON(proto.FExit, proto.Exit{})
				stub.SetReadDeadline(time.Now().Add(time.Second))
				if kind, _, err := proto.ReadFrame(stub); err != nil || kind != proto.FExit {
					t.Fatalf("command completion blocked: %d %v", kind, err)
				}
			}
			select {
			case <-blocked.entered:
			case <-time.After(time.Second):
				t.Fatal("upstream Close was never attempted")
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("relay waited for blocked FIN/Close")
			}
			select {
			case <-blocked.closed:
				t.Fatal("fake Close completed before its release")
			default:
			}
			close(blocked.release)
			select {
			case <-blocked.closed:
			case <-time.After(time.Second):
				t.Fatal("asynchronous Close did not eventually finish")
			}
		})
	}
}

type execCountedConn struct {
	net.Conn
	writes atomic.Uint64
}

func (c *execCountedConn) Write(p []byte) (int, error) { c.writes.Add(1); return c.Conn.Write(p) }

func TestExecExitInvalidatesRealSFTPCacheWithoutWatchEvent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	clientConn, front := net.Pipe()
	upstream, serverConn := net.Pipe()
	for _, conn := range []net.Conn{clientConn, front, upstream, serverConn} {
		conn.SetDeadline(time.Now().Add(5 * time.Second))
	}
	counted := &execCountedConn{Conn: upstream}
	cache := fscache.New(root, time.Minute, time.Minute)
	cache.SetEnabled(true)
	server, err := sftp.NewServer(serverConn)
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve()
	cacheDone := make(chan error, 1)
	go func() { cacheDone <- cache.Serve(front, counted) }()
	client, err := sftp.NewClientPipe(clientConn, clientConn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		client.Close()
		server.Close()
		clientConn.Close()
		front.Close()
		upstream.Close()
		serverConn.Close()
		select {
		case <-cacheDone:
		case <-time.After(time.Second):
			t.Error("SFTP cache did not stop")
		}
	})
	read := func() string {
		h, err := client.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.Stat(); err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(h)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.Close(); err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if got := read(); got != "old" {
		t.Fatal(got)
	}
	before := counted.writes.Load()
	if got := read(); got != "old" {
		t.Fatal(got)
	}
	if counted.writes.Load() != before {
		t.Fatal("test did not warm the content cache")
	}
	// A changes the source without delivering any watch event. This represents
	// a timed-out notification subscription whose command barrier was released.
	if err := os.WriteFile(path, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	h := &host{cacheWatches: []*cacheWatch{{cache: cache}}}
	f := newExecRelayFixture(t, h.invalidateCachesBeforeExit)
	go proto.NewFrameWriter(f.remote).WriteJSON(proto.FExit, proto.Exit{})
	if kind, _, err := proto.ReadFrame(f.stub); err != nil || kind != proto.FExit {
		t.Fatalf("missing command exit: %d %v", kind, err)
	}
	// A late heartbeat is allowed to renew the lease, but cannot resurrect an
	// entry or an in-flight fill from before B observed command completion.
	cache.SetEnabled(true)
	if got := read(); got != "new" {
		t.Fatalf("command exit exposed old cached contents: %q", got)
	}
}
