// Package host implements `waldo host`, the per-session process on B. It is
// started by A over SSH, builds the capsule (bwrap) the agent runs in, mounts
// A's project via SFTP, and funnels stub requests back to A.
package host

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hashicorp/yamux"

	"waldo/internal/proto"
	"waldo/internal/util"
)

type host struct {
	dataDir string
	runDir  string
	sess    *yamux.Session
	hello   proto.Hello
	scrub   map[string]bool
	idMaps  idMapFiles

	mu       sync.Mutex
	children []*exec.Cmd
	mounts   []string
}

// Main is the entry point of `waldo host`.
func Main(args []string) int {
	fs := flag.NewFlagSet("host", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "data directory on B")
	doctor := fs.Bool("doctor", false, "print a JSON environment report and exit")
	adopt := fs.Bool("adopt", false, "share existing agent state (paths on stdin); false disables saved directory bindings")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dataDir == "" {
		home, _ := os.UserHomeDir()
		*dataDir = filepath.Join(home, ".local", "share", "waldo")
	}
	if *doctor {
		return doctorMain(*dataDir)
	}
	if flagWasSet(fs, "adopt") {
		if *adopt {
			return adoptMain(*dataDir)
		}
		return unadoptMain(*dataDir)
	}
	h := &host{dataDir: *dataDir}
	// When the SSH link dies we must still clean up: writes to the closed
	// stdout return EPIPE instead of killing us, and SIGHUP/SIGTERM end the
	// session through the normal path.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGPIPE, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for s := range sigs {
			if s == syscall.SIGPIPE {
				continue
			}
			log.Printf("signal %v: ending session", s)
			if h.sess != nil {
				h.sess.Close()
			}
			time.AfterFunc(5*time.Second, func() { h.cleanup(); os.Exit(1) })
		}
	}()
	if err := os.MkdirAll(filepath.Join(h.dataDir, "log"), 0o700); err == nil {
		if f, err := os.OpenFile(filepath.Join(h.dataDir, "log", "host.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			log.SetOutput(f)
		}
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetPrefix(fmt.Sprintf("[%d] ", os.Getpid()))
	code, err := h.run()
	h.cleanup()
	if err != nil {
		log.Printf("error: %v", err)
		fmt.Fprintf(os.Stderr, "waldo host: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

func (h *host) run() (int, error) {
	// The banner tells A that the right binary is running and yamux follows.
	if _, err := os.Stdout.WriteString(proto.BannerPrefix + proto.FullVersion() + "\n"); err != nil {
		return 1, err
	}
	sess, err := yamux.Server(util.RWC{R: os.Stdin, W: os.Stdout, C: []io.Closer{os.Stdin, os.Stdout}}, util.YamuxConfig())
	if err != nil {
		return 1, err
	}
	h.sess = sess
	ctl, err := sess.AcceptStream()
	if err != nil {
		return 1, err
	}
	var hd proto.Header
	if err := proto.ReadMsg(ctl, &hd); err != nil || hd.Kind != proto.KindCtl {
		return 1, fmt.Errorf("expected ctl stream")
	}
	if err := proto.ReadMsg(ctl, &h.hello); err != nil {
		return 1, err
	}
	ack := proto.HelloAck{Version: proto.FullVersion(), SkewMs: time.Now().UnixMilli() - h.hello.TimeUnixMs}
	if h.hello.Version != proto.FullVersion() {
		ack.Error = fmt.Sprintf("version mismatch: A=%s B=%s", h.hello.Version, proto.FullVersion())
		proto.WriteMsg(ctl, ack)
		return 1, errors.New(ack.Error)
	}
	log.Printf("session %s: agent=%s project=%s", h.hello.Session, h.hello.Profile.Name, h.hello.Project)
	h.scrub = map[string]bool{}
	for _, k := range h.hello.Profile.Scrub {
		h.scrub[k] = true
	}
	cmd, err := h.prepare()
	if err != nil {
		ack.Error = err.Error()
		proto.WriteMsg(ctl, ack)
		return 1, err
	}
	if err := proto.WriteMsg(ctl, ack); err != nil {
		return 1, err
	}
	io, err := sess.AcceptStream()
	if err != nil {
		return 1, err
	}
	if err := proto.ReadMsg(io, &hd); err != nil || hd.Kind != proto.KindIO {
		return 1, fmt.Errorf("expected io stream")
	}
	code := h.runAgent(cmd, io)
	log.Printf("agent exited with %d", code)
	return code, nil
}

func randID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// runtimeBase is where per-session run directories live on B.
func (h *host) runtimeBase() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" && util.IsDir(d) {
		return filepath.Join(d, "waldo")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("waldo-%d", os.Getuid()))
}

// prepare mounts, stubs and builds the capsule command.
func (h *host) prepare() (*exec.Cmd, error) {
	if h.hello.MetadataCacheTTLSeconds < 0 || h.hello.MetadataCacheTTLSeconds > 60 {
		return nil, fmt.Errorf("invalid metadata cache TTL: %d seconds", h.hello.MetadataCacheTTLSeconds)
	}
	base := h.runtimeBase()
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	cleanStale(base)
	h.runDir = filepath.Join(base, randID())
	for _, d := range []string{"m", "stubs", "s"} {
		if err := os.MkdirAll(filepath.Join(h.runDir, d), 0o700); err != nil {
			return nil, err
		}
	}
	os.WriteFile(filepath.Join(h.runDir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
	var err error
	h.idMaps, err = prepareIDMaps(h.runDir, h.hello.Facts.UID, h.hello.Facts.GID)
	if err != nil {
		return nil, err
	}

	agentHome := filepath.Join(h.dataDir, "home")
	if err := os.MkdirAll(agentHome, 0o700); err != nil {
		return nil, err
	}

	// Stub socket first so stubs never find it missing.
	ln, err := net.Listen("unix", filepath.Join(h.runDir, "s", "sock"))
	if err != nil {
		return nil, err
	}
	go h.serveStubs(ln)

	mnts := make([]string, len(h.hello.Mounts))
	for i, m := range h.hello.Mounts {
		dir := filepath.Join(h.runDir, "m", strconv.Itoa(i))
		if err := h.mount(m, dir); err != nil {
			return nil, fmt.Errorf("挂载 %s: %w", m, err)
		}
		mnts[i] = dir
	}
	stubDirs, err := h.makeStubs()
	if err != nil {
		return nil, err
	}
	agent, err := resolveAgent(h.hello.Profile.Bin)
	if err != nil {
		return nil, err
	}
	args, err := h.capsuleArgs(agentHome, mnts, stubDirs, agent)
	if err != nil {
		return nil, err
	}
	bw, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("B 上找不到 bwrap（bubblewrap）")
	}
	cmd := exec.Command(bw, args...)
	cmd.Env = []string{}
	return cmd, nil
}

// mount attaches A's directory m at dir with `sshfs -o passive`, whose SFTP
// session runs over a dedicated stream to A.
func (h *host) mount(m, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	st, err := h.sess.OpenStream()
	if err != nil {
		return err
	}
	if err := proto.WriteMsg(st, proto.Header{Kind: proto.KindFS, Path: m}); err != nil {
		return err
	}
	opts := "passive,sshfs_sync,fsname=" + escapeFuseValue("waldo:"+strings.ReplaceAll(m, ",", "_"))
	args := []string{"-f", "-o", opts}
	cacheable := projectMetadataCacheable(h.hello.Project, m)
	ttl := time.Duration(h.hello.MetadataCacheTTLSeconds) * time.Second
	for _, option := range sshfsCacheOptions(ttl, cacheable) {
		args = append(args, "-o", option)
	}
	if !cacheable {
		ttl = 0
	}
	log.Printf("mount %s: metadata_cache_ttl=%s", m, ttl)
	for _, option := range h.idMaps.options() {
		args = append(args, "-o", option)
	}
	args = append(args, ":"+m, dir)
	cmd := exec.Command("sshfs", args...)
	cmd.Stdin = st
	cmd.Stdout = st
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, n: 4096}
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	if err := cmd.Start(); err != nil {
		st.Close()
		return fmt.Errorf("启动 sshfs 失败（B 上需要安装 sshfs）: %w", err)
	}
	h.mu.Lock()
	h.children = append(h.children, cmd)
	h.mounts = append(h.mounts, dir)
	h.mu.Unlock()
	exited := make(chan struct{})
	go func() { cmd.Wait(); st.Close(); close(exited) }()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if isMounted(dir) {
			return nil
		}
		select {
		case <-exited:
			return fmt.Errorf("sshfs 退出: %s", strings.TrimSpace(stderr.String()))
		case <-time.After(20 * time.Millisecond):
		}
	}
	return fmt.Errorf("sshfs 挂载超时")
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		q := p
		if len(q) > l.n {
			q = q[:l.n]
		}
		l.w.Write(q)
		l.n -= len(q)
	}
	return len(p), nil
}

