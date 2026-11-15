package host

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"
	"waldo/internal/proto"
)

type execRelayFixture struct {
	stub, remote net.Conn
	done         chan struct{}
}

func newExecRelayFixture(t *testing.T) *execRelayFixture {
	t.Helper()
	stub, relayStub := net.Pipe()
	remote, relayRemote := net.Pipe()
	for _, c := range []net.Conn{stub, relayStub, remote, relayRemote} {
		c.SetDeadline(time.Now().Add(5 * time.Second))
	}
	done := make(chan struct{})
	go func() { relayExec(relayRemote, relayStub); close(done) }()
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

func TestExecRelayPreservesOutputFrames(t *testing.T) {
	f := newExecRelayFixture(t)
	var expected bytes.Buffer
	fw := proto.NewFrameWriter(&expected)
	fw.Write(proto.FStdout, []byte("output"))
	fw.Write(proto.FStderr, []byte("error"))
	fw.WriteJSON(proto.FExit, proto.Exit{Code: 7})
	go f.remote.Write(expected.Bytes())
	got, err := io.ReadAll(f.stub)
	if err != nil || !bytes.Equal(got, expected.Bytes()) {
		t.Fatalf("changed frames: %x %v", got, err)
	}
}

func TestExecRelayPreservesInputAndControlFrames(t *testing.T) {
	f := newExecRelayFixture(t)
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
			f := newExecRelayFixture(t)
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
	go func() { relayExec(half, relayStub); close(done) }()
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
			go func() { relayExec(blocked, relayStub); close(done) }()
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
