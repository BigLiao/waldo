// Package session is the A-side orchestration in the trusted waldo process:
// it starts the sandbox and the SSH link and moves bytes between them, while
// applying the terminal filter, network policy and audit log.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/hashicorp/yamux"

	"waldo/internal/audit"
	"waldo/internal/config"
	"waldo/internal/netpolicy"
	"waldo/internal/proto"
	"waldo/internal/sandbox"
	"waldo/internal/util"
)

// ProxyPort is the outbound proxy port inside the sandbox network namespace.
const ProxyPort = 39339

// Sandbox is a running A-side sandbox.
type Sandbox struct {
	Facts   proto.Facts
	Project string
	Policy  *netpolicy.Policy
	Fwd     *netpolicy.Forwarder
	Audit   *audit.Logger

	stop func()
	sess *yamux.Session
}

// SandboxOptions configures StartSandbox.
type SandboxOptions struct {
	Home    string
	Project string
	Policy  *config.Policy
	Audit   *audit.Logger
	DataDir string
	Mkdirs  []string
	OnPorts func(active []int)
}

var passEnv = []string{
	"TERM", "COLORTERM", "LANG", "LANGUAGE", "TZ", "USER", "LOGNAME", "SHELL", "EDITOR", "VISUAL",
	"PAGER", "NO_COLOR", "FORCE_COLOR", "CLICOLOR", "TERM_PROGRAM", "TERM_PROGRAM_VERSION",
	"NVM_DIR", "NVM_BIN", "NVM_INC", "VOLTA_HOME", "PNPM_HOME", "BUN_INSTALL", "RUSTUP_HOME",
	"GOROOT", "PYENV_ROOT", "JAVA_HOME", "SDKMAN_DIR", "MISE_DATA_DIR", "ASDF_DIR", "ASDF_DATA_DIR",
}

// BaseEnv is the environment the sandbox (and the agent capsule) starts from.
// Only a fixed allow-list of variables leaves the host.
func BaseEnv(home, dataDir string, extra []string) []string {
	env := []string{"HOME=" + home}
	for _, k := range passEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for _, e := range os.Environ() {
		k, _, _ := strings.Cut(e, "=")
		if k == "HOME" || k == "PATH" || util.IsProxyVar(k) || strings.HasPrefix(k, "WALDO_") {
			continue
		}
		pass := strings.HasPrefix(k, "LC_")
		for _, pat := range extra {
			if ok, _ := path.Match(pat, k); ok {
				pass = true
			}
		}
		if pass {
			env = util.EnvSet(env, k, e[len(k)+1:])
		}
	}
	if _, ok := util.EnvGet(env, "TERM"); !ok {
		env = append(env, "TERM=xterm-256color")
	}
	var dirs []string
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if d == "" || !filepath.IsAbs(d) || util.Within(d, dataDir) {
			continue
		}
		dirs = append(dirs, d)
	}
	if len(dirs) == 0 {
		dirs = []string{"/usr/local/bin", "/usr/bin", "/bin"}
	}
	return append(env, "PATH="+strings.Join(dirs, ":"))
}

// ProxyEnv is forced onto every process executed in the sandbox.
func ProxyEnv() []string {
	u := fmt.Sprintf("http://127.0.0.1:%d", ProxyPort)
	return []string{
		"HTTP_PROXY=" + u, "HTTPS_PROXY=" + u, "http_proxy=" + u, "https_proxy=" + u,
		"ALL_PROXY=" + u, "all_proxy=" + u,
		"NO_PROXY=localhost,127.0.0.1,::1", "no_proxy=localhost,127.0.0.1,::1",
		"NODE_USE_ENV_PROXY=1",
	}
}

