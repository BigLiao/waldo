// Command fakeagent stands in for a real CLI agent in the e2e tests. Like a
// real agent it reads and writes files in-process and starts subprocesses;
// it prints "RESULT <name> <value>" lines that the test asserts on.
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
		fmt.Println("usage: fakeagent probe|ownership|metadata [ttl-seconds]|tty|adopt")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "paseo":
		if err := paseoProbe(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "probe":
		probe()
	case "reconnect":
		f, err := os.Open("README")
		if err != nil {
			panic(err)
		}
		defer f.Close()
		result("pid_before", os.Getpid())
		out, code := run("", "bash", "-c", "echo started >> reconnect-exec-count; echo ready > reconnect-ready; sleep 4; printf completed")
		result("inflight_exec", fmt.Sprintf("%s/%d", out, code))
		b, err := io.ReadAll(f)
		result("open_handle", strings.TrimSpace(string(b))+fmt.Sprint(err != nil))
		result("pid_after", os.Getpid())
		if err := os.WriteFile("reconnect-result", []byte("same session\n"), 0o644); err != nil {
			panic(err)
		}
	case "ownership":
		if err := ownership(); err != nil {
			fmt.Fprintln(os.Stderr, "ownership probe:", err)
			os.Exit(1)
		}
	case "metadata":
		if err := metadata(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "metadata probe:", err)
			os.Exit(1)
		}
	case "cache":
		if err := cacheProbe(); err != nil {
			fmt.Fprintln(os.Stderr, "cache probe:", err)
			os.Exit(1)
		}
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
	result("readme_owner", ownedBySelf(filepath.Join(cwd, "README")))
	os.WriteFile(filepath.Join(cwd, "agent.tmp"), []byte("written-by-agent\n"), 0o644)
	err = os.Rename(filepath.Join(cwd, "agent.tmp"), filepath.Join(cwd, "agent.txt"))
	result("rename_err", err == nil)
	result("agent_file_owner", ownedBySelf(filepath.Join(cwd, "agent.txt")))
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
	probePrivateTmp()

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

func ownedBySelf(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	id, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(id.Uid) == os.Getuid() && int(id.Gid) == os.Getgid()
}

func probePrivateTmp() {
	dir, err := os.MkdirTemp("/tmp", ".waldo-ownership-")
	if err != nil {
		result("tmp_private_owner", false)
		result("tmp_private_mode", false)
		return
	}
	defer os.RemoveAll(dir)
	// pkg/sftp's OS server currently creates directories as 0755. Exercise
	// explicit chmod before checking the private mode expected by agents.
	if err := os.Chmod(dir, 0o700); err != nil {
		result("tmp_private_owner", ownedBySelf(dir))
		result("tmp_private_mode", false)
		return
	}
	result("tmp_private_owner", ownedBySelf(dir))
	st, err := os.Stat(dir)
	result("tmp_private_mode", err == nil && st.Mode().Perm() == 0o700)
}

// ownership is a small, model-free probe for real hosts with different UIDs.
// All filesystem calls happen in the agent process on B; spawning stat would
// exercise A's forwarded command environment and miss the ownership mismatch.
// The only writes are inside fresh, uniquely named directories, removed on exit.
func ownership() error {
	result("process_uid", os.Getuid())
	result("process_gid", os.Getgid())
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	result("project_owner", ownedBySelf(cwd))
	if !ownedBySelf(cwd) {
		return fmt.Errorf("project directory is not owned by the agent UID/GID: %s", cwd)
	}
	for _, tc := range []struct{ name, parent string }{{"project", cwd}, {"tmp", "/tmp"}} {
		if err := ownershipFiles(tc.name, tc.parent); err != nil {
			return err
		}
	}
	result("ownership_ok", true)
	return nil
}

func ownershipFiles(name, parent string) error {
	dir, err := os.MkdirTemp(parent, ".waldo-ownership-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	// The current SFTP server ignores mkdir's requested mode; set it
	// explicitly so this probe checks ownership translation and chmod.
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	st, err := os.Stat(dir)
	if err != nil {
		return err
	}
	ownerOK := ownedBySelf(dir)
	modeOK := st.Mode().Perm() == 0o700
	result(name+"_private_owner", ownerOK)
	result(name+"_private_mode", modeOK)
	if !ownerOK || !modeOK {
		id, _ := st.Sys().(*syscall.Stat_t)
		if id != nil {
			return fmt.Errorf("%s private directory: owner=%d:%d mode=%04o expected=%d:%d/0700", name, id.Uid, id.Gid, st.Mode().Perm(), os.Getuid(), os.Getgid())
		}
		return fmt.Errorf("%s private directory: unknown ownership", name)
	}
	src, dst := filepath.Join(dir, "file.tmp"), filepath.Join(dir, "file.txt")
	const contents = "waldo ownership probe\n"
	if err := os.WriteFile(src, []byte(contents), 0o600); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		return err
	}
	st, err = os.Stat(dst)
	if err != nil {
		return err
	}
	ownerOK = ownedBySelf(dst)
	modeOK = st.Mode().Perm() == 0o600
	readOK := string(b) == contents
	result(name+"_file_owner", ownerOK)
	result(name+"_file_mode", modeOK)
	result(name+"_write_rename", readOK)
	if !ownerOK || !modeOK || !readOK {
		return fmt.Errorf("%s file: owner=%v mode=%04o readback=%v", name, ownerOK, st.Mode().Perm(), readOK)
	}
	return nil
}

