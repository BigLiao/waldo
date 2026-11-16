//go:build darwin

package native_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pkg/sftp"

	"waldo/internal/config"
	"waldo/internal/hosttools"
	"waldo/internal/proto"
	"waldo/internal/sandbox"
	"waldo/internal/serve"
	"waldo/internal/session"
	"waldo/internal/util"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "native-listen":
			ln, err := net.Listen("tcp4", "127.0.0.1:"+os.Args[2])
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "native-listener") }))
			os.Exit(0)
		case "sandbox-serve":
			os.Exit(serve.Main())
		case "host-file-tool":
			os.Exit(hosttools.Main(os.Args[2:]))
		}
	}
	os.Exit(m.Run())
}

func TestNativeProtectsExecutableInsideWritableProject(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"project/bin", "home", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(root, "project/bin/waldo")
	if err := os.WriteFile(file, []byte("trusted-executable"), 0600); err != nil {
		t.Fatal(err)
	}
	profile, err := sandbox.SeatbeltProfile(sandbox.Spec{Project: filepath.Join(root, "project"), SandboxHome: filepath.Join(root, "home"), TempDir: filepath.Join(root, "tmp"), Exe: file, ProxyPort: 39339})
	if err != nil {
		t.Fatal(err)
	}
	for _, script := range []string{"printf bad > " + util.ShellQuote(file), "mv " + util.ShellQuote(filepath.Dir(file)) + " " + util.ShellQuote(filepath.Join(root, "project/renamed"))} {
		cmd := exec.Command("/usr/bin/sandbox-exec", "-p", profile, "/bin/sh", "-c", script)
		cmd.Dir = filepath.Join(root, "project")
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("executable replacement succeeded: %s", out)
		}
	}
	if b, _ := os.ReadFile(file); string(b) != "trusted-executable" {
		t.Fatal("trusted executable changed")
	}
}

