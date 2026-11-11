package serve

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"

	"waldo/internal/proto"
	"waldo/internal/util"
)

// drainGrace is how long we keep relaying output after the main process has
// exited while something else (a background job) still holds the pipe.
const drainGrace = 200 * time.Millisecond

// sink forwards output to the stream until it is told to discard; background
// processes that keep the pipe open then never see EPIPE.
type sink struct {
	fw      *proto.FrameWriter
	typ     byte
	discard atomic.Bool
}

func (s *sink) Write(p []byte) (int, error) {
	if !s.discard.Load() {
		if err := s.fw.Write(s.typ, p); err != nil {
			s.discard.Store(true)
		}
	}
	return len(p), nil
}

func fail(st io.WriteCloser, fw *proto.FrameWriter, code int, format string, a ...any) {
	fw.Write(proto.FError, []byte("waldo: "+fmt.Sprintf(format, a...)+"\n"))
	fw.WriteJSON(proto.FExit, proto.Exit{Code: code})
	st.Close()
}

// runExec runs one forwarded process. The stream carries frames both ways.
// A stream that closes before the process exits means the stub on B died or
// the connection dropped: the whole process group is killed. After a normal
// exit, background processes keep running (until the session ends).
func runExec(st io.ReadWriteCloser, req *proto.ExecReq, execEnv []string) {
	fw := proto.NewFrameWriter(st)
	if len(req.Argv) == 0 || req.Path == "" || !strings.HasPrefix(req.Path, "/") {
		fail(st, fw, 126, "invalid exec request")
		return
	}
	if fi, err := os.Stat(req.Path); err != nil || fi.IsDir() {
		fail(st, fw, 127, "%s: 在 A 的沙箱中不存在（命令执行转发到 A）", req.Path)
		return
	}
	if !util.IsDir(req.Cwd) {
		fail(st, fw, 126, "工作目录 %s 在 A 的沙箱中不存在", req.Cwd)
		return
	}
	env := append([]string(nil), req.Env...)
	for _, e := range execEnv {
		k, v, _ := strings.Cut(e, "=")
		env = util.EnvSet(env, k, v)
	}
	cmd := &exec.Cmd{Path: req.Path, Args: req.Argv, Dir: req.Cwd, Env: env}

	var (
		stdin   io.WriteCloser
		outs    []io.Reader
		master  *os.File
		outSink = &sink{fw: fw, typ: proto.FStdout}
		errSink = &sink{fw: fw, typ: proto.FStderr}
	)
	if req.TTY {
		sz := &pty.Winsize{Rows: req.Rows, Cols: req.Cols}
		if sz.Rows == 0 || sz.Cols == 0 {
			sz = &pty.Winsize{Rows: 24, Cols: 80}
		}
		m, err := pty.StartWithSize(cmd, sz)
		if err != nil {
			fail(st, fw, 126, "%s: %v", req.Path, err)
			return
		}
		master = m
		stdin = m
		outs = []io.Reader{m}
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		inR, inW, err1 := os.Pipe()
		outR, outW, err2 := os.Pipe()
		errR, errW, err3 := os.Pipe()
		if err := errors.Join(err1, err2, err3); err != nil {
			fail(st, fw, 126, "pipe: %v", err)
			return
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
		if err := cmd.Start(); err != nil {
			fail(st, fw, 126, "%s: %v", req.Path, err)
			return
		}
		inR.Close()
		outW.Close()
		errW.Close()
		stdin = inW
		outs = []io.Reader{outR, errR}
	}
	pgid := cmd.Process.Pid
	var exited atomic.Bool

	// Output pumps.
	var outWG sync.WaitGroup
	for i, r := range outs {
		dst := io.Writer(outSink)
		if i == 1 {
			dst = errSink
		}
		outWG.Add(1)
		go func(r io.Reader, dst io.Writer) {
			defer outWG.Done()
			io.Copy(dst, r) // pty master returns EIO when all slaves close
		}(r, dst)
	}

	// Input / control from the stub.
	go func() {
		for {
			typ, p, err := proto.ReadFrame(st)
			if err != nil {
				if !exited.Load() {
					syscall.Kill(-pgid, syscall.SIGKILL)
				}
				stdin.Close()
				return
			}
			switch typ {
			case proto.FStdin:
				stdin.Write(p)
			case proto.FStdinEOF:
				if master == nil {
					stdin.Close()
				}
			case proto.FSignal:
				if len(p) == 1 && p[0] > 0 && p[0] < 65 {
					syscall.Kill(-pgid, syscall.Signal(p[0]))
				}
			case proto.FResize:
				var rz proto.Resize
				if master != nil && proto.DecodeJSON(p, &rz) == nil && rz.Rows > 0 && rz.Cols > 0 {
					pty.Setsize(master, &pty.Winsize{Rows: rz.Rows, Cols: rz.Cols})
				}
			}
		}
	}()

	err := cmd.Wait()
	exited.Store(true)
	done := make(chan struct{})
	go func() { outWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(drainGrace):
		outSink.discard.Store(true)
		errSink.discard.Store(true)
	}
	ex := proto.Exit{}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				ex.Signal = int(ws.Signal())
				ex.Code = 128 + ex.Signal
			} else {
				ex.Code = ee.ExitCode()
			}
		} else {
			ex.Code = 126
		}
	}
	fw.WriteJSON(proto.FExit, ex)
	st.Close()
}
