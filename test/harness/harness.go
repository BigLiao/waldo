// Package harness sets up an isolated loopback environment for the e2e and
// attack tests: a freshly built waldo binary, private A data/config dirs, a
// private B data dir, and (for e2e) a user-mode sshd on a free port, so the
// tests never touch the real ~/.ssh or ~/.local/share/waldo.
package harness

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type Env struct {
	T       *testing.T
	Root    string // all state, removed at the end
	Bin     string // waldo binary
	Project string
	AData   string
	AConfig string
	BData   string
	SSHConf string
	Host    string
	sshd    *exec.Cmd
}

func repoRoot() string {
	_, f, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(filepath.Dir(f)))
}

// Require skips the test when a dependency is missing.
func Require(t *testing.T, bins ...string) {
	for _, b := range bins {
		if _, err := exec.LookPath(b); err != nil {
			if _, err := os.Stat(b); err != nil {
				t.Skipf("%s not available", b)
			}
		}
	}
}

// Build compiles a package of this module into dir.
func Build(t *testing.T, dir, pkg, name string) string {
	out := filepath.Join(dir, name)
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = repoRoot()
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, b)
	}
	return out
}

// New creates the environment. The project lives under $HOME (like a real
// project), in a fresh git repository.
func New(t *testing.T) *Env {
	Require(t, "bwrap", "git")
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cache, "waldo-test"), 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(filepath.Join(cache, "waldo-test"), "run-")
	if err != nil {
		t.Fatal(err)
	}
	e := &Env{T: t, Root: root}
	e.Bin = Build(t, filepath.Join(root, "bin"), "./cmd/waldo", "waldo")
	e.Project = filepath.Join(root, "proj")
	e.AData = filepath.Join(root, "a-data")
	e.AConfig = filepath.Join(root, "a-config")
	e.BData = filepath.Join(root, "b-data")
	for _, d := range []string{e.Project, e.AData, e.AConfig} {
		os.MkdirAll(d, 0o700)
	}
	git := exec.Command("git", "init", "-q", e.Project)
	if b, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, b)
	}
	os.WriteFile(filepath.Join(e.Project, "README"), []byte("hello from A\n"), 0o644)
	t.Cleanup(e.cleanup)
	return e
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// FreePort returns an unused TCP port.
func FreePort(t *testing.T) int { return freePort(t) }

// StartSSHD runs a user-mode sshd on 127.0.0.1 that only accepts a key
// generated for this test.
func (e *Env) StartSSHD() {
	t := e.T
	Require(t, "/usr/sbin/sshd", "ssh", "ssh-keygen", "sshfs")
	d := filepath.Join(e.Root, "sshd")
	os.MkdirAll(d, 0o700)
	for _, k := range []string{"hostkey", "clientkey"} {
		if b, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(d, k)).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen: %v %s", err, b)
		}
	}
	pub, _ := os.ReadFile(filepath.Join(d, "clientkey.pub"))
	os.WriteFile(filepath.Join(d, "authorized_keys"), pub, 0o600)
	port := freePort(t)
	cfg := fmt.Sprintf(`Port %d
ListenAddress 127.0.0.1
HostKey %s/hostkey
AuthorizedKeysFile %s/authorized_keys
PidFile %s/sshd.pid
UsePAM no
StrictModes no
PasswordAuthentication no
KbdInteractiveAuthentication no
AllowTcpForwarding no
AllowAgentForwarding no
X11Forwarding no
`, port, d, d, d)
	os.WriteFile(filepath.Join(d, "sshd_config"), []byte(cfg), 0o600)
	e.sshd = exec.Command("/usr/sbin/sshd", "-D", "-f", filepath.Join(d, "sshd_config"), "-E", filepath.Join(d, "sshd.log"))
	if err := e.sshd.Start(); err != nil {
		t.Fatalf("sshd: %v", err)
	}
	user := os.Getenv("USER")
	if user == "" {
		out, _ := exec.Command("id", "-un").Output()
		user = strings.TrimSpace(string(out))
	}
	e.SSHConf = filepath.Join(d, "ssh_config")
	os.WriteFile(e.SSHConf, []byte(fmt.Sprintf(`Host waldo-e2e
  HostName 127.0.0.1
  Port %d
  User %s
  IdentityFile %s/clientkey
  IdentitiesOnly yes
  StrictHostKeyChecking no
  UserKnownHostsFile %s/known_hosts
  LogLevel ERROR
`, port, user, d, d)), 0o600)
	e.Host = "waldo-e2e"
	for i := 0; i < 50; i++ {
		if c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
			c.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	e.WriteConfig("")
}

// WriteConfig writes A's config.toml (host settings plus extra TOML).
func (e *Env) WriteConfig(extra string) {
	var b strings.Builder
	if e.Host != "" {
		fmt.Fprintf(&b, "host = %q\nssh_args = [\"-F\", %q]\nremote_dir = %q\n", e.Host, e.SSHConf, e.BData)
	}
	b.WriteString(extra)
	os.WriteFile(filepath.Join(e.AConfig, "config.toml"), []byte(b.String()), 0o600)
}

// Cmd prepares a waldo invocation in the project directory.
func (e *Env) Cmd(args ...string) *exec.Cmd {
	cmd := exec.Command(e.Bin, args...)
	cmd.Dir = e.Project
	cmd.Env = e.Environ()
	return cmd
}

// Environ is the environment for waldo invocations.
func (e *Env) Environ() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "WALDO_") || strings.HasPrefix(k, "CLAUDE") || strings.HasPrefix(k, "ANTHROPIC") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "WALDO_DATA_DIR="+e.AData, "WALDO_CONFIG_DIR="+e.AConfig, "PWD="+e.Project)
}

// Mounts lists waldo FUSE mounts still present.
func Mounts() []string {
	b, _ := os.ReadFile("/proc/self/mountinfo")
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, " waldo:") {
			out = append(out, l)
		}
	}
	return out
}

func (e *Env) cleanup() {
	if e.sshd != nil && e.sshd.Process != nil {
		e.sshd.Process.Kill()
		e.sshd.Wait()
	}
	for _, m := range Mounts() {
		f := strings.Fields(m)
		if len(f) > 4 {
			exec.Command("fusermount3", "-u", "-z", f[4]).Run()
		}
	}
	if os.Getenv("WALDO_KEEP_TEST") == "" {
		exec.Command("chmod", "-R", "u+w", e.Root).Run()
		os.RemoveAll(e.Root)
	} else {
		e.T.Logf("kept %s", e.Root)
	}
}

// IsDir reports whether p is a directory.
func IsDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Exists reports whether p exists.
func Exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}
