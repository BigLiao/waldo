package hostbash

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"waldo/internal/proto"
)

func execSocket(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		serve(c)
	}()
	t.Cleanup(func() {
		ln.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("exec fixture did not stop")
		}
	})
	return socket
}

func receiveExec(t *testing.T, c net.Conn) proto.ExecReq {
	t.Helper()
	var header proto.Header
	if err := proto.ReadMsg(c, &header); err != nil || header.Kind != proto.KindExec || header.Exec == nil {
		t.Errorf("bad exec header: %+v %v", header, err)
		return proto.ExecReq{}
	}
	kind, _, err := proto.ReadFrame(c)
	if err != nil || kind != proto.FStdinEOF {
		t.Errorf("expected explicit stdin EOF: %d %v", kind, err)
	}
	return *header.Exec
}

func TestExecuteReusesFramedExecAndBoundsOutput(t *testing.T) {
	socket := execSocket(t, func(c net.Conn) {
		req := receiveExec(t, c)
		if req.Tool != ToolName || req.Path != "/host/bin/bash" || req.Cwd != "/project/sub" || req.TTY || len(req.Env) != 0 || strings.Join(req.Argv, "|") != "/host/bin/bash|--noprofile|--norc|-c|printf hi" {
			t.Errorf("bad command request: %+v", req)
		}
		fw := proto.NewFrameWriter(c)
		for range 5 {
			if err := fw.Write(proto.FStdout, []byte(strings.Repeat("x", 32<<10))); err != nil {
				t.Error(err)
				return
			}
		}
		fw.Write(proto.FStderr, []byte("stderr"))
		fw.WriteJSON(proto.FExit, proto.Exit{Code: 7})
	})
	result, err := Execute(context.Background(), socket, "/host/bin/bash", "/project", Call{Command: "printf hi", Cwd: "/project/sub"})
	if err != nil || len(result.Stdout) != 128<<10 || result.Stderr != "stderr" || result.ExitCode != 7 || !result.Truncated {
		t.Fatalf("result: stdout=%d stderr=%q exit=%d truncated=%t err=%v", len(result.Stdout), result.Stderr, result.ExitCode, result.Truncated, err)
	}
}

func TestExecuteTimeoutClosesExecChannel(t *testing.T) {
	closed := make(chan error, 1)
	socket := execSocket(t, func(c net.Conn) { receiveExec(t, c); _, _, err := proto.ReadFrame(c); closed <- err })
	result, err := Execute(context.Background(), socket, "/bin/bash", "/project", Call{Command: "sleep 30", TimeoutMS: 100})
	if !errors.Is(err, context.DeadlineExceeded) || result.ExitCode != 255 {
		t.Fatalf("timeout: %+v %v", result, err)
	}
	select {
	case err := <-closed:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("peer did not see cancellation EOF: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("command channel remained open")
	}
}

func TestExecuteDoesNotTreatLostChannelAsSuccess(t *testing.T) {
	socket := execSocket(t, func(c net.Conn) { receiveExec(t, c) })
	result, err := Execute(context.Background(), socket, "/bin/bash", "/project", Call{Command: "id"})
	if err == nil || result.ExitCode != 255 {
		t.Fatalf("lost channel reported success: %+v %v", result, err)
	}
}

func TestFileToolArgumentsUseStdinAndKeepAttribution(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"path": "/project/private.txt", "content": strings.Repeat("secret$()", 10000)})
	socket := execSocket(t, func(c net.Conn) {
		var header proto.Header
		if e := proto.ReadMsg(c, &header); e != nil {
			t.Error(e)
			return
		}
		req := header.Exec
		if req.Tool != "Write" || req.Path != "/run/waldo/waldo" || strings.Join(req.Argv, " ") != "/run/waldo/waldo host-file-tool Write" {
			t.Errorf("bad request: %+v", req)
		}
		var input []byte
		for {
			kind, p, e := proto.ReadFrame(c)
			if e != nil {
				t.Error(e)
				return
			}
			if kind == proto.FStdinEOF {
				break
			}
			if kind != proto.FStdin {
				t.Error("bad frame")
				return
			}
			input = append(input, p...)
		}
		if string(input) != string(raw) {
			t.Error("arguments changed")
		}
		fw := proto.NewFrameWriter(c)
		fw.Write(proto.FStdout, []byte(`{"bytes":80000}`))
		fw.WriteJSON(proto.FExit, proto.Exit{})
	})
	result, e := Execute(context.Background(), socket, "/bin/bash", "/project", Call{Tool: "Write", Arguments: raw})
	if e != nil || result.ExitCode != 0 || result.Truncated {
		t.Fatal(result, e)
	}
}
