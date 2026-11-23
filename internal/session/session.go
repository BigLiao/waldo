package session

import (
	"bytes"
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
	"waldo/internal/hostbash"
	"waldo/internal/hosttools"
	"waldo/internal/paseo"
	"waldo/internal/profile"
	"waldo/internal/proto"
	"waldo/internal/sandbox"
	"waldo/internal/termfilter"
	"waldo/internal/transport"
	"waldo/internal/util"
	"waldo/internal/wsl"
)

// Options for one agent session.
type Options struct {
	LocalSandbox    *bool  // nil inherits global/project configuration
	ExecMode        string // empty inherits global/project configuration
	Observe         *bool  // nil inherits global/project configuration
	Paseo           bool
	paseoBroker     *paseo.Broker
	startup         *startupStream
	readOnlyMounts  []string
	Config          *config.Config
	Profile         profile.Profile
	Args            []string
	Host            string // overrides Config.Host
	Remote          string // empty selects Config.DefaultRemote, then legacy Host
	route           *sessionRoute
	connectionState func(string)
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
		return fmt.Errorf("项目目录 %s 太宽（会将整个家目录投影给远端）。请 cd 到具体项目目录再运行", project)
	}
	return nil
}

// SSHTransport builds the configured transport.
func SSHTransport(cfg *config.Config, host string) *transport.SSH {
	extra := append([]string{}, cfg.SSHArgs...)
	extra = append(extra, strings.Fields(os.Getenv("WALDO_SSH_ARGS"))...)
	return &transport.SSH{Host: host, ExtraArgs: extra, RemoteDir: cfg.RemoteDirOrDefault()}
}

// RunState describes a local client and its fixed remote target.
type RunState struct {
	PID        int    `json:"pid"`
	Session    string `json:"session"`
	Agent      string `json:"agent"`
	Project    string `json:"project"`
	Host       string `json:"host"`
	Remote     string `json:"remote,omitempty"`
	Ports      []int  `json:"ports"`
	Connection string `json:"connection,omitempty"`
}

// RunDir holds one state file per live session (for `waldo ports`).
func RunDir() string { return filepath.Join(config.DataDir(), "run") }