// metadata records timings without asserting a performance threshold, then
// checks that changes made by commands on A become visible to file calls on B.
// Pass the configured metadata TTL in seconds (default 10) to bound convergence.
func metadata(args []string) (retErr error) {
	ttlSeconds := 10
	if len(args) > 1 {
		return fmt.Errorf("usage: fakeagent metadata [ttl-seconds]")
	}
	if len(args) == 1 {
		value, err := strconv.Atoi(args[0])
		if err != nil || value < 0 || value > 60 {
			return fmt.Errorf("metadata TTL must be an integer from 0 to 60 seconds")
		}
		ttlSeconds = value
	}
	result("metadata_ttl_seconds", ttlSeconds)
	path := "README.md"
	if _, err := os.Stat(path); os.IsNotExist(err) {
		path = "README" // The loopback harness uses README without an extension.
	}
	start := time.Now()
	for i := 0; i < 20; i++ {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("stat benchmark: %w", err)
		}
	}
	result("metadata_stat_count", 20)
	result("metadata_stat_ms", fmt.Sprintf("%.3f", float64(time.Since(start))/float64(time.Millisecond)))
	want, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	start = time.Now()
	for i := 0; i < 20; i++ {
		contents, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(contents, want) {
			return fmt.Errorf("repeated read changed for %s: %v", path, err)
		}
	}
	result("metadata_read_count", 20)
	result("metadata_read_ms", fmt.Sprintf("%.3f", float64(time.Since(start))/float64(time.Millisecond)))
	result("metadata_repeated_read_consistent", true)

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp(cwd, ".waldo-metadata-")
	if err != nil {
		return err
	}
	// Clean up on A, where an expired B lookup cannot hide a newly made file.
	// The path is this probe's fresh directory and travels as a separate argv.
	defer func() {
		if err := metadataCommand(`rm -rf -- "$1"`, dir); err != nil && retErr == nil {
			retErr = fmt.Errorf("cleanup: %w", err)
		}
	}()
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	current := filepath.Join(dir, "current.txt")
	if err := os.WriteFile(current, []byte("old"), 0o600); err != nil {
		return err
	}
	if _, err := os.Stat(current); err != nil {
		return err
	}
	if _, err := os.ReadFile(current); err != nil {
		return err
	}
	const changed = "updated in place by a command on A\n"
	if err := metadataCommand(`printf '%s' "$2" > "$1"`, current, changed); err != nil {
		return err
	}
	timeout := time.Duration(ttlSeconds)*time.Second + 2*time.Second
	if err := awaitMetadata("external_write", timeout, func() bool {
		st, err := os.Stat(current)
		return err == nil && st.Size() == int64(len(changed)) && metadataContents(current, changed)
	}); err != nil {
		return err
	}

	created, renamed := filepath.Join(dir, "created.txt"), filepath.Join(dir, "renamed.txt")
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		return fmt.Errorf("expected an absent create target, got %v", err)
	}
	if _, err := os.ReadDir(dir); err != nil {
		return err
	}
	if err := metadataCommand(`printf '%s' "$2" > "$1"`, created, "created on A"); err != nil {
		return err
	}
	if err := awaitMetadata("external_create", timeout, func() bool {
		return metadataContents(created, "created on A") && metadataDirectory(dir, "created.txt", "")
	}); err != nil {
		return err
	}
	if err := metadataCommand(`mv -- "$1" "$2"`, created, renamed); err != nil {
		return err
	}
	if err := awaitMetadata("external_rename", timeout, func() bool {
		_, err := os.Stat(created)
		return os.IsNotExist(err) && metadataContents(renamed, "created on A") && metadataDirectory(dir, "renamed.txt", "created.txt")
	}); err != nil {
		return err
	}
	if err := metadataCommand(`rm -- "$1"`, renamed); err != nil {
		return err
	}
	if err := awaitMetadata("external_delete", timeout, func() bool {
		_, err := os.Stat(renamed)
		return os.IsNotExist(err) && metadataDirectory(dir, "", "renamed.txt")
	}); err != nil {
		return err
	}
	result("metadata_ok", true)
	return nil
}

func metadataCommand(script string, args ...string) error {
	argv := append([]string{"-c", script, "waldo-metadata"}, args...)
	out, code := run("", "bash", argv...)
	if code != 0 {
		return fmt.Errorf("A command failed with status %d: %s", code, out)
	}
	return nil
}

func awaitMetadata(name string, timeout time.Duration, check func() bool) error {
	deadline := time.Now().Add(timeout)
	for {
		if check() {
			result("metadata_"+name, true)
			return nil
		}
		if !time.Now().Before(deadline) {
			result("metadata_"+name, false)
			return fmt.Errorf("%s was not visible within %s", name, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func metadataContents(path, want string) bool {
	contents, err := os.ReadFile(path)
	return err == nil && string(contents) == want
}

func metadataDirectory(dir, present, absent string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	found := present == ""
	for _, entry := range entries {
		if entry.Name() == absent {
			return false
		}
		if entry.Name() == present {
			found = true
		}
	}
	return found
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
