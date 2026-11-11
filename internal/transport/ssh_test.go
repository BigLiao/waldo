package transport

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/creack/pty"
)

// B must not be able to use the connection to reach back into A.
func TestHardening(t *testing.T) {
	s := &SSH{Host: "me@b", ExtraArgs: []string{"-F", "cfg"}}
	args := strings.Join(s.Args("cmd"), " ")
	for _, want := range []string{"-T", "ForwardAgent=no", "ForwardX11=no", "ClearAllForwardings=yes", "PermitLocalCommand=no", "Tunnel=no", "-e none"} {
		if !strings.Contains(args, want) {
			t.Errorf("ssh args lack %q: %s", want, args)
		}
	}
	a := s.Args("cmd")
	if a[len(a)-2] != "me@b" || a[len(a)-3] != "--" {
		t.Errorf("host must follow --: %v", a)
	}
	// User options come first so hardening options win (ssh uses the first value).
	if a[0] != "-F" {
		t.Errorf("extra args first: %v", a)
	}
}

func TestRemoteDirExpr(t *testing.T) {
	for in, want := range map[string]string{
		"":                    `"$HOME"/.local/share/waldo`,
		"~/x y":               `"$HOME"/'x y'`,
		"/srv/w":              "/srv/w",
		"/tmp/$(touch pwned)": `'/tmp/$(touch pwned)'`,
	} {
		if got := (&SSH{RemoteDir: in}).remoteDirExpr(); got != want {
			t.Errorf("%q -> %s want %s", in, got, want)
		}
	}
}

// Initialization needs a tty for password-based sudo, while protocol sessions
// must keep their existing no-tty behavior. Never invoke the real ssh here.
func TestRunInteractive(t *testing.T) {
	for _, mode := range []string{"nil", "pipe", "tty"} {
		t.Run(mode, func(t *testing.T) {
			argsPath := fakeSSH(t, `printf '%s\000' "$@" > "$WALDO_TEST_SSH_ARGS"
printf 'stdout-safe\033]52;c;c2VjcmV0\007\n'
printf 'stderr-safe\033]52;c;c2VjcmV0\007\n' >&2
`)
			var stdin *os.File
			switch mode {
			case "pipe":
				r, w, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				w.Close()
				stdin = r
			case "tty":
				master, slave, err := pty.Open()
				if err != nil {
					t.Fatal(err)
				}
				defer master.Close()
				defer slave.Close()
				stdin = slave
			}
			remoteCmd := "sh -c 'printf \"%s\\n\" literal; true'"
			s := &SSH{Host: "me@b", ExtraArgs: []string{"-F", "custom ssh config"}}
			var stdout, stderr bytes.Buffer
			if err := s.RunInteractive(remoteCmd, stdin, &stdout, &stderr); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(argsPath)
			if err != nil {
				t.Fatal(err)
			}
			args := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
			if len(args) < 5 || args[0] != "-F" || args[1] != "custom ssh config" ||
				!slices.Equal(args[len(args)-3:], []string{"--", "me@b", remoteCmd}) {
				t.Fatalf("lost SSH options or remote command boundary: %q", args)
			}
			for _, want := range []string{"ForwardAgent=no", "ForwardX11=no", "ForwardX11Trusted=no", "ClearAllForwardings=yes", "PermitLocalCommand=no", "Tunnel=no", "GSSAPIDelegateCredentials=no", "RemoteCommand=none", "SessionType=default", "-e", "none"} {
				if !slices.Contains(args, want) {
					t.Errorf("initialization lacks hardening option %q: %q", want, args)
				}
			}
			if mode == "tty" {
				if !slices.Contains(args, "-tt") || slices.Contains(args, "-T") || slices.Contains(args, "RequestTTY=no") {
					t.Errorf("terminal initialization must request a tty: %q", args)
				}
			} else if !slices.Contains(args, "-T") || !slices.Contains(args, "RequestTTY=no") || slices.Contains(args, "-tt") {
				t.Errorf("unattended initialization must not request a tty: %q", args)
			}
			if stdout.String() != "stdout-safe\n" || stderr.String() != "stderr-safe\n" {
				t.Errorf("unfiltered remote output: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunInteractivePreservesStdin(t *testing.T) {
	fakeSSH(t, `IFS= read -r reply
printf '%s\n' "$reply"
`)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := w.WriteString("interactive input\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	var stdout bytes.Buffer
	if err := (&SSH{Host: "me@b"}).RunInteractive("installer", r, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "interactive input\n" {
		t.Fatalf("stdin was consumed by the installer transport: %q", stdout.String())
	}
}

func TestRunInteractiveFailure(t *testing.T) {
	fakeSSH(t, "exit 7\n")
	err := (&SSH{Host: "me@b"}).RunInteractive("installer", nil, io.Discard, io.Discard)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("expected SSH exit status 7, got %v", err)
	}
}

func fakeSSH(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nset -eu\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("WALDO_TEST_SSH_ARGS", argsPath)
	return argsPath
}
