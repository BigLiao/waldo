package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hashicorp/yamux"
	"golang.org/x/term"

	"waldo/internal/audit"
	"waldo/internal/config"
	"waldo/internal/profile"
	"waldo/internal/proto"
	"waldo/internal/sandbox"
	"waldo/internal/termfilter"
	"waldo/internal/transport"
	"waldo/internal/util"
)

// Options for one agent session.
type Options struct {
	Config  *config.Config
	Profile profile.Profile
	Args    []string
	Host    string // overrides Config.Host
}

func warnf(format string, a ...any) { fmt.Fprintf(os.Stderr, "waldo: "+format+"\n", a...) }

// ProjectDir returns the git top-level of cwd, or cwd itself.
func ProjectDir(cwd string) string {
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err == nil {
		p := strings.TrimSpace(string(out))
		if filepath.IsAbs(p) {
			// git resolves symlinks; keep the user's spelling if cwd is inside.
			if rcwd, err := filepath.EvalSymlinks(cwd); err == nil && util.Within(rcwd, p) {
				rel, _ := filepath.Rel(p, rcwd)
				if rel == "." {
					return cwd
				}
				if base, ok := strings.CutSuffix(cwd, "/"+rel); ok {
					return base
				}
			}
			return p
		}
	}
	return cwd
}

// CheckProject refuses directories that would expose too much.
func CheckProject(project, home string) error {
	if os.Getenv("WALDO_ALLOW_ANY_DIR") == "1" {
		return nil
	}
	if project == "/" || project == home || util.Within(home, project) {
		return fmt.Errorf("项目目录 %s 太宽（会把整个家目录交给沙箱）。请 cd 到具体项目目录再运行", project)
	}
	return nil
}

// SSHTransport builds the configured transport.
func SSHTransport(cfg *config.Config, host string) *transport.SSH {
	extra := append([]string{}, cfg.SSHArgs...)
	extra = append(extra, strings.Fields(os.Getenv("WALDO_SSH_ARGS"))...)
	return &transport.SSH{Host: host, ExtraArgs: extra, RemoteDir: cfg.RemoteDirOrDefault()}
}

type runState struct {
	PID     int    `json:"pid"`
	Session string `json:"session"`
	Agent   string `json:"agent"`
	Project string `json:"project"`
	Host    string `json:"host"`
	Ports   []int  `json:"ports"`
}

// RunDir holds one state file per live session (for `waldo ports`).
func RunDir() string { return filepath.Join(config.DataDir(), "run") }

// Run runs one agent session and returns the exit code.
func Run(o Options) int {
	host := o.Host
	if host == "" {
		host = o.Config.Host
	}
	if host == "" {
		warnf("还没有配置服务器。先运行：waldo setup <user@host>")
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		warnf("%v", err)
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		warnf("%v", err)
		return 1
	}
	project := ProjectDir(cwd)
	if err := CheckProject(project, home); err != nil {
		warnf("%v", err)
		return 2
	}
	pol, err := o.Config.PolicyFor(project)
	if err != nil {
		warnf("%v", err)
		return 2
	}
	dataDir := config.DataDir()
	id := newID()
	al, err := audit.Open(dataDir, id)
	if err != nil {
		warnf("审计日志: %v", err)
		return 1
	}
	defer al.Close()

	// What B sees through the file projection.
	mounts := []string{project}
	if !util.Within(project, "/tmp") {
		mounts = append(mounts, "/tmp")
	}
	var mkdirs []string
	for _, s := range o.Profile.Shared {
		p := filepath.Clean(util.ExpandHome(s, home))
		if !filepath.IsAbs(p) || util.Within(p, project) || util.Within(p, "/tmp") {
			continue
		}
		mounts = append(mounts, p)
		mkdirs = append(mkdirs, p)
	}

	al.Log("session", map[string]any{"agent": o.Profile.Name, "project": project, "host": host, "args": o.Args})
	snap := sandbox.TakeSnapshot(project)

	state := runState{PID: os.Getpid(), Session: id, Agent: o.Profile.Name, Project: project, Host: host}
	statePath := filepath.Join(RunDir(), fmt.Sprintf("%d.json", os.Getpid()))
	var stMu sync.Mutex
	writeState := func(ports []int) {
		stMu.Lock()
		defer stMu.Unlock()
		state.Ports = ports
		os.MkdirAll(RunDir(), 0o700)
		b, _ := json.Marshal(state)
		os.WriteFile(statePath, b, 0o600)
	}
	writeState(nil)
	defer os.Remove(statePath)

	sb, err := StartSandbox(SandboxOptions{Home: home, Project: project, Policy: pol, Audit: al, DataDir: dataDir, Mkdirs: mkdirs, OnPorts: writeState})
	if err != nil {
		warnf("%v", err)
		return 1
	}
	defer sb.Close()

	code := runRemote(o, host, sb, id, cwd, project, mounts, al)

	reportProtected(snap, al)
	al.Log("session-end", map[string]any{"code": code})
	return code
}

