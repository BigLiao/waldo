// Package transport connects A to B. The interface is "one bidirectional
// byte stream"; v1 implements it with the system ssh client, so
// authentication, encryption, jump hosts and NAT traversal all come from the
// user's ~/.ssh/config.
package transport

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"golang.org/x/term"

	"waldo/internal/proto"
	"waldo/internal/termfilter"
	"waldo/internal/util"
)

// Transport opens the byte stream to `waldo host`.
type Transport interface {
	Open(hostArgs []string) (*Conn, error)
}

// Conn is a running link to B.
type Conn struct {
	RW           io.ReadWriteCloser
	cmd          *exec.Cmd
	closeOnce    sync.Once
	closeFunc    func()
	reconnecting func() bool
}

// Close terminates the link.
func (c *Conn) Close() {
	c.closeOnce.Do(func() {
		if c.closeFunc != nil {
			c.closeFunc()
			return
		}
		c.RW.Close()
		if c.cmd != nil && c.cmd.Process != nil {
			c.cmd.Process.Kill()
			c.cmd.Wait()
		}
	})
}

func (c *Conn) Reconnecting() bool { return c.reconnecting != nil && c.reconnecting() }

// SSH is the ssh(1) based transport.
type SSH struct {
	Host      string
	ExtraArgs []string // from config ssh_args and $WALDO_SSH_ARGS
	RemoteDir string   // data dir on B, may start with "~/"
	Stderr    io.Writer
}

// HardeningArgs make sure B cannot use this connection to reach back into A:
// no agent, X11 or port forwarding, no tty, no escape character.
var HardeningArgs = []string{
	"-T", "-e", "none",
	"-o", "ForwardAgent=no",
	"-o", "ForwardX11=no",
	"-o", "ForwardX11Trusted=no",
	"-o", "ClearAllForwardings=yes",
	"-o", "PermitLocalCommand=no",
	"-o", "Tunnel=no",
	"-o", "GSSAPIDelegateCredentials=no",
	"-o", "RequestTTY=no",
	"-o", "RemoteCommand=none",
	"-o", "SessionType=default",
	"-o", "BatchMode=no",
}

// Args returns the ssh argument list for running remoteCmd on the host.
func (s *SSH) Args(remoteCmd string) []string {
	return s.args(remoteCmd, false)
}

func (s *SSH) args(remoteCmd string, tty bool) []string {
	a := append([]string{}, s.ExtraArgs...)
	for i := 0; i < len(HardeningArgs); i++ {
		arg := HardeningArgs[i]
		if tty && arg == "-T" {
			a = append(a, "-tt")
			continue
		}
		if tty && arg == "-o" && i+1 < len(HardeningArgs) && HardeningArgs[i+1] == "RequestTTY=no" {
			i++
			continue
		}
		a = append(a, arg)
	}
	a = append(a, "--", s.Host, remoteCmd)
	return a
}

// RunInteractive runs initialization outside the protocol connection. The
// command travels as an SSH argument, leaving stdin available for sudo. Only
// an actual local terminal requests a remote tty; unattended setup stays in
// pipe mode so the installer can fail promptly when sudo needs a password.
func (s *SSH) RunInteractive(remoteCmd string, stdin *os.File, stdout, stderr io.Writer) error {
	tty := stdin != nil && term.IsTerminal(int(stdin.Fd()))
	cmd := exec.Command("ssh", s.args(remoteCmd, tty)...)
	cmd.Stdin = stdin
	cmd.Stdout = termfilter.New(stdout)
	cmd.Stderr = termfilter.New(stderr)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("远端初始化 %s 失败: %w", termfilter.Sanitize(s.Host), err)
	}
	return nil
}

// remoteDirExpr renders RemoteDir as a shell expression on B.
func (s *SSH) remoteDirExpr() string {
	d := s.RemoteDir
	if d == "" {
		d = "~/.local/share/waldo"
	}
	if d == "~" {
		return `"$HOME"`
	}
	if rest, ok := strings.CutPrefix(d, "~/"); ok {
		return `"$HOME"/` + util.ShellQuote(rest)
	}
	return util.ShellQuote(d)
}

// wrap runs script with sh on B regardless of the user's login shell.
func wrap(script string) string { return "sh -c " + util.ShellQuote(script) }

func (s *SSH) command(remoteCmd string) *exec.Cmd {
	cmd := exec.Command("ssh", s.Args(remoteCmd)...)
	cmd.Stderr = s.Stderr
	if cmd.Stderr == nil {
		// Everything B writes to stderr reaches the user's terminal.
		cmd.Stderr = termfilter.New(os.Stderr)
	}
	return cmd
}

// bootstrap runs waldo host if the right version is installed, otherwise
// reports what to install.
func (s *SSH) bootstrap(sub string) string {
	return wrap(fmt.Sprintf(`D=%s; B="$D/bin/waldo"; if [ -x "$B" ] && [ "$("$B" version 2>/dev/null)" = %s ]; then exec "$B" host --data-dir "$D" %s; fi; echo "%s $(uname -s) $(uname -m)"`,
		s.remoteDirExpr(), util.ShellQuote(proto.FullVersion()), sub, proto.NeedInstall))
}

