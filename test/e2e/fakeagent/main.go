// Command fakeagent stands in for a real CLI agent in the e2e tests. Like a
// real agent it reads and writes files in-process and starts subprocesses;
// it prints "RESULT <name> <value>" lines that the test asserts on.
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

func result(name string, v any) { fmt.Printf("RESULT %s %v\n", name, v) }

func run(stdin string, name string, args ...string) (string, int) {
	cmd := exec.Command(name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		return err.Error(), -1
	}
	return strings.TrimSpace(out.String()), code
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: fakeagent probe|tty")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "probe":
		probe()
	case "tty":
		tty()
	case "adopt":
		adopt(os.Args[2], os.Args[3])
	}
}

func probe() {
	self, _ := os.Readlink("/proc/self/ns/net")
	result("agent_netns", self)
	host, _ := os.Hostname()
	result("agent_hostname", host)

	// M2: PATH lookup -> stub -> A; output and exit code.
	out, code := run("", "bash", "-c", "readlink /proc/self/ns/net; exit 3")
	result("bash_netns", out)
	result("bash_code", code)

	// Hard-coded absolute interpreter path.
	out, code = run("", "/bin/sh", "-c", "echo abs-ok")
	result("abs_sh", fmt.Sprintf("%s/%d", out, code))

	// The capsule has no real tools: /usr/bin/bash is a stub.
	t, _ := os.Readlink("/usr/bin/bash")
	result("bash_is_stub", t == "/run/waldo/waldo")

	// File projection, in-process.
	cwd, _ := os.Getwd()
	readme, err := os.ReadFile(filepath.Join(cwd, "README"))
	result("readme", strings.TrimSpace(string(readme))+fmt.Sprint(err != nil))
	os.WriteFile(filepath.Join(cwd, "agent.tmp"), []byte("written-by-agent\n"), 0o644)
	err = os.Rename(filepath.Join(cwd, "agent.tmp"), filepath.Join(cwd, "agent.txt"))
	result("rename_err", err == nil)
	os.MkdirAll(filepath.Join(cwd, "sub", "dir"), 0o755)
	out, _ = run("", "cat", "agent.txt")
	result("cat_agent_txt", out)

	// Environment of forwarded commands.
	out, _ = run("", "env")
	result("env_has_proxy", strings.Contains(out, "HTTPS_PROXY=http://127.0.0.1:"))
	result("env_has_waldo", strings.Contains(out, "WALDO_"))
	result("env_has_secret", strings.Contains(out, "FAKE_SECRET="))

	// A "#!" script started directly: the interpreter is a stub too.
	os.WriteFile(filepath.Join(cwd, "script.sh"), []byte("#!/usr/bin/env bash\nreadlink /proc/self/ns/net\n"), 0o755)
	out, code = run("", "./script.sh")
	result("script_netns", fmt.Sprintf("%s/%d", out, code))

	// stdin streaming.
	out, _ = run("hello world\n", "tr", "a-z", "A-Z")
	result("tr", out)

	// /tmp is projected to A.
	os.WriteFile("/tmp/waldo-e2e-marker", []byte("tmp-ok"), 0o644)
	out, _ = run("", "cat", "/tmp/waldo-e2e-marker")
	result("tmp_shared", out)

	// Signals are forwarded to the process on A.
	cmd := exec.Command("bash", "-c", `trap "echo got-term; exit 7" TERM; echo started; while :; do sleep 0.1; done`)
	var sb bytes.Buffer
	cmd.Stdout = &sb
	cmd.Start()
	time.Sleep(1500 * time.Millisecond)
	cmd.Process.Signal(syscall.SIGTERM)
	cmd.Wait()
	result("sigterm", fmt.Sprintf("%s/%d", strings.Join(strings.Fields(sb.String()), ","), cmd.ProcessState.ExitCode()))

	// A killed stub takes its process group on A with it.
	os.Remove("/tmp/waldo-e2e-pid")
	cmd = exec.Command("bash", "-c", `echo $$ > /tmp/waldo-e2e-pid; exec sleep 100`)
	cmd.Start()
	for i := 0; i < 100; i++ {
		if b, _ := os.ReadFile("/tmp/waldo-e2e-pid"); len(b) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cmd.Process.Kill()
	cmd.Wait()
	time.Sleep(500 * time.Millisecond)
	out, _ = run("", "bash", "-c", `kill -0 "$(cat /tmp/waldo-e2e-pid)" 2>/dev/null && echo alive || echo dead`)
	result("killed_group", out)

	// A background server outlives the command that started it and shows up
	// on A's localhost.
	port := os.Getenv("E2E_PORT")
	start := time.Now()
	out, code = run("", "bash", "-c", "python3 -m http.server "+port+" --bind 127.0.0.1 >/dev/null 2>&1 &")
	result("bg_start", fmt.Sprintf("%d/%v", code, time.Since(start) < 5*time.Second))
	for i := 0; i < 50; i++ {
		out, _ = run("", "curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "http://127.0.0.1:"+port+"/")
		if out == "200" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	result("bg_curl_in_sandbox", out)
	fmt.Println("READY")
	for i := 0; i < 300; i++ {
		if _, err := os.Stat(filepath.Join(cwd, ".e2e-done")); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	os.Exit(42)
}

func tty() {
	fmt.Printf("RESULT isatty %v %v\r\n", term.IsTerminal(0), term.IsTerminal(1))
	w, h, _ := term.GetSize(1)
	fmt.Printf("RESULT size %dx%d\r\n", w, h)
	// A forwarded command inside a pty sees a tty on A too.
	cmd := exec.Command("bash", "-c", "test -t 0 && test -t 1 && echo remote-tty-yes || echo remote-tty-no; stty size")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Run()
	old, _ := term.MakeRaw(0)
	fmt.Print("TYPE> ")
	var line []byte
	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 || buf[0] == '\r' || buf[0] == '\n' {
			break
		}
		line = append(line, buf[0])
	}
	term.Restore(0, old)
	fmt.Printf("\r\nRESULT typed %s\r\n", line)
	os.Exit(5)
}

// adopt exercises adopted agent state: an atomic rewrite of a state file
// (tmp + rename, like most agents do) and a write into a state directory.
func adopt(dir, file string) {
	home, _ := os.UserHomeDir()
	f := filepath.Join(home, file)
	os.WriteFile(f+".tmp", []byte("rewritten-in-capsule"), 0o600)
	result("rename_state_file", os.Rename(f+".tmp", f) == nil)
	err := os.WriteFile(filepath.Join(home, dir, "fromcapsule.txt"), []byte("x"), 0o600)
	result("write_state_dir", err == nil)
}
