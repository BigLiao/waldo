package bootstrap

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// Every command visible to the installer is a fixture. These tests never invoke
// the host's package manager or sudo, even when the tests themselves run as root.
type fixture struct {
	t   *testing.T
	bin string
	log string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	bin := t.TempDir()
	f := &fixture{t: t, bin: bin, log: filepath.Join(t.TempDir(), "commands")}
	t.Setenv("PATH", bin)
	t.Setenv("FAKE_BIN", bin)
	t.Setenv("BOOTSTRAP_LOG", f.log)
	t.Setenv("TEST_UID", "0")
	t.Setenv("TEST_OS", "Linux")
	t.Setenv("INSTALL_COMMANDS", "")
	t.Setenv("INSTALL_FAIL", "0")
	f.write("id", `printf '%s\n' "$TEST_UID"`)
	f.write("uname", `printf '%s\n' "$TEST_OS"`)
	f.link("sh", "/bin/sh")
	f.link("env", "/usr/bin/env")
	return f
}

func (f *fixture) write(name, body string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.bin, name), []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0o755); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) link(name, target string) {
	f.t.Helper()
	if err := os.Symlink(target, filepath.Join(f.bin, name)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) present(names ...string) {
	f.t.Helper()
	for _, name := range names {
		f.link(name, "/bin/true")
	}
}

const recordCommand = `{
    printf '%s' "${0##*/}"
    printf ' <%s>' "$@"
    printf '\n'
} >> "$BOOTSTRAP_LOG"
`

func (f *fixture) manager(name string) {
	f.t.Helper()
	f.write(name, recordCommand+`
if [ "$INSTALL_FAIL" != 0 ]; then exit 23; fi
if [ "${1:-}" = update ]; then exit 0; fi
for command in $INSTALL_COMMANDS; do
    /bin/ln -sf /bin/true "$FAKE_BIN/$command"
done`)
}

func (f *fixture) sudo(fail bool) {
	f.t.Helper()
	body := recordCommand
	if fail {
		body += "exit 1"
	} else {
		body += `if [ "${1:-}" = -n ]; then shift; fi
if [ "${1:-}" = -- ]; then shift; fi
exec "$@"`
	}
	f.write("sudo", body)
}

func (f *fixture) calls() string {
	f.t.Helper()
	b, err := os.ReadFile(f.log)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func (f *fixture) run(role string, needs ...string) (string, error) {
	f.t.Helper()
	var out bytes.Buffer
	err := Install(role, needs, strings.NewReader(""), &out, &out)
	return out.String(), err
}

func TestAlreadyInstalledDoesNotRequirePrivilegesOrPackageManager(t *testing.T) {
	f := newFixture(t)
	t.Setenv("TEST_UID", "1000")
	f.present("bwrap", "sshfs", "fusermount3")
	out, err := f.run("b")
	if err != nil || !strings.Contains(out, "依赖已齐全") {
		t.Fatalf("output %q, error %v", out, err)
	}
	if got := f.calls(); got != "" {
		t.Fatalf("unexpected command: %s", got)
	}
}

func TestAPTInstallsOnlyMissingDependenciesAndIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.manager("apt-get")
	f.present("ssh", "bash")
	t.Setenv("INSTALL_COMMANDS", "bwrap rg git")
	out, err := f.run("a", "rg", "git", "bash", "rg")
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	want := "apt-get <update>\napt-get <install> <-y> <bubblewrap> <ripgrep> <git>\n"
	if got := f.calls(); got != want {
		t.Fatalf("commands:\n%swant:\n%s", got, want)
	}
	out, err = f.run("a", "rg", "git", "bash")
	if err != nil || !strings.Contains(out, "依赖已齐全") || f.calls() != want {
		t.Fatalf("repeat installed packages: %v / %q / %q", err, out, f.calls())
	}
}

func TestPackageManagerMappings(t *testing.T) {
	for _, tc := range []struct {
		manager string
		want    string
	}{
		{"apt-get", "apt-get <update>\napt-get <install> <-y> <bubblewrap> <sshfs> <fuse3>\n"},
		{"dnf", "dnf <install> <-y> <bubblewrap> <fuse-sshfs> <fuse3>\n"},
		{"yum", "yum <install> <-y> <bubblewrap> <fuse-sshfs> <fuse3>\n"},
		{"pacman", "pacman <-S> <--needed> <--noconfirm> <bubblewrap> <sshfs> <fuse3>\n"},
		{"apk", "apk <add> <bubblewrap> <sshfs> <fuse3>\n"},
	} {
		t.Run(tc.manager, func(t *testing.T) {
			f := newFixture(t)
			f.manager(tc.manager)
			t.Setenv("INSTALL_COMMANDS", "bwrap sshfs fusermount3")
			out, err := f.run("b")
			if err != nil || f.calls() != tc.want {
				t.Fatalf("error %v, output %q, calls %q, want %q", err, out, f.calls(), tc.want)
			}
		})
	}
}

func TestSSHClientPackageMappings(t *testing.T) {
	for _, tc := range []struct{ manager, pkg string }{
		{"apt-get", "openssh-client"},
		{"dnf", "openssh-clients"},
		{"yum", "openssh-clients"},
		{"pacman", "openssh"},
		{"apk", "openssh-client"},
	} {
		t.Run(tc.manager, func(t *testing.T) {
			f := newFixture(t)
			f.manager(tc.manager)
			f.present("bwrap")
			t.Setenv("INSTALL_COMMANDS", "ssh")
			out, err := f.run("a")
			if err != nil || !strings.Contains(f.calls(), "<"+tc.pkg+">") {
				t.Fatalf("%v / %s / %s", err, out, f.calls())
			}
		})
	}
}

func TestNoninteractiveSudoDoesNotReadAPassword(t *testing.T) {
	f := newFixture(t)
	f.manager("dnf")
	f.sudo(false)
	f.present("ssh")
	t.Setenv("TEST_UID", "1000")
	t.Setenv("INSTALL_COMMANDS", "bwrap")
	out, err := f.run("a")
	if err != nil || !strings.Contains(f.calls(), "sudo <-n> <--> <dnf>") {
		t.Fatalf("%v / %s / %s", err, out, f.calls())
	}
}

func TestInteractiveSudoReadsFromTerminal(t *testing.T) {
	f := newFixture(t)
	f.manager("dnf")
	f.present("ssh")
	t.Setenv("TEST_UID", "1000")
	t.Setenv("INSTALL_COMMANDS", "bwrap")
	f.write("sudo", recordCommand+`
if [ "${1:-}" = -n ]; then exit 41; fi
if ! [ -t 0 ] || ! [ -t 1 ]; then exit 42; fi
printf 'FAKE_SUDO_PASSWORD> '
IFS= read -r password
if [ "$password" != fixture-password ]; then exit 43; fi
printf '%s\n' 'sudo-read-terminal-input' >> "$BOOTSTRAP_LOG"
if [ "${1:-}" = -- ]; then shift; fi
exec "$@"`)
	command, err := Command("a", nil)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", command)
	terminal, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	defer cmd.Process.Kill()
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	readDone := make(chan string, 1)
	go func() {
		out, _ := io.ReadAll(terminal) // Linux PTYs report EIO after the slave closes.
		readDone <- string(out)
	}()
	// A terminal buffers this line until fake sudo reaches its password read.
	if _, err := terminal.Write([]byte("fixture-password\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("interactive installer failed: %v; calls %s", err, f.calls())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interactive installer did not consume terminal input")
	}
	select {
	case out := <-readDone:
		if !strings.Contains(out, "FAKE_SUDO_PASSWORD>") || !strings.Contains(out, "依赖安装完成") {
			t.Fatalf("unexpected terminal output: %q", out)
		}
	case <-time.After(time.Second):
		t.Fatal("installer left the terminal open")
	}
	calls := f.calls()
	if strings.Contains(calls, "<-n>") || !strings.Contains(calls, "sudo <--> <dnf>") || !strings.Contains(calls, "sudo-read-terminal-input") {
		t.Fatalf("sudo did not use interactive terminal input: %s", calls)
	}
}

func TestInstallationFailuresRemainFailures(t *testing.T) {
	for _, mode := range []string{"no-sudo", "sudo-refused", "manager-failed", "command-still-missing", "unknown-manager", "unsupported-os", "unknown-command"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.present("ssh")
			if mode != "unknown-manager" {
				f.manager("dnf")
			}
			var needs []string
			want := "依赖安装失败"
			switch mode {
			case "no-sudo":
				t.Setenv("TEST_UID", "1000")
			case "sudo-refused":
				t.Setenv("TEST_UID", "1000")
				f.sudo(true)
			case "manager-failed":
				t.Setenv("INSTALL_FAIL", "1")
			case "command-still-missing":
				want = "安装后仍找不到命令"
			case "unknown-manager":
				want = "未找到支持的包管理器"
			case "unsupported-os":
				t.Setenv("TEST_OS", "Darwin")
				want = "只支持 Linux"
			case "unknown-command":
				needs = []string{"custom-agent-tool"}
				want = "无法自动安装依赖命令 custom-agent-tool"
			}
			out, err := f.run("a", needs...)
			if err == nil || !strings.Contains(out, want) || strings.Contains(out, "依赖安装完成") {
				t.Fatalf("expected %s failure, got %v / %s", mode, err, out)
			}
			if mode == "no-sudo" || mode == "sudo-refused" || mode == "manager-failed" {
				if !strings.Contains(out, "sudo dnf install -y bubblewrap") {
					t.Fatalf("missing manual recovery command: %s", out)
				}
			}
			if mode == "no-sudo" || mode == "sudo-refused" || mode == "unknown-command" || mode == "unsupported-os" {
				if strings.Contains(f.calls(), "dnf <") {
					t.Fatalf("package manager must not run: %s", f.calls())
				}
			}
		})
	}
}

func TestAPTUpdateFailureStopsBeforeInstall(t *testing.T) {
	f := newFixture(t)
	f.manager("apt-get")
	t.Setenv("INSTALL_FAIL", "1")
	out, err := f.run("b")
	if err == nil || f.calls() != "apt-get <update>\n" {
		t.Fatalf("%v / %s / %s", err, out, f.calls())
	}
}

func TestCommandQuotesArgumentsAndRejectsInvalidRoles(t *testing.T) {
	f := newFixture(t)
	f.present("ssh", "bwrap")
	marker := filepath.Join(t.TempDir(), "injected")
	command, err := Command("a", []string{"bad'; /bin/touch '" + marker})
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/bin/sh", "-c", command).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "无效的依赖命令名") {
		t.Fatalf("%v / %s", err, out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("injected command executed: %v", err)
	}
	if _, err := Command("invalid", nil); err == nil {
		t.Fatal("expected invalid role error")
	}
}