// Open starts `waldo host` on B, installing the binary first if needed.
func (s *SSH) Open(hostArgs []string) (*Conn, error) {
	var sub []string
	for _, a := range hostArgs {
		sub = append(sub, util.ShellQuote(a))
	}
	for attempt := 0; attempt < 2; attempt++ {
		cmd := s.command(s.bootstrap(strings.Join(sub, " ")))
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("启动 ssh 失败: %w", err)
		}
		br := bufio.NewReaderSize(stdout, 64<<10)
		line, err := readLine(br)
		if err != nil {
			stdin.Close()
			werr := cmd.Wait()
			return nil, fmt.Errorf("连接 %s 失败（ssh: %v）", s.Host, errors.Join(err, werr))
		}
		if strings.HasPrefix(line, proto.BannerPrefix) {
			return &Conn{RW: util.RWC{R: br, W: stdin, C: []io.Closer{stdin}}, cmd: cmd}, nil
		}
		stdin.Close()
		cmd.Wait()
		if !strings.HasPrefix(line, proto.NeedInstall) || attempt > 0 {
			return nil, fmt.Errorf("B 上的 waldo 无法启动: %q", line)
		}
		f := strings.Fields(line)
		if len(f) < 3 {
			return nil, fmt.Errorf("unexpected bootstrap reply %q", line)
		}
		if err := s.Install(f[1], f[2]); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("unreachable")
}

func readLine(br *bufio.Reader) (string, error) {
	var b []byte
	for len(b) < 512 {
		c, err := br.ReadByte()
		if err != nil {
			return string(b), err
		}
		if c == '\n' {
			return string(b), nil
		}
		b = append(b, c)
	}
	return string(b), errors.New("bootstrap line too long")
}

func goArch(unameM string) string {
	switch unameM {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	case "armv7l", "armv6l":
		return "arm"
	case "riscv64":
		return "riscv64"
	}
	return unameM
}

// binaryFor finds a waldo binary for B's platform.
func binaryFor(unameS, unameM string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	goos, arch := strings.ToLower(unameS), goArch(unameM)
	if goos == runtime.GOOS && arch == runtime.GOARCH {
		return exe, nil
	}
	cand := filepath.Join(filepath.Dir(exe), fmt.Sprintf("waldo-%s-%s", goos, arch))
	if _, err := os.Stat(cand); err == nil {
		return cand, nil
	}
	return "", fmt.Errorf("B 是 %s/%s，找不到对应的二进制 %s", goos, arch, cand)
}

// Install copies the matching binary to B.
func (s *SSH) Install(unameS, unameM string) error {
	src, err := binaryFor(unameS, unameM)
	if err != nil {
		return err
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	script := fmt.Sprintf(`set -e; D=%s; mkdir -p "$D/bin"; T="$D/bin/.waldo.$$"; cat > "$T"; chmod 755 "$T"; mv -f "$T" "$D/bin/waldo"; "$D/bin/waldo" version`, s.remoteDirExpr())
	cmd := s.command(wrap(script))
	cmd.Stdin = f
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("安装 waldo 到 %s 失败: %w", s.Host, err)
	}
	if got := strings.TrimSpace(out.String()); got != proto.FullVersion() && src == mustExe() {
		return fmt.Errorf("安装后版本不一致: %q", got)
	}
	return nil
}

func mustExe() string { e, _ := os.Executable(); return e }

// Run runs `waldo <args>` on B (installing first if needed) and returns
// stdout. Used by doctor/setup.
func (s *SSH) Run(args []string, stdin []byte) ([]byte, error) {
	var q []string
	for _, a := range args {
		q = append(q, util.ShellQuote(a))
	}
	for attempt := 0; attempt < 2; attempt++ {
		script := fmt.Sprintf(`D=%s; B="$D/bin/waldo"; if [ -x "$B" ] && [ "$("$B" version 2>/dev/null)" = %s ]; then echo OK; exec "$B" %s; fi; echo "%s $(uname -s) $(uname -m)"`,
			s.remoteDirExpr(), util.ShellQuote(proto.FullVersion()), strings.ReplaceAll(strings.Join(q, " "), "@DATA@", `"$D"`), proto.NeedInstall)
		cmd := s.command(wrap(script))
		cmd.Stdin = bytes.NewReader(stdin)
		out, err := cmd.Output()
		line, rest, _ := strings.Cut(string(out), "\n")
		if strings.HasPrefix(line, proto.NeedInstall) && attempt == 0 {
			f := strings.Fields(line)
			if len(f) < 3 {
				return nil, fmt.Errorf("unexpected reply %q", line)
			}
			if err := s.Install(f[1], f[2]); err != nil {
				return nil, err
			}
			continue
		}
		if line != "OK" {
			if err != nil {
				return nil, fmt.Errorf("ssh %s: %w", s.Host, err)
			}
			return nil, fmt.Errorf("unexpected reply %q", line)
		}
		return []byte(rest), err
	}
	return nil, errors.New("unreachable")
}
