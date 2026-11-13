package session

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"waldo/internal/audit"
	"waldo/internal/proto"
)

func execObservationFrames(t *testing.T) []byte {
	t.Helper()
	var wire bytes.Buffer
	fw := proto.NewFrameWriter(&wire)
	if err := fw.Write(proto.FStdout, bytes.Repeat([]byte("private-output\x00"), 8192)); err != nil {
		t.Fatal(err)
	}
	if err := fw.Write(proto.FStderr, []byte("private-error")); err != nil {
		t.Fatal(err)
	}
	if err := fw.WriteJSON(proto.FExit, proto.Exit{Code: 143, Signal: 15}); err != nil {
		t.Fatal(err)
	}
	return wire.Bytes()
}

func TestExecObserverFragmentedFrames(t *testing.T) {
	wire := execObservationFrames(t)
	for _, chunk := range []int{1, 2, 4, 5, 7, 1024, len(wire)} {
		o := &execObserver{}
		for offset := 0; offset < len(wire); offset += chunk {
			part := wire[offset:min(offset+chunk, len(wire))]
			if n, err := o.Write(part); n != len(part) || err != nil {
				t.Fatalf("chunk %d: Write=%d, %v", chunk, n, err)
			}
		}
		if o.exit == nil || *o.exit != (proto.Exit{Code: 143, Signal: 15}) || o.endedAt.IsZero() {
			t.Fatalf("chunk %d: exit=%+v, end=%v", chunk, o.exit, o.endedAt)
		}
	}
}

func TestExecObserverMalformedFramesDoNotAffectForwarding(t *testing.T) {
	for _, wire := range [][]byte{
		{proto.FExit, 0, 0, 0, 1, '{'},
		{proto.FExit, 0, 0, 4, 1},    // exit payload exceeds its bounded storage
		{proto.FStdout, 0, 16, 0, 1}, // protocol frame limit exceeded
	} {
		o := &execObserver{}
		if n, err := o.Write(wire); n != len(wire) || err != nil {
			t.Fatalf("observer rejected wire: %d %v", n, err)
		}
		if !o.disabled || o.exit != nil {
			t.Fatalf("malformed frame was not disabled: %+v", o)
		}
	}
	// Empty output frames may be adjacent to a normal exit.
	var wire bytes.Buffer
	proto.NewFrameWriter(&wire).Write(proto.FStdout, nil)
	proto.NewFrameWriter(&wire).WriteJSON(proto.FExit, proto.Exit{})
	o := &execObserver{}
	o.Write(wire.Bytes())
	if o.exit == nil || o.exit.Code != 0 {
		t.Fatalf("empty output frame swallowed exit: %+v", o)
	}
}

func TestSpliceExecPreservesBytesAndRecordsCompletion(t *testing.T) {
	stub, remote := net.Pipe()
	sandbox, process := net.Pipe()
	for _, c := range []net.Conn{stub, remote, sandbox, process} {
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
	}
	dir := t.TempDir()
	logger, err := audit.Open(dir, "exec-test")
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()
	done := make(chan struct{})
	go func() {
		spliceExec(remote, sandbox, logger, 42, "/bin/bash", time.Now().Add(-100*time.Millisecond))
		close(done)
	}()

	input := []byte{proto.FStdin, 0, 0, 0, 3, 'a', 0, 'b', proto.FSignal, 0, 0, 0, 1, 2}
	errCh := make(chan error, 1)
	wire := execObservationFrames(t)
	go func() {
		got := make([]byte, len(input))
		if _, err := io.ReadFull(process, got); err != nil {
			errCh <- err
			return
		}
		if !bytes.Equal(got, input) {
			errCh <- io.ErrUnexpectedEOF
			return
		}
		_, err := process.Write(wire)
		process.Close()
		errCh <- err
	}()
	if _, err := stub.Write(input); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(stub)
	if err != nil || !bytes.Equal(got, wire) {
		t.Fatalf("output forwarding changed: equal=%t, err=%v", bytes.Equal(got, wire), err)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("splice did not finish")
	}
	data, err := os.ReadFile(audit.Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("private-output")) || bytes.Contains(data, []byte("private-error")) {
		t.Fatal("performance log recorded output")
	}
	var event map[string]any
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{"kind": "exec-end", "id": float64(42), "program": "bash", "status": "completed", "exit_code": float64(143), "signal": float64(15)} {
		if !reflect.DeepEqual(event[key], want) {
			t.Errorf("%s=%v, want %v", key, event[key], want)
		}
	}
	if duration, ok := event["duration_ms"].(float64); !ok || duration < 100 {
		t.Fatalf("duration_ms=%v", event["duration_ms"])
	}
}

func TestSpliceExecRecordsDisconnectWithoutExit(t *testing.T) {
	stub, remote := net.Pipe()
	sandbox, process := net.Pipe()
	for _, c := range []net.Conn{stub, remote, sandbox, process} {
		defer c.Close()
	}
	dir := t.TempDir()
	logger, err := audit.Open(dir, "exec-disconnect")
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()
	done := make(chan struct{})
	go func() {
		spliceExec(remote, sandbox, logger, 1, filepath.Join("/usr", "bin", "python3"), time.Now())
		close(done)
	}()
	process.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("disconnected splice did not finish")
	}
	data, err := os.ReadFile(audit.Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatal(err)
	}
	if event["status"] != "disconnected" || event["program"] != "python3" {
		t.Fatalf("unexpected disconnect event: %s", data)
	}
	if _, ok := event["exit_code"]; ok {
		t.Fatalf("disconnect got an invented exit code: %s", data)
	}
}

func TestExecObserverDoesNotRetainOutputPayload(t *testing.T) {
	var header [5]byte
	header[0] = proto.FStdout
	binary.BigEndian.PutUint32(header[1:], 1<<20)
	o := &execObserver{}
	o.Write(header[:])
	output := bytes.Repeat([]byte{'x'}, 1<<20)
	o.Write(output)
	if o.exitN != 0 || o.headerN != 0 || o.remaining != 0 {
		t.Fatalf("output retained or incomplete: %+v", o)
	}
	if !bytes.Equal(o.exitBody[:], make([]byte, len(o.exitBody))) {
		t.Fatal("stdout was copied into observer storage")
	}
}

type execHalfCloseProbe struct {
	io.ReadWriteCloser
	writeClosed bool
	closed      bool
}

func (p *execHalfCloseProbe) CloseWrite() error { p.writeClosed = true; return nil }
func (p *execHalfCloseProbe) Close() error      { p.closed = true; return nil }

func TestExecObservedConnPreservesHalfClose(t *testing.T) {
	probe := &execHalfCloseProbe{}
	observed := execObservedConn{ReadWriteCloser: probe, observer: &execObserver{}}
	if err := observed.CloseWrite(); err != nil || !probe.writeClosed || probe.closed {
		t.Fatalf("half-close changed: writeClosed=%t closed=%t err=%v", probe.writeClosed, probe.closed, err)
	}
}
