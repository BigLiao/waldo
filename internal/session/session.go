package session

import (
	"bytes"
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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hashicorp/yamux"
	"golang.org/x/term"

	"waldo/internal/audit"
	"waldo/internal/config"
	"waldo/internal/paseo"
	"waldo/internal/profile"
	"waldo/internal/proto"
	"waldo/internal/sandbox"
	"waldo/internal/termfilter"
	"waldo/internal/transport"
	"waldo/internal/util"
)

// Options for one agent session.
type Options struct {
	Observe        bool
	Paseo          bool
	paseoBroker    *paseo.Broker
	readOnlyMounts []string
	Config         *config.Config
	Profile        profile.Profile
	Args           []string
	Host           string // overrides Config.Host
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
	var skillMounts []sandbox.ReadOnlyMount
	if o.Paseo {
		if o.Profile.MCPConfigFlag != "--mcp-config" || o.Profile.SkillDir == "" {
			warnf("此 Agent 尚未配置 Paseo MCP / skills 注入")
			return 2
		}
		// Discovery commands never open an interactive agent and have no
		// caller identity. Keep provider version/auth diagnostics operational.
		probe := len(o.Args) > 0 && (o.Args[0] == "--version" || o.Args[0] == "-v" || o.Args[0] == "version" || (o.Args[0] == "auth" && len(o.Args) > 1 && o.Args[1] == "status"))
		if !probe {
			var endpoint *paseo.Endpoint
			o.Args, endpoint, err = paseo.Configs(o.Args)
			if err != nil {
				warnf("%v", err)
				return 2
			}
			o.paseoBroker = paseo.NewBroker(endpoint)
			defer o.paseoBroker.Close()
			skills, err := paseo.Skills(home, util.ExpandHome(o.Profile.SkillDir, home))
			if err != nil {
				warnf("Paseo skills: %v", err)
				return 2
			}
			for _, skill := range skills {
				skillMounts = append(skillMounts, sandbox.ReadOnlyMount{Source: skill.Source, Target: skill.Target})
				o.readOnlyMounts = append(o.readOnlyMounts, skill.Target)
			}
		}
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
	// Runtime roots stay uncached even when selected as the entire project.
	if project == "/tmp" || project == proto.ContextDir {
		pol.MetadataCacheTTL = 0
		pol.ContentCacheTTL = 0
	}
	for _, shared := range o.Profile.Shared {
		if filepath.Clean(util.ExpandHome(shared, home)) == project {
			pol.MetadataCacheTTL = 0
			pol.ContentCacheTTL = 0
		}
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
	mounts = append(mounts, o.readOnlyMounts...)
	for _, s := range o.Profile.Shared {
		p := filepath.Clean(util.ExpandHome(s, home))
		if !filepath.IsAbs(p) || util.Within(p, project) || util.Within(p, "/tmp") {
			continue
		}
		mounts = append(mounts, p)
		mkdirs = append(mkdirs, p)
	}
	var guide []byte
	if pol.ContextEnabled {
		mounts = append(mounts, proto.ContextDir)
		guide = contextGuide(pol, host, o.Profile.Name, cwd, mounts)
	}

	al.Log("session", map[string]any{"agent": o.Profile.Name, "project": project, "host": host, "args": paseo.AuditArgs(o.Args), "paseo": o.paseoBroker != nil, "observe": o.Observe, "metadata_cache_ttl": pol.MetadataCacheTTL.String(), "content_cache_ttl": pol.ContentCacheTTL.String()})
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

	sb, err := StartSandbox(SandboxOptions{Home: home, Project: project, Policy: pol, Audit: al, DataDir: dataDir, Mkdirs: mkdirs, OnPorts: writeState, ContextGuide: guide, ReadOnlyMounts: skillMounts})
	if err != nil {
		warnf("%v", err)
		return 1
	}
	defer sb.Close()
	sb.Observe = o.Observe
	sb.ModelTelemetry = o.Profile.Telemetry

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
	conn, err := tr.OpenResumable()
	if err != nil {
		warnf("%v", err)
		return 255
	}
	defer conn.Close()
	bs, err := yamux.Client(conn.RW, util.ResumableYamuxConfig())
	if err != nil {
		warnf("%v", err)
		return 255
	}
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		acceptFromB(bs, sb, al, o.paseoBroker)
	}()
	defer func() {
		bs.Close()
		// Closing the link wakes the observers. Let final command/file/model
		// summaries reach the log before Run closes it; never wait on a peer.
		select {
		case <-acceptDone:
		case <-time.After(100 * time.Millisecond):
		}
	}()

	interactive := term.IsTerminal(0) && term.IsTerminal(1)
	hello := proto.Hello{
		Observe: o.Observe,
		Paseo:   o.paseoBroker != nil, ReadOnlyMounts: o.readOnlyMounts,
		Version: proto.FullVersion(), Session: id, Profile: o.Profile.Wire(), Argv: o.Args,
		Cwd: cwd, Project: project, Mounts: mounts, Facts: sb.Facts, Interactive: interactive,
		TimeUnixMs:              time.Now().UnixMilli(),
		MetadataCacheTTLSeconds: int64(sb.MetadataCacheTTL / time.Second),
		ContentCacheTTLSeconds:  int64(sb.ContentCacheTTL / time.Second),
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
	code := relayAgentIOCancelable(ios, interactive, &relayReconnect{
		cancel: func() bool {
			if conn.Reconnecting() {
				bs.Close()
				conn.Close()
				return true
			}
			return false
		},
		watch: conn.OnReconnect,
	})
	if conn.ReconnectExpired() {
		warnf("90 秒内未能恢复连接，本次会话已结束")
	}
	return code
}

// acceptFromB serves file projection, project-cache watches and exec streams. A fixed Paseo broker
// is accepted only when explicitly enabled for this session; arbitrary
// connection requests remain refused and logged.
func acceptFromB(bs *yamux.Session, sb *Sandbox, al *audit.Logger, brokers ...*paseo.Broker) {
	var handlers sync.WaitGroup
	defer handlers.Wait()
	var watchActive atomic.Bool
	var observeActive atomic.Bool
	var execSeq atomic.Uint64
	for {
		st, err := bs.AcceptStream()
		if err != nil {
			return
		}
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			st.SetReadDeadline(time.Now().Add(150 * time.Second))
			var h proto.Header
			err := proto.ReadMsg(st, &h)
			st.SetReadDeadline(time.Time{})
			if err != nil {
				st.Close()
				return
			}
			switch h.Kind {
			case proto.KindObserve:
				if !sb.Observe || h.Path != "" || h.Host != "" || h.Port != 0 || h.Exec != nil || !observeActive.CompareAndSwap(false, true) {
					al.Log("reject", map[string]any{"from": "B", "stream": h.Kind})
					st.Close()
					return
				}
				defer observeActive.Store(false)
				serveModelStats(st, al, sb.ModelTelemetry)
			case proto.KindFSWatch:
				if sb.ContentCacheTTL <= 0 || h.Path != sb.Project || h.Exec != nil || h.Host != "" || h.Port != 0 {
					al.Log("reject", map[string]any{"from": "B", "stream": h.Kind})
					st.Close()
					return
				}
				if !watchActive.CompareAndSwap(false, true) {
					al.Log("reject", map[string]any{"from": "B", "stream": h.Kind, "reason": "duplicate cache watch"})
					st.Close()
					return
				}
				defer watchActive.Store(false)
				in, err := sb.Open(proto.Header{Kind: proto.KindFSWatch, Path: sb.Project})
				if err != nil {
					st.Close()
					return
				}
				al.Log("fs-watch", map[string]any{"path": sb.Project})
				util.Splice(st, in)
			case proto.KindPaseo:
				if len(brokers) == 1 && brokers[0] != nil && h.Host == "" && h.Path == "" && h.Port == 0 && h.Exec == nil {
					al.Log("paseo-mcp", nil)
					brokers[0].Serve(st)
				} else {
					al.Log("reject", map[string]any{"from": "B", "stream": h.Kind})
					st.Close()
				}
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
				spliceObservedFS(st, in, sniff, al)
			case proto.KindExec:
				if h.Exec == nil {
					st.Close()
					return
				}
				id, start := execSeq.Add(1), time.Now()
				al.Log("exec", map[string]any{"id": id, "argv": h.Exec.Argv, "path": h.Exec.Path, "cwd": h.Exec.Cwd})
				in, err := sb.Open(proto.Header{Kind: proto.KindExec, Exec: h.Exec})
				if err != nil {
					logExecEnd(al, id, h.Exec.Path, start, time.Now(), "start_failed", nil)
					st.Close()
					return
				}
				spliceExec(st, in, al, id, h.Exec.Path, start)
			default:
				al.Log("reject", map[string]any{"from": "B", "stream": h.Kind})
				st.Close()
			}
		}()
	}
}

// relayAgentIO connects the user's terminal (or pipes) to the agent on B.
func relayAgentIO(st io.ReadWriteCloser, interactive bool) int {
	return relayAgentIOCancelable(st, interactive, nil)
}

// relayReconnect lets a resumable link cancel a pending reconnect and show
// its state without disturbing the agent's full-screen UI.
type relayReconnect struct {
	cancel func() bool
	watch  func(func(up bool)) func()
}

func relayAgentIOCancelable(st io.ReadWriteCloser, interactive bool, rc *relayReconnect) int {
	var cancelReconnect func() bool
	if rc != nil {
		cancelReconnect = rc.cancel
	}
	fw := proto.NewFrameWriter(st)
	var outMu sync.Mutex // agent output and reconnect notices share the terminal
	restoreTerminal := func() {}
	if interactive {
		old, err := term.MakeRaw(0)
		restoreTerminal = sync.OnceFunc(func() {
			resetAgentTerminal(os.Stdout)
			if err == nil {
				term.Restore(0, old)
			}
		})
		defer restoreTerminal()
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
			if cancelReconnect != nil && cancelReconnect() {
				return
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
				if n > 0 && bytes.IndexByte(buf[:n], 3) >= 0 && cancelReconnect != nil && cancelReconnect() {
					return
				}
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
	if interactive && rc != nil && rc.watch != nil && term.IsTerminal(1) {
		defer rc.watch(func(up bool) {
			outMu.Lock()
			defer outMu.Unlock()
			cols, rows, err := term.GetSize(1)
			if err != nil {
				return
			}
			io.WriteString(os.Stdout, reconnectNotice(rows, cols, up))
			if up {
				// The notice overwrote part of the agent's screen; a size
				// change makes it repaint everything.
				if rows > 1 {
					fw.WriteJSON(proto.FResize, proto.Resize{Rows: uint16(rows - 1), Cols: uint16(cols)})
				}
				fw.WriteJSON(proto.FResize, proto.Resize{Rows: uint16(rows), Cols: uint16(cols)})
			}
		})()
	}
	if term.IsTerminal(2) {
		errw = termfilter.New(os.Stderr)
	}
	for {
		typ, p, err := proto.ReadFrame(st)
		if err != nil {
			restoreTerminal()
			if interactive {
				os.Stdout.WriteString("\r\n")
			}
			warnf("与 B 的连接中断")
			return 255
		}
		switch typ {
		case proto.FStdout:
			outMu.Lock()
			out.Write(p)
			outMu.Unlock()
		case proto.FStderr, proto.FError:
			errw.Write(p)
		case proto.FExit:
			var ex proto.Exit
			proto.DecodeJSON(p, &ex)
			return ex.Code
		}
	}
}