// reportProtected warns about protected paths that changed even though they
// could not be mounted read-only (because they did not exist yet).
func reportProtected(snap sandbox.Snapshot, al *audit.Logger) {
	if changed := snap.Changed(); len(changed) > 0 {
		warnf("注意：会话期间以下受保护路径被创建、删除或替换，在宿主机上使用前请先检查：")
		for _, p := range changed {
			fmt.Fprintf(os.Stderr, "    %s\n", p)
		}
		al.Log("protected-changed", map[string]any{"paths": changed})
	}
}

func runRemote(o Options, host string, sb *Sandbox, id, cwd, project string, mounts []string, al *audit.Logger) int {
	tr := SSHTransport(o.Config, host)
	conn, err := tr.Open(nil)
	if err != nil {
		warnf("%v", err)
		return 255
	}
	defer conn.Close()
	bs, err := yamux.Client(conn.RW, util.YamuxConfig())
	if err != nil {
		warnf("%v", err)
		return 255
	}
	defer bs.Close()
	go acceptFromB(bs, sb, al)

	interactive := term.IsTerminal(0) && term.IsTerminal(1)
	hello := proto.Hello{
		Version: proto.FullVersion(), Session: id, Profile: o.Profile.Wire(), Argv: o.Args,
		Cwd: cwd, Project: project, Mounts: mounts, Facts: sb.Facts, Interactive: interactive,
		TimeUnixMs: time.Now().UnixMilli(),
	}
	if interactive {
		if c, r, err := term.GetSize(1); err == nil {
			hello.Cols, hello.Rows = uint16(c), uint16(r)
		}
	}
	ctl, err := bs.OpenStream()
	if err != nil {
		warnf("%v", err)
		return 255
	}
	if err := errors.Join(proto.WriteMsg(ctl, proto.Header{Kind: proto.KindCtl}), proto.WriteMsg(ctl, hello)); err != nil {
		warnf("%v", err)
		return 255
	}
	var ack proto.HelloAck
	if err := proto.ReadMsg(ctl, &ack); err != nil {
		warnf("与 B 握手失败: %v", err)
		return 255
	}
	if ack.Error != "" {
		warnf("B: %s", termfilter.Sanitize(ack.Error))
		return 1
	}
	if skew := ack.SkewMs; skew > 2000 || skew < -2000 {
		warnf("A 与 B 的时钟相差约 %.1f 秒，Agent 可能误报“文件已被修改”，建议两端启用 NTP", float64(skew)/1000)
	}
	ios, err := bs.OpenStream()
	if err != nil {
		warnf("%v", err)
		return 255
	}
	if err := proto.WriteMsg(ios, proto.Header{Kind: proto.KindIO}); err != nil {
		warnf("%v", err)
		return 255
	}
	return relayAgentIO(ios, interactive)
}