func newID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// StartSandbox launches `waldo sandbox-serve` inside the sandbox and waits
// for its facts.
func StartSandbox(o SandboxOptions) (*Sandbox, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	sbHome := filepath.Join(o.DataDir, "sandbox-home")
	if err := os.MkdirAll(sbHome, 0o700); err != nil {
		return nil, err
	}
	expose := sandbox.DetectToolchains(o.Home)
	expose = append(expose, o.Policy.ExposeRO...)
	base := BaseEnv(o.Home, o.DataDir, o.Policy.PassEnv)
	pathv, _ := util.EnvGet(base, "PATH")
	spec := sandbox.Spec{
		Home: o.Home, SandboxHome: sbHome, Project: o.Project, ExposeRO: expose, Exe: exe,
		Env: []string{"HOME=" + o.Home, "PATH=" + pathv},
	}
	cmd, err := sandbox.Default().Command(spec)
	if err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	os.MkdirAll(filepath.Join(o.DataDir, "log"), 0o700)
	if lf, err := os.OpenFile(filepath.Join(o.DataDir, "log", "sandbox.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		cmd.Stderr = lf
		defer lf.Close()
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动沙箱失败: %w", err)
	}
	waited := make(chan struct{})
	go func() { cmd.Wait(); close(waited) }()
	stop := func() {
		stdin.Close()
		select {
		case <-waited:
		case <-time.After(500 * time.Millisecond):
			cmd.Process.Kill()
			<-waited
		}
	}
	sess, err := yamux.Client(util.RWC{R: stdout, W: stdin, C: []io.Closer{stdin}}, util.YamuxConfig())
	if err != nil {
		stop()
		return nil, err
	}
	sb := &Sandbox{Project: o.Project, Audit: o.Audit, sess: sess}
	sb.stop = func() { sess.Close(); stop() }
	sb.Policy = netpolicy.New(o.Policy, o.Audit)
	sb.Fwd = &netpolicy.Forwarder{
		Mode:    o.Policy.Ports,
		Audit:   o.Audit,
		OnState: o.OnPorts,
		Dial: func(port int) (io.ReadWriteCloser, error) {
			return sb.Open(proto.Header{Kind: proto.KindDial, Port: port})
		},
	}
	ctl, err := sess.OpenStream()
	if err != nil {
		sb.Close()
		return nil, err
	}
	cfg := proto.SandboxConfig{BaseEnv: base, ExecEnv: ProxyEnv(), ProxyPort: ProxyPort, HostPorts: o.Policy.HostPorts, Mkdirs: o.Mkdirs}
	if err := proto.WriteMsg(ctl, proto.Header{Kind: proto.KindSandbox}); err != nil {
		sb.Close()
		return nil, err
	}
	if err := proto.WriteMsg(ctl, cfg); err != nil {
		sb.Close()
		return nil, err
	}
	factsCh := make(chan error, 1)
	go func() { factsCh <- proto.ReadMsg(ctl, &sb.Facts) }()
	select {
	case err := <-factsCh:
		if err != nil {
			sb.Close()
			return nil, fmt.Errorf("沙箱启动失败（详见 %s）: %v", filepath.Join(o.DataDir, "log", "sandbox.log"), err)
		}
	case <-time.After(20 * time.Second):
		sb.Close()
		return nil, fmt.Errorf("沙箱启动超时")
	}
	go func() {
		for {
			var ev proto.PortEvent
			if err := proto.ReadMsg(ctl, &ev); err != nil {
				return
			}
			sb.Fwd.Update(ev.Ports)
		}
	}()
	go sb.acceptLoop()
	return sb, nil
}

// acceptLoop serves streams the sandbox opens towards the host. Only proxy
// and host-port requests exist; everything else is refused.
func (sb *Sandbox) acceptLoop() {
	for {
		st, err := sb.sess.AcceptStream()
		if err != nil {
			return
		}
		go func() {
			st.SetReadDeadline(time.Now().Add(10 * time.Second))
			var h proto.Header
			err := proto.ReadMsg(st, &h)
			st.SetReadDeadline(time.Time{})
			if err != nil {
				st.Close()
				return
			}
			switch h.Kind {
			case proto.KindConnect:
				sb.Policy.HandleConnect(st, h.Host, h.Port)
			case proto.KindHostPort:
				sb.Policy.HandleHostPort(st, h.Port)
			default:
				sb.Audit.Log("reject", map[string]any{"from": "sandbox", "stream": h.Kind})
				st.Close()
			}
		}()
	}
}

// Open opens a stream into the sandbox and sends h.
func (sb *Sandbox) Open(h proto.Header) (*yamux.Stream, error) {
	st, err := sb.sess.OpenStream()
	if err != nil {
		return nil, err
	}
	if err := proto.WriteMsg(st, h); err != nil {
		st.Close()
		return nil, err
	}
	return st, nil
}

// Close tears the sandbox down; every process in it dies.
func (sb *Sandbox) Close() {
	if sb.Fwd != nil {
		sb.Fwd.Close()
	}
	if sb.stop != nil {
		sb.stop()
	}
}

// LookPath resolves name against the sandbox PATH as reported in Facts.
func (sb *Sandbox) LookPath(name string) string {
	if strings.Contains(name, "/") {
		return name
	}
	for _, pd := range sb.Facts.PathDirs {
		if !pd.OnPath {
			continue
		}
		for _, n := range pd.Names {
			if n == name {
				return filepath.Join(pd.Dir, n)
			}
		}
	}
	return ""
}