func unescapeMount(s string) string {
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}

func isMounted(dir string) bool {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) > 4 && unescapeMount(fields[4]) == dir {
			return true
		}
	}
	return false
}

// makeStubs creates one directory of stub symlinks per distinct A PATH dir.
func (h *host) makeStubs() (map[string]string, error) {
	out := map[string]string{} // capsule dir -> stub dir on B
	byReal := map[string]string{}
	for i, pd := range h.hello.Facts.PathDirs {
		if !filepath.IsAbs(pd.Dir) || !filepath.IsAbs(pd.Real) {
			continue
		}
		sd, ok := byReal[pd.Real]
		if !ok {
			sd = filepath.Join(h.runDir, "stubs", strconv.Itoa(i))
			if err := os.MkdirAll(sd, 0o755); err != nil {
				return nil, err
			}
			for _, n := range pd.Names {
				if n == "" || n == "." || n == ".." || strings.ContainsAny(n, "/\x00") {
					continue
				}
				os.Symlink("/run/waldo/waldo", filepath.Join(sd, n))
			}
			byReal[pd.Real] = sd
		}
		out[filepath.Clean(pd.Dir)] = sd
		out[filepath.Clean(pd.Real)] = sd
	}
	return out, nil
}

// cleanup terminates children, unmounts and removes the run directory.
func (h *host) cleanup() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.mounts {
		exec.Command("fusermount3", "-u", "-z", m).Run()
	}
	for _, c := range h.children {
		if c.Process != nil {
			c.Process.Signal(syscall.SIGTERM)
		}
	}
	if h.runDir != "" {
		removeRunDir(h.runDir)
	}
}

func removeRunDir(dir string) {
	// Never recurse into a live mount.
	ents, _ := os.ReadDir(filepath.Join(dir, "m"))
	for _, e := range ents {
		p := filepath.Join(dir, "m", e.Name())
		if isMounted(p) {
			exec.Command("fusermount3", "-u", "-z", p).Run()
		}
		os.Remove(p)
	}
	for _, sub := range []string{"stubs", "s", "pid", "uidmap", "gidmap"} {
		os.RemoveAll(filepath.Join(dir, sub))
	}
	os.Remove(filepath.Join(dir, "m"))
	os.Remove(dir)
}

// cleanStale removes run directories left behind by dead host processes.
func cleanStale(base string) {
	ents, _ := os.ReadDir(base)
	for _, e := range ents {
		d := filepath.Join(base, e.Name())
		b, err := os.ReadFile(filepath.Join(d, "pid"))
		if err != nil {
			continue
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		if pid > 0 && syscall.Kill(pid, 0) == nil {
			continue
		}
		removeRunDir(d)
	}
}
