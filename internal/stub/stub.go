// Package stub is the form the waldo binary takes inside B's capsule when it
// is started under any other name (bash, git, rg, ...). It forwards that one
// process start to A and relays stdio, signals and the exit status. It never
// executes anything on B.
package stub

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"waldo/internal/proto"
)

// Paths inside the capsule.
const (
	Exe    = "/run/waldo/waldo"
	Socket = "/run/waldo/s/sock"
)

func die(code int, format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "waldo: "+format+"\n", a...)
	return code
}

// Main runs the stub and returns the exit code.
func Main() int {
	argv := os.Args
	path := identity(argv)
	if path == "" {
		return die(127, "%s: 无法确定要转发的程序", argv[0])
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = os.Getenv("PWD")
	}
	conn, err := net.Dial("unix", Socket)
	if err != nil {
		return die(127, "%s: 与 A 的连接不可用，拒绝在本地执行（%v）", filepath.Base(path), err)
	}
	defer conn.Close()
	return Relay(conn, &proto.ExecReq{Path: path, Argv: argv, Cwd: cwd, Env: os.Environ()})
}

// Relay sends req over conn and then mirrors the remote process locally:
// stdio, signals, window size and exit status. req.TTY and the window size
// are filled in from the local stdio.
func Relay(conn io.ReadWriter, req *proto.ExecReq) int {
	path := req.Path
	tty := term.IsTerminal(0) && term.IsTerminal(1)
	req.TTY = tty
	if tty {
		if c, r, err := term.GetSize(1); err == nil {
			req.Cols, req.Rows = uint16(c), uint16(r)
		}
	}
	if err := proto.WriteMsg(conn, proto.Header{Kind: proto.KindExec, Exec: req}); err != nil {
		return die(127, "%v", err)
	}
	fw := proto.NewFrameWriter(conn)

	var restore func()
	if tty {
		// Like an ssh client: the local tty goes raw, the line discipline
		// that matters is the pty the process gets on A.
		if st, err := term.MakeRaw(0); err == nil {
			restore = func() { term.Restore(0, st) }
		}
	}
	cleanup := func() {
		if restore != nil {
			restore()
		}
	}

	sigs := make(chan os.Signal, 16)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT,
		syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGWINCH, syscall.SIGALRM)
	go func() {
		for s := range sigs {
			sig := s.(syscall.Signal)
			if sig == syscall.SIGWINCH {
				if tty {
					if c, r, err := term.GetSize(1); err == nil {
						fw.WriteJSON(proto.FResize, proto.Resize{Rows: uint16(r), Cols: uint16(c)})
					}
				}
				continue
			}
			fw.Write(proto.FSignal, []byte{byte(sig)})
		}
	}()

	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if fw.Write(proto.FStdin, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				fw.Write(proto.FStdinEOF, nil)
				return
			}
		}
	}()

	for {
		typ, p, err := proto.ReadFrame(conn)
		if err != nil {
			cleanup()
			return die(255, "%s: 与 A 的连接中断", filepath.Base(path))
		}
		switch typ {
		case proto.FStdout:
			os.Stdout.Write(p)
		case proto.FStderr, proto.FError:
			os.Stderr.Write(p)
		case proto.FExit:
			cleanup()
			var ex proto.Exit
			proto.DecodeJSON(p, &ex)
			if ex.Signal > 0 {
				sig := syscall.Signal(ex.Signal)
				signal.Reset(sig)
				syscall.Kill(os.Getpid(), sig)
				time.Sleep(100 * time.Millisecond)
			}
			return ex.Code
		}
	}
}

// identity works out which program this stub stands in for.
//
// AT_EXECFN is the path the kernel was asked to execute. If that path is a
// stub symlink, it is the program. Otherwise this is an interpreter started
// for a "#!" script, and argv[0] holds the interpreter path from the script.
func identity(argv []string) string {
	cwd, _ := os.Getwd()
	abs := func(p string) string {
		if filepath.IsAbs(p) {
			return filepath.Clean(p)
		}
		return filepath.Join(cwd, p)
	}
	if fn := execFn(); fn != "" {
		p := abs(fn)
		if t, err := os.Readlink(p); err == nil && t == Exe {
			return p
		}
	}
	if len(argv) == 0 {
		return ""
	}
	if strings.Contains(argv[0], "/") {
		return abs(argv[0])
	}
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		p := filepath.Join(d, argv[0])
		if t, err := os.Readlink(p); err == nil && t == Exe {
			return p
		}
	}
	return ""
}

const atExecFn = 31 // AT_EXECFN from <elf.h>

// execFn reads AT_EXECFN from the auxiliary vector.
func execFn() string {
	aux, err := os.ReadFile("/proc/self/auxv")
	if err != nil {
		return ""
	}
	var addr uint64
	for i := 0; i+16 <= len(aux); i += 16 {
		k := binary.NativeEndian.Uint64(aux[i:])
		v := binary.NativeEndian.Uint64(aux[i+8:])
		if k == atExecFn {
			addr = v
			break
		}
		if k == 0 {
			break
		}
	}
	if addr == 0 {
		return ""
	}
	f, err := os.Open("/proc/self/mem")
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 4096)
	n, _ := f.ReadAt(buf, int64(addr))
	if i := strings.IndexByte(string(buf[:n]), 0); i >= 0 {
		return string(buf[:i])
	}
	return ""
}