func TestNativeContextProjectionAndReadonlySkillAlias(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, source := filepath.Join(root, "project"), filepath.Join(root, "source-skill")
	for _, p := range []string{project, source} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	const content = "native-skill-test\n"
	os.WriteFile(filepath.Join(source, "SKILL.md"), []byte(content), 0600)
	home, _ := os.UserHomeDir()
	target := filepath.Join(home, ".claude/skills/paseo-test")
	policy, err := (&config.Config{}).PolicyFor(project)
	if err != nil {
		t.Fatal(err)
	}
	policy.Ports = config.PortsMode{Mode: "off"}
	policy.InstructionFiles = []string{"AGENTS.md"}
	sb, err := session.StartSandbox(session.SandboxOptions{Home: home, Project: project, Policy: policy, DataDir: filepath.Join(root, "data"), ContextGuide: []byte("guide " + proto.ContextFile), ReadOnlyMounts: []sandbox.ReadOnlyMount{{Source: source, Target: target}}})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	if !filepath.IsAbs(sb.Facts.ContextFile) {
		t.Fatal("missing physical context path")
	}
	if out, code := run(t, sb, "cat "+util.ShellQuote(sb.Facts.ContextFile)); code != 0 || !strings.Contains(out, "guide "+sb.Facts.ContextFile) {
		t.Fatal(code, out)
	}
	if out, code := run(t, sb, "printf bad > "+util.ShellQuote(sb.Facts.ContextFile)); code == 0 {
		t.Fatal("context writable", out)
	}
	st, err := sb.Open(proto.Header{Kind: proto.KindFS})
	if err != nil {
		t.Fatal(err)
	}
	client, err := sftp.NewClientPipe(st, st)
	if err != nil {
		t.Fatal(err)
	}
	f, err := client.Open(filepath.Join(project, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(f)
	f.Close()
	client.Close()
	if err != nil || !strings.Contains(string(b), sb.Facts.ContextFile) {
		t.Fatal("wrong projected context", string(b), err)
	}
	if _, err := os.Stat(filepath.Join(project, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatal("projection changed original project")
	}
	logical := sb.ExportPath(target)
	if sb.ExportSources()[logical] == "" || sb.ExportSources()[logical] == source {
		t.Fatal("skill source was not staged")
	}
	call := func(tool string, args map[string]any) (string, int) {
		t.Helper()
		st, err := sb.Open(proto.Header{Kind: proto.KindExec, Exec: &proto.ExecReq{Tool: tool, Path: "/run/waldo/waldo", Argv: []string{"/run/waldo/waldo", "host-file-tool", tool}, Cwd: project, Env: sb.Facts.Env}})
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		st.SetDeadline(time.Now().Add(10 * time.Second))
		body, _ := json.Marshal(args)
		fw := proto.NewFrameWriter(st)
		fw.Write(proto.FStdin, body)
		fw.Write(proto.FStdinEOF, nil)
		var out strings.Builder
		for {
			kind, p, err := proto.ReadFrame(st)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case proto.FStdout, proto.FStderr:
				out.Write(p)
			case proto.FExit:
				var exit proto.Exit
				proto.DecodeJSON(p, &exit)
				return out.String(), exit.Code
			}
		}
	}
	path := filepath.Join(logical, "SKILL.md")
	if out, code := call("Read", map[string]any{"file_path": path}); code != 0 || !strings.Contains(out, "native-skill-test") {
		t.Fatal("skill alias read failed", code, out)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	if out, code := call("Write", map[string]any{"file_path": path, "content": "bad", "expected_sha256": hash}); code == 0 {
		t.Fatal("skill alias writable", out)
	}
	if b, _ := os.ReadFile(filepath.Join(source, "SKILL.md")); string(b) != content {
		t.Fatal("source skill modified")
	}
}

func start(t *testing.T, configure func(*config.Policy)) (*session.Sandbox, string, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(root, "project-café")
	if err := os.MkdirAll(filepath.Join(project, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(project, ".git/config"), []byte("protected"), 0600)
	secret := filepath.Join(root, "secret")
	os.WriteFile(secret, []byte("private-host-data"), 0600)
	policy, err := (&config.Config{}).PolicyFor(project)
	if err != nil {
		t.Fatal(err)
	}
	policy.Ports = config.PortsMode{Mode: "off"}
	if configure != nil {
		configure(policy)
	}
	home, _ := os.UserHomeDir()
	sb, err := session.StartSandbox(session.SandboxOptions{Home: home, Project: project, Policy: policy, DataDir: filepath.Join(root, "data")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sb.Close)
	return sb, project, secret
}

func run(t *testing.T, sb *session.Sandbox, command string) (string, int) {
	t.Helper()
	st, err := sb.Open(proto.Header{Kind: proto.KindExec, Exec: &proto.ExecReq{Path: "/bin/bash", Argv: []string{"bash", "--noprofile", "--norc", "-c", command}, Cwd: sb.Project, Env: sb.Facts.Env}})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.SetDeadline(time.Now().Add(15 * time.Second))
	if err := proto.NewFrameWriter(st).Write(proto.FStdinEOF, nil); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for {
		kind, p, err := proto.ReadFrame(st)
		if err != nil {
			t.Fatal(err)
		}
		switch kind {
		case proto.FStdout, proto.FStderr, proto.FError:
			out.Write(p)
		case proto.FExit:
			var exit proto.Exit
			if err := proto.DecodeJSON(p, &exit); err != nil {
				t.Fatal(err)
			}
			return out.String(), exit.Code
		}
	}
}

func TestNativeFilesAndProtectedAliases(t *testing.T) {
	sb, project, secret := start(t, nil)
	if sb.Facts.OS != "darwin" || sb.Facts.Home == os.Getenv("HOME") {
		t.Fatalf("bad native facts: %+v", sb.Facts)
	}
	out, code := run(t, sb, `printf hello > plain; cat plain; printf tmp > "$TMPDIR/probe"; cat "$TMPDIR/probe"`)
	if code != 0 || out != "hellotmp" {
		t.Fatalf("native exec: %d %q", code, out)
	}
	for _, command := range []string{
		"cat " + util.ShellQuote(secret),
		"printf stolen > " + util.ShellQuote(secret),
		"printf bad > .git/config", "printf bad > .GIT/config", "printf bad > .Git/config",
		"printf bad > .envrc", "mv .git replacement", "printf bad > .git/commondir", "printf bad > .git/config.worktree", "mkdir .git/hooks",
		"printf bad > " + util.ShellQuote(filepath.Join(strings.ReplaceAll(project, "é", "e\u0301"), ".git/config")),
	} {
		if out, code := run(t, sb, command); code == 0 {
			t.Fatalf("forbidden command succeeded: %s: %s", command, out)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(project, ".git/config")); string(b) != "protected" {
		t.Fatal("protected config changed")
	}
}

func TestNativeAuthenticatedProxyAndDirectNetworkDenial(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy token reached upstream")
		}
		io.WriteString(w, "through-policy")
	}))
	defer server.Close()
	addr := strings.TrimPrefix(server.URL, "http://")
	sb, _, _ := start(t, func(p *config.Policy) { p.Allow = []string{addr}; p.AllowPrivate = true })
	if out, code := run(t, sb, "curl --noproxy '' -fsS "+util.ShellQuote(server.URL)); code != 0 || out != "through-policy" {
		t.Fatalf("proxy: %d %s", code, out)
	}
	if out, code := run(t, sb, "curl --noproxy '*' --connect-timeout 1 -fsS "+util.ShellQuote(server.URL)); code == 0 {
		t.Fatalf("direct network succeeded: %s", out)
	}
	if out, code := run(t, sb, "curl --noproxy '' -fsS http://not-allowed.invalid"); code == 0 {
		t.Fatalf("allow-list bypassed: %s", out)
	}
}

func TestNativeSessionCleanupAndSeparateHomes(t *testing.T) {
	first, _, _ := start(t, nil)
	second, _, _ := start(t, nil)
	if first.Facts.Home == second.Facts.Home {
		t.Fatal("fixture homes overlap")
	}
	out, code := run(t, first, "sleep 120 >/dev/null 2>&1 & printf '%s' $!")
	if code != 0 {
		t.Fatalf("background command: %s", out)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	first.Close()
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatalf("background job %d survived", pid)
	}
	if out, code := run(t, second, "printf other-session-alive"); code != 0 || out != "other-session-alive" {
		t.Fatal(code, out)
	}
}

func TestLinuxWireSignalReachesDarwinProcess(t *testing.T) {
	sb, _, _ := start(t, nil)
	st, err := sb.Open(proto.Header{Kind: proto.KindExec, Exec: &proto.ExecReq{Path: "/bin/bash", Argv: []string{"bash", "--noprofile", "--norc", "-c", "trap 'exit 7' USR1; printf ready; while :; do sleep 1; done"}, Cwd: sb.Project, Env: sb.Facts.Env}})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.SetDeadline(time.Now().Add(10 * time.Second))
	fw := proto.NewFrameWriter(st)
	fw.Write(proto.FStdinEOF, nil)
	for {
		kind, p, err := proto.ReadFrame(st)
		if err != nil {
			t.Fatal(err)
		}
		if kind == proto.FStdout && strings.Contains(string(p), "ready") {
			fw.Write(proto.FSignal, []byte{10})
		}
		if kind == proto.FExit {
			var exit proto.Exit
			proto.DecodeJSON(p, &exit)
			if exit.Code != 7 {
				t.Fatal(fmt.Sprintf("USR1 mapped incorrectly: %+v", exit))
			}
			return
		}
	}
}

func TestNativeExplicitListenAndHostPort(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	blocked, _, _ := start(t, nil)
	exe, _ := os.Executable()
	command := fmt.Sprintf("%s native-listen %d", util.ShellQuote(exe), port)
	if out, code := run(t, blocked, command); code == 0 {
		t.Fatalf("unlisted listener allowed: %s", out)
	}
	allowed, _, _ := start(t, func(p *config.Policy) {
		p.Ports = config.PortsMode{Mode: "list", List: []int{port}}
		p.HostPorts = []int{port}
	})
	if out, code := run(t, allowed, command+" >\"$TMPDIR/listen.log\" 2>&1 &"); code != 0 {
		t.Fatal(code, out)
	}
	client := http.Client{Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d", port))
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(body) != "native-listener" {
				t.Fatal(string(body))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("explicit listener unavailable", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if out, code := run(t, allowed, fmt.Sprintf("curl --noproxy '*' -fsS http://127.0.0.1:%d", port)); code != 0 || out != "native-listener" {
		t.Fatal(code, out)
	}
}