// acceptFromB serves the streams B opens: only file projection and exec
// forwarding are accepted; anything else is refused and logged.
func acceptFromB(bs *yamux.Session, sb *Sandbox, al *audit.Logger) {
	for {
		st, err := bs.AcceptStream()
		if err != nil {
			return
		}
		go func() {
			st.SetReadDeadline(time.Now().Add(30 * time.Second))
			var h proto.Header
			err := proto.ReadMsg(st, &h)
			st.SetReadDeadline(time.Time{})
			if err != nil {
				st.Close()
				return
			}
			switch h.Kind {
			case proto.KindFS:
				if !filepath.IsAbs(h.Path) {
					st.Close()
					return
				}
				al.Log("mount", map[string]any{"path": h.Path})
				in, err := sb.Open(proto.Header{Kind: proto.KindFS, Path: h.Path})
				if err != nil {
					st.Close()
					return
				}
				sniff := audit.NewSFTPSniffer(func(op, path, to string) {
					f := map[string]any{"path": path}
					if to != "" {
						f["to"] = to
					}
					al.Log(op, f)
				})
				spliceTee(st, in, sniff)
			case proto.KindExec:
				if h.Exec == nil {
					st.Close()
					return
				}
				al.Log("exec", map[string]any{"argv": h.Exec.Argv, "path": h.Exec.Path, "cwd": h.Exec.Cwd})
				in, err := sb.Open(proto.Header{Kind: proto.KindExec, Exec: h.Exec})
				if err != nil {
					st.Close()
					return
				}
				util.Splice(st, in)
			default:
				al.Log("reject", map[string]any{"from": "B", "stream": h.Kind})
				st.Close()
			}
		}()
	}
}

// spliceTee is Splice with the B->A direction also fed to w.
func spliceTee(b, a io.ReadWriteCloser, w io.Writer) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(a, io.TeeReader(b, w)); a.Close() }()
	go func() { defer wg.Done(); io.Copy(b, a); b.Close() }()
	wg.Wait()
}

// relayAgentIO connects the user's terminal (or pipes) to the agent on B.
func relayAgentIO(st io.ReadWriteCloser, interactive bool) int {
	fw := proto.NewFrameWriter(st)
	if interactive {
		old, err := term.MakeRaw(0)
		if err == nil {
			defer term.Restore(0, old)
		}
	}
	sigs := make(chan os.Signal, 16)
	if interactive {
		signal.Notify(sigs, syscall.SIGWINCH, syscall.SIGTERM, syscall.SIGHUP)
	} else {
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2)
	}
	defer signal.Stop(sigs)
	go func() {
		for s := range sigs {
			sig := s.(syscall.Signal)
			if sig == syscall.SIGWINCH {
				if c, r, err := term.GetSize(1); err == nil {
					fw.WriteJSON(proto.FResize, proto.Resize{Rows: uint16(r), Cols: uint16(c)})
				}
				continue
			}
			fw.Write(proto.FSignal, []byte{byte(sig)})
		}
	}()
	if !interactive && term.IsTerminal(0) {
		// stdout is redirected but stdin is a terminal: the agent gets an
		// empty stdin rather than a pipe that never ends.
		fw.Write(proto.FStdinEOF, nil)
	} else {
		go func() {
			buf := make([]byte, 32<<10)
			for {
				n, err := os.Stdin.Read(buf)
				if n > 0 && fw.Write(proto.FStdin, buf[:n]) != nil {
					return
				}
				if err != nil {
					fw.Write(proto.FStdinEOF, nil)
					return
				}
			}
		}()
	}
	var out, errw io.Writer = os.Stdout, os.Stderr
	if term.IsTerminal(1) {
		out = termfilter.New(os.Stdout)
	}
	if term.IsTerminal(2) {
		errw = termfilter.New(os.Stderr)
	}
	for {
		typ, p, err := proto.ReadFrame(st)
		if err != nil {
			if interactive {
				os.Stdout.WriteString("\r\n")
			}
			warnf("与 B 的连接中断")
			return 255
		}
		switch typ {
		case proto.FStdout:
			out.Write(p)
		case proto.FStderr, proto.FError:
			errw.Write(p)
		case proto.FExit:
			var ex proto.Exit
			proto.DecodeJSON(p, &ex)
			return ex.Code
		}
	}
}
