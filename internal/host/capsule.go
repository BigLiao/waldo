package host

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"waldo/internal/proto"
	"waldo/internal/util"
)

// agentSpec is the agent binary resolved on B.
type agentSpec struct {
	exe    string            // real path on B
	interp string            // real path of the "#!" interpreter, if any
	iargs  []string          // interpreter arguments from the "#!" line
	binds  map[string]string // extra ro binds (B path -> capsule path)
}

func extraBinDirs() []string {
	home, _ := os.UserHomeDir()
	return []string{
		filepath.Join(home, ".local", "bin"), filepath.Join(home, "bin"),
		filepath.Join(home, ".npm-global", "bin"), filepath.Join(home, ".bun", "bin"),
		filepath.Join(home, ".cargo", "bin"), filepath.Join(home, ".volta", "bin"),
		"/usr/local/bin", "/opt/homebrew/bin",
	}
}

// lookBin finds name on B's PATH, common install dirs, or via a login shell.
func lookBin(name string) (string, error) {
	if strings.Contains(name, "/") {
		home, _ := os.UserHomeDir()
		p := util.ExpandHome(name, home)
		if _, err := os.Stat(p); err != nil {
			return "", err
		}
		return p, nil
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	for _, d := range extraBinDirs() {
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/sh"
	}
	out, err := exec.Command(sh, "-lc", "command -v "+util.ShellQuote(name)).Output()
	if err == nil {
		p := strings.TrimSpace(string(out))
		if filepath.IsAbs(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("B 上找不到 %q（请先在 B 上安装该 Agent）", name)
}

func resolveAgent(bin string) (*agentSpec, error) {
	p, err := lookBin(bin)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return nil, err
	}
	a := &agentSpec{exe: real, binds: map[string]string{}}
	f, err := os.Open(real)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	line, _ := bufio.NewReader(f).ReadString('\n')
	if !strings.HasPrefix(line, "#!") {
		return a, nil
	}
	// A script: its interpreter must come from B, not from A via a stub.
	fields := strings.Fields(strings.TrimSpace(line[2:]))
	if len(fields) == 0 {
		return nil, fmt.Errorf("%s: 空的 #! 行", real)
	}
	ip, iargs := fields[0], fields[1:]
	if filepath.Base(ip) == "env" && len(iargs) > 0 {
		for len(iargs) > 0 && strings.HasPrefix(iargs[0], "-") {
			iargs = iargs[1:] // env -S etc.
		}
		if len(iargs) == 0 {
			return nil, fmt.Errorf("%s: 无法解析 #! 行", real)
		}
		ip, iargs = iargs[0], iargs[1:]
	}
	ipath, err := lookBin(ip)
	if err != nil {
		return nil, err
	}
	if a.interp, err = filepath.EvalSymlinks(ipath); err != nil {
		return nil, err
	}
	a.iargs = iargs
	// Make the script's package tree visible at its real path.
	if i := strings.Index(real, "/node_modules/"); i >= 0 {
		root := real[:i+len("/node_modules")]
		a.binds[root] = root
	} else {
		a.binds[real] = real
	}
	return a, nil
}

var etcFiles = []string{
	"resolv.conf", "hosts", "host.conf", "nsswitch.conf", "gai.conf", "passwd", "group",
	"localtime", "timezone", "ld.so.cache", "ld.so.conf", "ld.so.conf.d", "ssl", "ca-certificates",
	"ca-certificates.conf", "pki", "crypto-policies", "mime.types", "terminfo", "os-release",
}

// capsuleArgs builds the bwrap command line for the agent capsule.
func (h *host) capsuleArgs(agentHome string, mnts []string, stubs map[string]string, ag *agentSpec) ([]string, error) {
	hl := h.hello
	home := hl.Facts.Home
	if !filepath.IsAbs(home) {
		return nil, fmt.Errorf("invalid HOME from A: %q", home)
	}
	hn := hl.Facts.Hostname
	if hn == "" {
		hn = "waldo"
	}
	a := []string{
		"--unshare-user", "--unshare-pid", "--unshare-ipc", "--unshare-uts", "--unshare-cgroup-try",
		"--hostname", hn,
		"--die-with-parent",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/run",
	}
	for _, d := range []string{"/usr/lib", "/usr/lib64", "/usr/lib32", "/usr/libexec", "/usr/share"} {
		if util.IsDir(d) {
			a = append(a, "--ro-bind", d, d)
		}
	}
	for _, d := range []string{"/lib", "/lib64", "/lib32"} {
		st, err := os.Lstat(d)
		if err != nil {
			continue
		}
		if st.Mode()&os.ModeSymlink != 0 {
			if t, err := os.Readlink(d); err == nil {
				a = append(a, "--symlink", t, d)
			}
		} else if st.IsDir() {
			a = append(a, "--ro-bind", d, d)
		}
	}
	for _, f := range etcFiles {
		src := "/etc/" + f
		if real, err := filepath.EvalSymlinks(src); err == nil {
			a = append(a, "--ro-bind", real, src)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	a = append(a,
		"--ro-bind", exe, "/run/waldo/waldo",
		"--bind", filepath.Join(h.runDir, "s"), "/run/waldo/s",
	)
	agentPath := "/run/waldo/agent/" + filepath.Base(ag.exe)
	var argv0 []string
	if ag.interp != "" {
		ipath := "/run/waldo/agent/interp-" + filepath.Base(ag.interp)
		a = append(a, "--ro-bind", ag.interp, ipath)
		for src, dst := range ag.binds {
			a = append(a, "--ro-bind", src, dst)
		}
		argv0 = append(append([]string{ipath}, ag.iargs...), ag.exe)
	} else {
		a = append(a, "--ro-bind", ag.exe, agentPath)
		argv0 = []string{agentPath}
	}

	// HOME is B's persistent agent state, under A's HOME path string.
	type bind struct {
		src, dst string
		ro       bool
	}
	binds := []bind{{agentHome, home, false}}
	for _, p := range adopted(h.dataDir) {
		rel, ok := strings.CutPrefix(p, "~/")
		if !ok {
			continue
		}
		bhome, _ := os.UserHomeDir()
		src := filepath.Join(bhome, rel)
		// Only directories are bound: renaming a file onto a single-file
		// bind mount fails with EBUSY, which breaks atomic rewrites.
		if util.IsDir(src) {
			binds = append(binds, bind{src, filepath.Join(home, rel), false})
		}
	}
	for i, m := range hl.Mounts {
		ro := false
		for _, p := range hl.ReadOnlyMounts {
			if m == p {
				ro = true
				break
			}
		}
		binds = append(binds, bind{mnts[i], m, ro})
	}
	for dst, src := range stubs {
		binds = append(binds, bind{src, dst, true})
	}
	sort.SliceStable(binds, func(i, j int) bool {
		return strings.Count(binds[i].dst, "/") < strings.Count(binds[j].dst, "/")
	})
	for _, b := range binds {
		if b.ro {
			a = append(a, "--ro-bind", b.src, b.dst)
		} else {
			a = append(a, "--bind", b.src, b.dst)
		}
	}
	cwd := hl.Cwd
	if cwd == "" {
		cwd = hl.Project
	}
	a = append(a, "--chdir", cwd, "--clearenv")
	for _, e := range h.agentEnv() {
		k, v, ok := strings.Cut(e, "=")
		if ok && k != "" {
			a = append(a, "--setenv", k, v)
		}
	}
	a = append(a, "--")
	a = append(a, argv0...)
	a = append(a, hl.Profile.Args...)
	a = append(a, hl.Argv...)
	return a, nil
}

// agentEnv is A's sandbox environment (minus proxy settings, which only make
// sense on A), plus the profile env, plus credentials that stay on B.
func (h *host) agentEnv() []string {
	hl := h.hello
	env := util.EnvFilter(hl.Facts.Env, func(k string) bool {
		return util.IsProxyVar(k) || strings.HasPrefix(k, "WALDO_")
	})
	keys := make([]string, 0, len(hl.Profile.Env))
	for k := range hl.Profile.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = util.EnvSet(env, k, hl.Profile.Env[k])
	}
	for _, k := range hl.Profile.Scrub {
		if v, ok := os.LookupEnv(k); ok {
			env = util.EnvSet(env, k, v)
		}
	}
	if h.modelObservation != nil {
		env = util.EnvFilter(env, func(k string) bool {
			return k == "OTEL_LOG_RAW_API_BODIES" || k == "BETA_TRACING_ENDPOINT" || k == "ENABLE_BETA_TRACING_DETAILED"
		})
	}
	for _, e := range h.observeEnv {
		k, v, _ := strings.Cut(e, "=")
		env = util.EnvSet(env, k, v)
	}
	cwd := hl.Cwd
	if cwd == "" {
		cwd = hl.Project
	}
	env = util.EnvSet(env, "PWD", cwd)
	env = util.EnvSet(env, "WALDO_SESSION", hl.Session)
	for _, m := range hl.Mounts {
		if m == proto.ContextDir {
			env = util.EnvSet(env, "WALDO_CONTEXT", proto.ContextFile)
		}
	}
	return env
}

// serveStubs relays each stub connection to A as one exec stream.
func (h *host) serveStubs(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go h.handleStub(c)
	}
}

func (h *host) handleStub(c net.Conn) {
	defer c.Close()
	var hd proto.Header
	if err := proto.ReadMsg(c, &hd); err != nil || hd.Kind != proto.KindExec || hd.Exec == nil {
		return
	}
	hd.Exec.Env = util.EnvFilter(hd.Exec.Env, func(k string) bool {
		return strings.HasPrefix(k, "WALDO_") || h.scrub[k]
	})
	st, err := h.sess.OpenStream()
	if err != nil {
		fw := proto.NewFrameWriter(c)
		fw.Write(proto.FError, []byte("waldo: 与 A 的会话已断开\n"))
		fw.WriteJSON(proto.FExit, proto.Exit{Code: 255})
		return
	}
	if err := proto.WriteMsg(st, hd); err != nil {
		st.Close()
		return
	}
	h.mu.Lock()
	hasCache := len(h.cacheWatches) > 0
	h.mu.Unlock()
	if hasCache {
		relayExec(st, c.(*net.UnixConn), h.invalidateCachesBeforeExit)
	} else {
		util.Splice(st, c.(*net.UnixConn))
	}
}

// runAgent starts the capsule and relays the agent's stdio over the io
// stream. It returns the agent's exit code.
func (h *host) runAgent(cmd *exec.Cmd, st io.ReadWriteCloser) int {
	fw := proto.NewFrameWriter(st)
	var (
		stdin  io.WriteCloser
		outs   []io.Reader
		master *os.File
	)
	if h.hello.Interactive {
		rows, cols := h.hello.Rows, h.hello.Cols
		if rows == 0 || cols == 0 {
			rows, cols = 24, 80
		}
		m, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
		if err != nil {
			fw.Write(proto.FError, []byte(fmt.Sprintf("waldo host: 启动胶囊失败: %v\n", err)))
			fw.WriteJSON(proto.FExit, proto.Exit{Code: 1})
			return 1
		}
		master, stdin, outs = m, m, []io.Reader{m}
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		in, err1 := cmd.StdinPipe()
		out, err2 := cmd.StdoutPipe()
		errp, err3 := cmd.StderrPipe()
		if err := errors.Join(err1, err2, err3); err != nil {
			return 1
		}
		if err := cmd.Start(); err != nil {
			fw.Write(proto.FError, []byte(fmt.Sprintf("waldo host: 启动胶囊失败: %v\n", err)))
			fw.WriteJSON(proto.FExit, proto.Exit{Code: 1})
			return 1
		}
		stdin, outs = in, []io.Reader{out, errp}
	}
	h.mu.Lock()
	h.children = append(h.children, cmd)
	h.mu.Unlock()
	pgid := cmd.Process.Pid

	var wg sync.WaitGroup
	for i, r := range outs {
		typ := proto.FStdout
		if i == 1 {
			typ = proto.FStderr
		}
		wg.Add(1)
		go func(r io.Reader, w io.Writer) { defer wg.Done(); io.Copy(w, r) }(r, fw.Writer(typ))
	}
	go func() {
		for {
			typ, p, err := proto.ReadFrame(st)
			if err != nil {
				// A went away: end the session.
				syscall.Kill(-pgid, syscall.SIGKILL)
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
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
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
			ex.Code = 1
		}
	}
	// The final telemetry batch must enter the transport before A sees the
	// agent's exit and closes the session. This is bounded and opt-in only.
	h.stopModelObservation()
	fw.WriteJSON(proto.FExit, ex)
	st.Close()
	if master != nil {
		master.Close()
	}
	return ex.Code
}

// ---- adopted login state ----

func adoptedPath(dataDir string) string { return filepath.Join(dataDir, "adopted.json") }

func adopted(dataDir string) []string {
	b, err := os.ReadFile(adoptedPath(dataDir))
	if err != nil {
		return nil
	}
	var list []string
	json.Unmarshal(b, &list)
	return list
}

// adoptMain makes existing agent state on B (paths on stdin, one per line,
// "~/"-relative) available to capsules. Directories are recorded and bound
// into every capsule, so there is one copy of the credentials they hold
// (e.g. ~/.claude/.credentials.json). Single files are copied once into the
// capsule HOME instead: a single-file bind would make rename-over-it fail.
func adoptMain(dataDir string) int {
	home, _ := os.UserHomeDir()
	agentHome := filepath.Join(dataDir, "home")
	cur := map[string]bool{}
	for _, p := range adopted(dataDir) {
		cur[p] = true
	}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		p := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(p, "~/") || strings.Contains(p, "..") {
			continue
		}
		src := filepath.Join(home, p[2:])
		st, err := os.Stat(src)
		switch {
		case err != nil:
			continue
		case st.IsDir():
			if !cur[p] {
				fmt.Printf("绑定目录 %s\n", p)
			}
			cur[p] = true
		case st.Mode().IsRegular():
			dst := filepath.Join(agentHome, p[2:])
			if dst == src {
				continue
			}
			if dt, err := os.Lstat(dst); err == nil && dt.Size() > 0 {
				fmt.Printf("保留已有副本 %s\n", p)
				continue
			}
			os.Remove(dst) // possibly an empty bind-mount placeholder
			if err := copyFile(src, dst, st.Mode().Perm()); err != nil {
				fmt.Fprintf(os.Stderr, "复制 %s: %v\n", p, err)
				return 1
			}
			fmt.Printf("复制文件 %s\n", p)
		}
	}
	var list []string
	for p := range cur {
		if util.IsDir(filepath.Join(home, p[2:])) {
			list = append(list, p)
		}
	}
	sort.Strings(list)
	if err := writeAdopted(dataDir, list); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// Disabling adoption affects future capsules. Only the binding registry is
// cleared; native state, private capsule state and copied files are retained.
func unadoptMain(dataDir string) int {
	if err := writeAdopted(dataDir, nil); err != nil {
		fmt.Fprintf(os.Stderr, "关闭 Agent 状态目录绑定: %v\n", err)
		return 1
	}
	fmt.Println("已关闭后续会话的状态目录绑定（B 原始状态和 waldo 已有副本均保留）")
	return 0
}

func writeAdopted(dataDir string, list []string) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	if list == nil {
		list = []string{}
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	// Replace the registry atomically so capsules see a complete list. A
	// pre-existing registry symlink must never cause a credentials overwrite.
	f, err := os.CreateTemp(dataDir, ".adopted-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), adoptedPath(dataDir))
}

func copyFile(src, dst string, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".waldo-tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