// Run runs one agent session and returns the exit code.
func Run(o Options) int {
	// Resolve once: reconnects and all forwarded channels use this same copy.
	if o.Host != "" {
		source := *o.Config
		source.Host, source.DefaultRemote = o.Host, ""
		o.Config = &source
	}
	cfg, remote, err := o.Config.ResolveRemote(o.Remote)
	if err != nil {
		warnf("%v", err)
		return 2
	}
	o.Config, o.Remote = cfg, remote
	if o.Host != "" {
		cfg.Host = o.Host
	}
	host := cfg.Host
	if startupEnabled(o) {
		o.startup = newStartupStream(os.Stdout)
		defer o.startup.Close()
	}
	home, err := os.UserHomeDir()
	if err != nil {
		warnf("%v", err)
		return 1
	}
	var skillMounts []sandbox.ReadOnlyMount
	paseoIdentity := ""
	probe := hostbash.IsProbe(o.Args)
	if o.Paseo {
		if o.Profile.MCPConfigFlag != "--mcp-config" || o.Profile.SkillDir == "" {
			warnf("此 Agent 尚未配置 Paseo MCP / skills 注入")
			return 2
		}
		// Discovery commands never open an interactive agent and have no
		// caller identity. Keep provider version/auth diagnostics operational.
		if !probe {
			var endpoint *paseo.Endpoint
			o.Args, endpoint, err = paseo.Configs(o.Args)
			if err != nil {
				warnf("%v", err)
				return 2
			}
			o.paseoBroker = paseo.NewBroker(endpoint)
			defer o.paseoBroker.Close()
			paseoIdentity = "paseo:" + endpoint.URL.Query().Get("callerAgentId")
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
	if !probe && (o.Profile.Telemetry == "claude" || paseoIdentity != "") {
		o.route = newSessionRoute(cfg, remote)
		var nativeArgs []string
		if o.Profile.Telemetry == "claude" {
			nativeArgs = append(append([]string(nil), o.Profile.Args...), o.Args...)
		}
		if err := o.route.prepare(paseoIdentity, nativeArgs); err != nil {
			warnf("%v", err)
			return 2
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
	if o.Observe != nil {
		pol.ObserveEnabled = *o.Observe
	}
	if o.LocalSandbox != nil {
		pol.SandboxEnabled = *o.LocalSandbox
	}
	// Discovery subcommands neither call tools nor models and can reject the
	// session-only settings/telemetry arguments used by the observers.
	if probe {
		pol.ObserveEnabled = false
	}
	if o.ExecMode != "" {
		pol.ExecMode = o.ExecMode
	}
	if pol.ExecMode == "" {
		pol.ExecMode = config.DefaultExecMode(o.Profile.MCPConfigFlag == "--mcp-config")
	}
	if err := config.ValidateExecMode(pol.ExecMode); err != nil {
		warnf("%v", err)
		return 2
	}
	if pol.ExecMode != proto.ExecHost && o.Profile.MCPConfigFlag != "--mcp-config" {
		warnf("--exec-mode remote/host-mcp 需要支持 --mcp-config 的 Agent profile（内置 Claude 支持）")
		return 2
	}
	o.ExecMode = pol.ExecMode
	if o.ExecMode != proto.ExecHost {
		args := append(append([]string(nil), o.Profile.Args...), o.Args...)
		o.Args, err = hostbash.Configs(args)
		if err != nil {
			warnf("%v", err)
			return 2
		}
		o.Profile.Args = nil
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
	al, err := audit.OpenRemote(dataDir, id, remote, host)
	if err != nil {
		warnf("审计日志: %v", err)
		return 1
	}
	defer al.Close()

	// What B sees through the file projection.
	mounts := []string{project}
	if pol.ExecMode != proto.ExecHostMCP && (!util.Within(project, "/tmp") || !pol.SandboxEnabled && project != "/tmp") {
		mounts = append(mounts, "/tmp")
	}
	var mkdirs []string
	mounts = append(mounts, o.readOnlyMounts...)
	for _, s := range o.Profile.Shared {
		p := filepath.Clean(util.ExpandHome(s, home))
		if !filepath.IsAbs(p) || util.Within(p, project) || (pol.ExecMode != proto.ExecHostMCP && util.Within(p, "/tmp")) {
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

	al.Log("session", map[string]any{"agent": o.Profile.Name, "project": project, "host": host, "args": paseo.AuditArgs(o.Args), "paseo": o.paseoBroker != nil, "exec_mode": o.ExecMode, "local_sandbox": pol.SandboxEnabled, "observe": pol.ObserveEnabled, "metadata_cache_ttl": pol.MetadataCacheTTL.String(), "content_cache_ttl": pol.ContentCacheTTL.String()})
	var snap sandbox.Snapshot
	if pol.SandboxEnabled {
		snap = sandbox.TakeSnapshot(project)
	} else if !probe {
		warnf("本机沙箱已关闭，远端可使用本机当前用户的文件权限和网络")
	}

	state := RunState{PID: os.Getpid(), Session: id, Agent: o.Profile.Name, Project: project, Host: host, Remote: remote, Connection: "starting"}
	statePath := filepath.Join(RunDir(), fmt.Sprintf("%d.json", os.Getpid()))
	var stMu sync.Mutex
	writeState := func(ports []int) {
		stMu.Lock()
		defer stMu.Unlock()
		state.Ports = ports
		os.MkdirAll(RunDir(), 0o700)
		writeRunState(statePath, state)
	}
	o.connectionState = func(status string) {
		stMu.Lock()
		defer stMu.Unlock()
		state.Connection = status
		writeRunState(statePath, state)
	}
	writeState(nil)
	defer os.Remove(statePath)

	sb, err := StartSandbox(SandboxOptions{Home: home, Project: project, Policy: pol, Audit: al, DataDir: dataDir, Mkdirs: mkdirs, OnPorts: writeState, ContextGuide: guide, ReadOnlyMounts: skillMounts, Unsandboxed: !pol.SandboxEnabled})
	if err != nil {
		warnf("%v", err)
		return 1
	}
	defer sb.Close()
	sb.Observe = pol.ObserveEnabled
	sb.ModelTelemetry = o.Profile.Telemetry

	for i := 1; i < len(mounts); i++ {
		mounts[i] = sb.ExportPath(mounts[i])
	}
	for i, p := range o.readOnlyMounts {
		o.readOnlyMounts[i] = sb.ExportPath(p)
	}
	if pol.ContextEnabled {
		o.readOnlyMounts = append(o.readOnlyMounts, sb.ExportPath(proto.ContextDir))
	}
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

func factsForRemote(facts proto.Facts, mode string, onWSL2 bool) proto.Facts {
	if !onWSL2 || mode != proto.ExecHost {
		return facts
	}
	// In host mode B mounts stubs at A's original PATH locations. The WSL
	// driver directory has no mountpoint under B's read-only /usr/lib.
	const wslRuntime = "/usr/lib/wsl"
	dirs := make([]proto.PathDir, 0, len(facts.PathDirs))
	var path []string
	for _, dir := range facts.PathDirs {
		if util.Within(filepath.Clean(dir.Dir), wslRuntime) || util.Within(filepath.Clean(dir.Real), wslRuntime) {
			continue
		}
		dirs = append(dirs, dir)
		if dir.OnPath {
			path = append(path, dir.Dir)
		}
	}
	if len(dirs) != len(facts.PathDirs) {
		facts.PathDirs = dirs
		facts.Env = util.EnvSet(facts.Env, "PATH", strings.Join(path, ":"))
	}
	return facts
}

func runRemote(o Options, host string, sb *Sandbox, id, cwd, project string, mounts []string, al *audit.Logger) int {
	tr := SSHTransport(o.Config, host)
	conn, err := tr.OpenResumable()
	if err != nil {
		warnf("%v", err)
		return 255
	}
	defer conn.Close()
	connectionState := func(up bool) {
		if o.connectionState != nil {
			status := "reconnecting"
			if up {
				status = "connected"
			}
			o.connectionState(status)
		}
	}
	// Track pipe/Paseo sessions too; terminal rendering replaces only the
	// notice part of this callback below, not the state tracking.
	stopConnectionNotice := conn.OnReconnect(func(up bool) {
		connectionState(up)
		fmt.Fprintf(os.Stderr, "\r\nwaldo: %s\r\n", transport.ReconnectMessage(up))
	})
	defer stopConnectionNotice()
	connectionState(!conn.Reconnecting())
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
		ExportSources: sb.ExportSources(),
		ExecMode:      o.ExecMode,
		Observe:       sb.Observe,
		Paseo:         o.paseoBroker != nil, ReadOnlyMounts: o.readOnlyMounts,
		Version: proto.FullVersion(), Session: id, Profile: o.Profile.Wire(), Argv: o.Args,
		Cwd: cwd, Project: project, Mounts: mounts, Facts: factsForRemote(sb.Facts, o.ExecMode, wsl.IsWSL2()), Interactive: interactive,
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
		watch: func(notice func(bool)) func() {
			return conn.OnReconnect(func(up bool) {
				connectionState(up)
				notice(up)
			})
		},
		startup: o.startup,
		route:   o.route.observer(append(append([]string(nil), o.Profile.Args...), o.Args...)),
	})
	if conn.ReconnectExpired() {
		warnf("90 秒内未能恢复连接，本次会话已结束")
	}
	return code
}

// acceptFromB serves file projection and exec streams. A fixed Paseo broker
// is accepted only when explicitly enabled for this session; arbitrary
// connection requests remain refused and logged.
func acceptFromB(bs *yamux.Session, sb *Sandbox, al *audit.Logger, brokers ...*paseo.Broker) {
	var handlers sync.WaitGroup
	defer handlers.Wait()
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
				// B is untrusted. Keep only the known observational label; it
				// never changes execution permissions or the target sandbox.
				if h.Exec.Tool != proto.ToolHostBash && !hosttools.Known(h.Exec.Tool) {
					h.Exec.Tool = ""
				}
				fields := map[string]any{"id": id, "argv": h.Exec.Argv, "path": h.Exec.Path, "cwd": h.Exec.Cwd}
				if h.Exec.Tool != "" {
					fields["tool"] = h.Exec.Tool
				}
				al.Log("exec", fields)
				in, err := sb.Open(proto.Header{Kind: proto.KindExec, Exec: h.Exec})
				if err != nil {
					logExecEnd(al, id, h.Exec, start, time.Now(), "start_failed", nil)
					st.Close()
					return
				}
				spliceExec(st, in, al, id, h.Exec, start)
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
	startup *startupStream
	route   *routeObserver
	cancel  func() bool
	watch   func(func(up bool)) func()
}

func relayAgentIOCancelable(st io.ReadWriteCloser, interactive bool, rc *relayReconnect) int {
	var cancelReconnect func() bool
	if rc != nil {
		cancelReconnect = rc.cancel
	}
	fw := proto.NewFrameWriter(st)
	var startup *startupStream
	if rc != nil {
		startup = rc.startup
	}
	if startup != nil {
		startup.attach(fw.Write)
	}
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
			if wire := proto.EncodeSignal(sig); wire != 0 {
				fw.Write(proto.FSignal, []byte{byte(wire)})
			}
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
				if n > 0 {
					var writeErr error
					if startup != nil {
						writeErr = startup.Input(buf[:n])
					} else {
						writeErr = fw.Write(proto.FStdin, buf[:n])
					}
					if writeErr != nil {
						warnf("发送 Claude 输入失败: %v", writeErr)
						st.Close()
						return
					}
				}
				if err != nil {
					if startup != nil {
						if err := startup.EOF(); err != nil {
							st.Close()
						}
					} else {
						fw.Write(proto.FStdinEOF, nil)
					}
					return
				}
			}
		}()
	}
	var out, errw io.Writer = os.Stdout, os.Stderr
	if startup != nil {
		out = startup
	}
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
			if rc != nil && rc.route != nil {
				if err := rc.route.observe(p); err != nil {
					warnf("记录远端会话失败: %v", err)
					st.Close()
					return 255
				}
			}
			outMu.Lock()
			_, writeErr := out.Write(p)
			outMu.Unlock()
			if writeErr != nil {
				warnf("处理 Claude 输出失败: %v", writeErr)
				st.Close()
				return 255
			}
		case proto.FStderr, proto.FError:
			errw.Write(p)
		case proto.FExit:
			var ex proto.Exit
			proto.DecodeJSON(p, &ex)
			return ex.Code
		}
	}
}
