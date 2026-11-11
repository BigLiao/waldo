// End-to-end tests: the full A -> ssh -> B -> capsule -> stub -> A chain on
// one machine (loopback sshd), driven by a fake agent.
//
// Because A and B are the same machine here, "where did it run" is decided by
// network namespace: the A sandbox has its own, the B capsule shares the host's.
package e2e

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"waldo/test/harness"
)

func parseResults(out string) map[string]string {
	r := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, "\r")
		if rest, ok := strings.CutPrefix(l, "RESULT "); ok {
			k, v, _ := strings.Cut(rest, " ")
			r[k] = v
		}
	}
	return r
}

func hostNetns(t *testing.T) string {
	s, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func setup(t *testing.T) (*harness.Env, string) {
	e := harness.New(t)
	e.StartSSHD()
	agent := harness.Build(t, filepath.Join(e.Root, "bin"), "./test/e2e/fakeagent", "fakeagent")
	return e, agent
}

func TestProbe(t *testing.T) {
	e, agent := setup(t)
	readmePath := filepath.Join(e.Project, "README")
	readmeBefore, err := os.Stat(readmePath)
	if err != nil {
		t.Fatal(err)
	}
	e.WriteConfig("[sandbox]\npass_env = [\"E2E_*\"]\n")
	port := harness.FreePort(t)
	cmd := e.Cmd("run", "--", agent, "probe")
	cmd.Env = append(cmd.Env, "E2E_PORT="+strconv.Itoa(port), "FAKE_SECRET=x")
	stdout, _ := cmd.StdoutPipe()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	sc := bufio.NewScanner(stdout)
	hostGot := ""
	for sc.Scan() {
		all.WriteString(sc.Text() + "\n")
		if sc.Text() == "READY" {
			// The sandbox's server must be reachable from A's host.
			for i := 0; i < 50; i++ {
				resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
				if err == nil {
					hostGot = resp.Status
					resp.Body.Close()
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			os.WriteFile(filepath.Join(e.Project, ".e2e-done"), nil, 0o644)
		}
	}
	err = cmd.Wait()
	code := cmd.ProcessState.ExitCode()
	t.Logf("output:\n%s\nstderr:\n%s", all.String(), stderr.String())
	if code != 42 {
		t.Fatalf("exit code %d (err %v), want 42", code, err)
	}
	r := parseResults(all.String())
	hn := hostNetns(t)
	sbNet := r["bash_netns"]
	check := func(k, want string) {
		t.Helper()
		if r[k] != want {
			t.Errorf("%s = %q, want %q", k, r[k], want)
		}
	}
	if r["agent_netns"] != hn {
		t.Errorf("agent should run in B's (host) netns: %q vs %q", r["agent_netns"], hn)
	}
	if sbNet == "" || sbNet == hn {
		t.Errorf("bash should run in A sandbox netns, got %q (host %q)", sbNet, hn)
	}
	check("bash_code", "3")
	check("abs_sh", "abs-ok/0")
	check("bash_is_stub", "true")
	check("readme", "hello from Afalse")
	check("readme_owner", "true")
	check("rename_err", "true")
	check("agent_file_owner", "true")
	check("cat_agent_txt", "written-by-agent")
	check("env_has_proxy", "true")
	check("env_has_waldo", "false")
	check("tr", "HELLO WORLD")
	check("tmp_shared", "tmp-ok")
	check("tmp_private_owner", "true")
	check("tmp_private_mode", "true")
	check("sigterm", "started,got-term/7")
	check("killed_group", "dead")
	check("bg_start", "0/true")
	check("bg_curl_in_sandbox", "200")
	check("script_netns", sbNet+"/0")
	if hostGot != "200 OK" {
		t.Errorf("host could not reach forwarded port: %q", hostGot)
	}
	// File written in-process on B landed in A's real project directory.
	if b, _ := os.ReadFile(filepath.Join(e.Project, "agent.txt")); string(b) != "written-by-agent\n" {
		t.Errorf("agent.txt on A: %q", b)
	}
	// Ownership translation is a view on B. It must not chown A's real files.
	readmeAfter, err := os.Stat(readmePath)
	if err != nil {
		t.Fatal(err)
	}
	beforeID := readmeBefore.Sys().(*syscall.Stat_t)
	afterID := readmeAfter.Sys().(*syscall.Stat_t)
	if beforeID.Uid != afterID.Uid || beforeID.Gid != afterID.Gid || readmeBefore.Mode() != readmeAfter.Mode() {
		t.Errorf("README ownership or permissions changed on A: before=%d:%d/%v after=%d:%d/%v",
			beforeID.Uid, beforeID.Gid, readmeBefore.Mode(), afterID.Uid, afterID.Gid, readmeAfter.Mode())
	}
	written, err := os.Stat(filepath.Join(e.Project, "agent.txt"))
	if err != nil {
		t.Fatal(err)
	}
	writtenID := written.Sys().(*syscall.Stat_t)
	if int(writtenID.Uid) != os.Getuid() || int(writtenID.Gid) != os.Getgid() {
		t.Errorf("agent.txt owner on A = %d:%d, want %d:%d", writtenID.Uid, writtenID.Gid, os.Getuid(), os.Getgid())
	}
	if !harness.IsDir(filepath.Join(e.Project, "sub", "dir")) {
		t.Error("mkdir did not reach A")
	}
	// Audit log on A has the exec and the write.
	alog, _ := os.ReadFile(filepath.Join(e.AData, "audit.log"))
	for _, want := range []string{`"kind":"exec"`, `"kind":"write"`, "agent.tmp", `"kind":"rename"`, `"kind":"port-open"`} {
		if !strings.Contains(string(alog), want) {
			t.Errorf("audit log lacks %s", want)
		}
	}
	waitNoMounts(t)
}

func TestOwnershipProbe(t *testing.T) {
	e, agent := setup(t)
	out, err := e.Cmd("run", "--", agent, "ownership").CombinedOutput()
	if err != nil {
		t.Fatalf("ownership probe: %v\n%s", err, out)
	}
	r := parseResults(string(out))
	for _, name := range []string{"project_owner", "project_private_owner", "project_private_mode", "project_file_owner", "project_file_mode", "project_write_rename", "tmp_private_owner", "tmp_private_mode", "tmp_file_owner", "tmp_file_mode", "tmp_write_rename", "ownership_ok"} {
		if r[name] != "true" {
			t.Errorf("%s = %q\n%s", name, r[name], out)
		}
	}
	left, err := filepath.Glob(filepath.Join(e.Project, ".waldo-ownership-*"))
	if err != nil || len(left) != 0 {
		t.Errorf("ownership probe left project files: %v (error %v)", left, err)
	}
	waitNoMounts(t)
}

func TestMetadataCacheFreshness(t *testing.T) {
	for _, ttl := range []string{"0", "1"} {
		t.Run(ttl+"s", func(t *testing.T) {
			e, agent := setup(t)
			e.WriteConfig("[fs]\nmetadata_cache_ttl = \"" + ttl + "s\"\n")
			out, err := e.Cmd("run", "--", agent, "metadata", ttl).CombinedOutput()
			if err != nil {
				t.Fatalf("metadata probe: %v\n%s", err, out)
			}
			r := parseResults(string(out))
			for _, name := range []string{"metadata_repeated_read_consistent", "metadata_external_write", "metadata_external_create", "metadata_external_rename", "metadata_external_delete", "metadata_ok"} {
				if r[name] != "true" {
					t.Errorf("%s = %q\n%s", name, r[name], out)
				}
			}
			left, err := filepath.Glob(filepath.Join(e.Project, ".waldo-metadata-*"))
			if err != nil || len(left) != 0 {
				t.Errorf("metadata probe left project files: %v (error %v)", left, err)
			}
			waitNoMounts(t)
		})
	}
}

func waitNoMounts(t *testing.T) {
	t.Helper()
	for i := 0; i < 50; i++ {
		if len(harness.Mounts()) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("mounts left behind: %v", harness.Mounts())
}

func TestPipesAndExitCode(t *testing.T) {
	e, _ := setup(t)
	cmd := e.Cmd("run", "--", "/bin/bash", "-c", "tr a-z A-Z; uname -s >&2; exit 9")
	cmd.Stdin = strings.NewReader("piped input\n")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.Run()
	if got := cmd.ProcessState.ExitCode(); got != 9 {
		t.Fatalf("exit %d, stderr %s", got, errb.String())
	}
	if out.String() != "PIPED INPUT\n" {
		t.Errorf("stdout %q", out.String())
	}
	if !strings.Contains(errb.String(), "Linux") {
		t.Errorf("stderr %q", errb.String())
	}
}

// M2 acceptance from DESIGN.md, verbatim.
func TestDesignM2(t *testing.T) {
	e, _ := setup(t)
	hn, _ := os.Hostname()
	cmd := e.Cmd("run", "--", "/bin/bash", "-c", "bash -c 'uname -n; exit 3'")
	out, _ := cmd.Output()
	if cmd.ProcessState.ExitCode() != 3 || strings.TrimSpace(string(out)) != hn {
		t.Fatalf("got %q / %d", out, cmd.ProcessState.ExitCode())
	}
}

func TestTTY(t *testing.T) {
	e, agent := setup(t)
	cmd := e.Cmd("run", "--", agent, "tty")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 33, Cols: 111})
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()
	var buf safeBuf
	go io.Copy(&buf, ptmx)
	waitFor(t, &buf, "TYPE> ", 30*time.Second)
	ptmx.Write([]byte("abc\r"))
	waitFor(t, &buf, "RESULT typed", 10*time.Second)
	cmd.Wait()
	out := buf.String()
	r := parseResults(out)
	if r["isatty"] != "true true" || r["size"] != "111x33" || r["typed"] != "abc" {
		t.Errorf("results %v\n%s", r, out)
	}
	if !strings.Contains(out, "remote-tty-yes") || !strings.Contains(out, "33 111") {
		t.Errorf("forwarded command did not get a matching pty:\n%s", out)
	}
	if c := cmd.ProcessState.ExitCode(); c != 5 {
		t.Errorf("exit %d", c)
	}
}

// An interactive shell as the agent: every command it runs goes to A.
func TestInteractiveShell(t *testing.T) {
	e, _ := setup(t)
	cmd := e.Cmd("run", "--", "/bin/bash", "--norc", "--noprofile", "-i")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 30, Cols: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()
	var buf safeBuf
	go io.Copy(&buf, ptmx)
	ptmx.Write([]byte("PS1='P> '\r"))
	waitFor(t, &buf, "P> ", 30*time.Second)
	ptmx.Write([]byte("echo $((6*7))x; readlink /proc/self/ns/net\r"))
	waitFor(t, &buf, "42x", 10*time.Second)
	pty.Setsize(ptmx, &pty.Winsize{Rows: 40, Cols: 120})
	time.Sleep(300 * time.Millisecond)
	ptmx.Write([]byte("stty size\r"))
	waitFor(t, &buf, "40 120", 10*time.Second)
	ptmx.Write([]byte("exit 6\r"))
	cmd.Wait()
	if c := cmd.ProcessState.ExitCode(); c != 6 {
		t.Errorf("exit %d\n%s", c, buf.String())
	}
	if strings.Contains(buf.String(), hostNetns(t)) {
		t.Errorf("a command ran in the host netns:\n%s", buf.String())
	}
}

func killSessionSSH(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	out, err := exec.Command("pgrep", "-P", strconv.Itoa(cmd.Process.Pid), "-x", "ssh").Output()
	if err != nil {
		t.Fatalf("find session ssh: %v", err)
	}
	for _, value := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(value)
		if err != nil {
			t.Fatal(err)
		}
		process, _ := os.FindProcess(pid)
		if err := process.Kill(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReconnectAfterBrokenLink(t *testing.T) {
	e, agent := setup(t)
	cmd := e.Cmd("run", "--", agent, "reconnect")
	var output safeBuf
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	waitFor(t, &output, "RESULT pid_before", 30*time.Second)
	for i := 0; ; i++ {
		if harness.Exists(filepath.Join(e.Project, "reconnect-ready")) {
			break
		}
		if i > 200 {
			t.Fatalf("exec never started: %s", output.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	killSessionSSH(t, cmd)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reconnected agent: %v\n%s", err, output.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("session did not reconnect: %s", output.String())
	}
	r := parseResults(output.String())
	if r["pid_before"] == "" || r["pid_before"] != r["pid_after"] || r["inflight_exec"] != "completed/0" || r["open_handle"] != "hello from Afalse" {
		t.Fatalf("original session was not preserved: %v\n%s", r, output.String())
	}
	count, _ := os.ReadFile(filepath.Join(e.Project, "reconnect-exec-count"))
	if string(count) != "started\n" {
		t.Fatalf("command replayed: %q", count)
	}
	result, _ := os.ReadFile(filepath.Join(e.Project, "reconnect-result"))
	if string(result) != "same session\n" {
		t.Fatalf("file projection failed after reconnect: %q", result)
	}
	waitNoMounts(t)
}

func TestCleanupAfterReconnectWindow(t *testing.T) {
	e, _ := setup(t)
	cmd := e.Cmd("run", "--", "/bin/bash", "-c", "echo READY; sleep 300")
	var output safeBuf
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	waitFor(t, &output, "READY", 30*time.Second)
	// Keep every subsequent SSH attempt offline, while leaving the original
	// private sshd alive so cleanup behavior is tested independently.
	if err := os.WriteFile(e.SSHConf, []byte("Host *\n  ProxyCommand /bin/false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	killSessionSSH(t, cmd)
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(110 * time.Second):
		cmd.Process.Kill()
		t.Fatal("waldo did not exit after the reconnect window expired")
	}
	if cmd.ProcessState.ExitCode() != 255 {
		t.Fatalf("exit %d, want 255\n%s", cmd.ProcessState.ExitCode(), output.String())
	}
	waitNoMounts(t)
	base := filepath.Join(os.TempDir(), fmt.Sprintf("waldo-%d", os.Getuid()))
	for i := 0; ; i++ {
		ents, _ := os.ReadDir(base)
		if len(ents) == 0 {
			break
		}
		if i > 100 {
			t.Fatalf("B run dir left behind: %v", ents)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}
func (s *safeBuf) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.buf.String() }

func waitFor(t *testing.T, b *safeBuf, s string, d time.Duration) {
	t.Helper()
	dl := time.Now().Add(d)
	for time.Now().Before(dl) {
		if strings.Contains(b.String(), s) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %q; output:\n%s", s, b.String())
}

// A compromised B writes escape sequences to stderr (which ssh relays to the
// user's terminal): they must be filtered like the agent's own output.
func TestHostStderrFiltered(t *testing.T) {
	e, _ := setup(t)
	// First run installs the real binary on B.
	if out, err := e.Cmd("run", "--", "/bin/true").CombinedOutput(); err != nil {
		t.Fatalf("install run: %v %s", err, out)
	}
	ver, _ := exec.Command(e.Bin, "version").Output()
	evil := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = version ]; then echo %s; exit 0; fi\nprintf 'EVIL\\033]52;c;ZXZpbA==\\007\\033P+q\\033\\\\TEXT\\n' >&2\nexit 1\n", strings.TrimSpace(string(ver)))
	if err := os.WriteFile(filepath.Join(e.BData, "bin", "waldo"), []byte(evil), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := e.Cmd("run", "--", "/bin/true")
	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()
	var buf safeBuf
	go io.Copy(&buf, ptmx)
	cmd.Wait()
	time.Sleep(200 * time.Millisecond)
	out := buf.String()
	if !strings.Contains(out, "EVIL") || !strings.Contains(out, "TEXT") {
		t.Fatalf("expected the plain text to come through:\n%q", out)
	}
	if strings.Contains(out, "\x1b]52") || strings.Contains(out, "\x1bP") {
		t.Errorf("escape sequence from B reached the terminal: %q", out)
	}
}

// A and B have different HOME paths: the capsule must use A's HOME string
// for B's agent state, and commands must see the A sandbox's HOME.
func TestDifferentHome(t *testing.T) {
	e, _ := setup(t)
	ahome := filepath.Join(e.Root, "ahome")
	proj := filepath.Join(ahome, "work", "p")
	os.MkdirAll(proj, 0o755)
	exec.Command("git", "init", "-q", proj).Run()
	cmd := e.Cmd("run", "--", "/bin/bash", "-c",
		`echo "agent HOME=$HOME"; : > ~/agent-was-here; bash -c 'echo "cmd HOME=$HOME"; touch ~/cmd-was-here; pwd'`)
	cmd.Dir = proj
	cmd.Env = append(cmd.Env, "HOME="+ahome, "PWD="+proj)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"agent HOME=" + ahome, "cmd HOME=" + ahome, proj} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if !harness.Exists(filepath.Join(e.BData, "home", "agent-was-here")) {
		t.Error("agent HOME is not B's state dir")
	}
	if !harness.Exists(filepath.Join(e.AData, "sandbox-home", "cmd-was-here")) {
		t.Error("command HOME is not A's sandbox home")
	}
	if harness.Exists(filepath.Join(ahome, "agent-was-here")) || harness.Exists(filepath.Join(ahome, "cmd-was-here")) {
		t.Error("something wrote into A's real HOME")
	}
}

// setup writes a fresh host line, adopts existing state by default, preserves
// state through opt-out/opt-in, and `waldo ports` sees a live session.
func TestSetupAdoptAndPorts(t *testing.T) {
	e, agent := setup(t)
	home, _ := os.UserHomeDir()
	tag := strconv.Itoa(os.Getpid())
	sdir, sfile := ".waldo-e2e-state-"+tag, ".waldo-e2e-state-"+tag+".json"
	os.MkdirAll(filepath.Join(home, sdir), 0o700)
	os.WriteFile(filepath.Join(home, sfile), []byte("original"), 0o600)
	defer os.RemoveAll(filepath.Join(home, sdir))
	defer os.Remove(filepath.Join(home, sfile))

	cfg := fmt.Sprintf("ssh_args = [\"-F\", %q]\nremote_dir = %q\n\n[agent.claude]\nstate = []\n[agent.codex]\nstate = []\n[agent.fake]\nbin = %q\nstate = [\"~/%s\", \"~/%s\"]\n",
		e.SSHConf, e.BData, agent, sdir, sfile)
	os.WriteFile(filepath.Join(e.AConfig, "config.toml"), []byte(cfg), 0o600)
	out, err := e.Cmd("setup", "waldo-e2e").CombinedOutput()
	t.Logf("setup:\n%s", out)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(e.AConfig, "config.toml"))
	if !strings.HasPrefix(string(b), `host = "waldo-e2e"`) || !strings.Contains(string(b), "remote_dir") {
		t.Errorf("config after setup:\n%s", b)
	}

	out, err = e.Cmd("fake", "adopt", sdir, sfile).CombinedOutput()
	r := parseResults(string(out))
	if err != nil || r["rename_state_file"] != "true" || r["write_state_dir"] != "true" {
		t.Fatalf("adopt run: %v %v\n%s", err, r, out)
	}
	if !harness.Exists(filepath.Join(home, sdir, "fromcapsule.txt")) {
		t.Error("adopted directory is not shared with B's native state")
	}
	if b, _ := os.ReadFile(filepath.Join(home, sfile)); string(b) != "original" {
		t.Errorf("adopted file should be a copy, native one changed to %q", b)
	}

	// Opting out stops directory sharing, while retaining the independent
	// directory and the previously copied file in the capsule's persistent HOME.
	capsuleDir := filepath.Join(e.BData, "home", sdir)
	if err := os.MkdirAll(capsuleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(path, contents string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	checkContents := func(path, want string) {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil || string(b) != want {
			t.Fatalf("state %s = %q (%v), want %q", path, b, err, want)
		}
	}
	runSetup := func(flag string) {
		t.Helper()
		out, err := e.Cmd("setup", "waldo-e2e", flag).CombinedOutput()
		if err != nil {
			t.Fatalf("setup %s: %v\n%s", flag, err, out)
		}
	}
	runStateProbe := func() {
		t.Helper()
		out, err := e.Cmd("fake", "adopt", sdir, sfile).CombinedOutput()
		r := parseResults(string(out))
		if err != nil || r["rename_state_file"] != "true" || r["write_state_dir"] != "true" {
			t.Fatalf("state probe: %v %v\n%s", err, r, out)
		}
	}
	nativeMarker := filepath.Join(home, sdir, "fromcapsule.txt")
	capsuleMarker := filepath.Join(capsuleDir, "fromcapsule.txt")
	independent := filepath.Join(capsuleDir, "independent.txt")
	copied := filepath.Join(e.BData, "home", sfile)
	write(nativeMarker, "native-before-optout")
	write(capsuleMarker, "capsule-before-optout")
	write(independent, "keep-independent-state")
	checkContents(copied, "rewritten-in-capsule")
	runSetup("--adopt-login=false")
	checkContents(copied, "rewritten-in-capsule")
	checkContents(independent, "keep-independent-state")
	checkContents(capsuleMarker, "capsule-before-optout")
	runStateProbe()
	checkContents(nativeMarker, "native-before-optout")
	checkContents(capsuleMarker, "x")

	// Explicit true remains compatible, resumes directory sharing and never
	// overwrites a nonempty single-file copy with B's native state file.
	runSetup("--adopt-login=true")
	checkContents(copied, "rewritten-in-capsule")
	checkContents(independent, "keep-independent-state")
	write(capsuleMarker, "capsule-before-reenable")
	runStateProbe()
	checkContents(nativeMarker, "x")
	checkContents(capsuleMarker, "capsule-before-reenable")
	checkContents(filepath.Join(home, sfile), "original")

	port := harness.FreePort(t)
	cmd := e.Cmd("run", "--", "/bin/bash", "-c", fmt.Sprintf("python3 -m http.server %d --bind 127.0.0.1 >/dev/null 2>&1 & sleep 4", port))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var ports string
	for i := 0; i < 40; i++ {
		time.Sleep(100 * time.Millisecond)
		o, _ := e.Cmd("ports").Output()
		if ports = string(o); strings.Contains(ports, fmt.Sprintf("127.0.0.1:%d", port)) {
			break
		}
	}
	cmd.Wait()
	if !strings.Contains(ports, fmt.Sprintf("127.0.0.1:%d", port)) {
		t.Errorf("waldo ports did not show the forwarded port:\n%s", ports)
	}
}
