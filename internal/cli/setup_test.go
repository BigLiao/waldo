package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"waldo/internal/config"
)

// These tests run setup across its shell/SSH boundaries with an isolated PATH.
// No real package manager, sudo, SSH connection, or user configuration is used.
func setupTestEnv(t *testing.T, bwrapInstalled bool) string {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	home := filepath.Join(root, "home")
	for _, d := range []string{bin, home} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"sh", "bash"} {
		if err := os.Symlink("/bin/"+name, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, script string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write("uname", "printf 'Linux\\n'\n")
	write("id", "printf '0\\n'\n")
	write("git", "exit 0\n")
	write("rg", "exit 0\n")
	if bwrapInstalled {
		write("bwrap", "exit 0\n")
	}
	write("apt-get", `
case "$1" in
  update)
    printf 'a-update\n' >> "$SETUP_TEST_LOG"
    [ "${SETUP_TEST_FAIL_A:-}" != 1 ] || exit 13
    ;;
  install)
    printf 'a-install\n' >> "$SETUP_TEST_LOG"
    printf '#!/bin/sh\nexit 0\n' > "$SETUP_TEST_BIN/bwrap"
    /bin/chmod 700 "$SETUP_TEST_BIN/bwrap"
    ;;
  *) exit 14 ;;
esac
`)
	// The apt branch uses env to pass DEBIAN_FRONTEND without affecting sudo.
	write("env", "shift\nexec \"$@\"\n")
	write("ssh", `
for arg do remote=$arg; done
case "$remote" in
  *waldo-install*)
    printf 'b-deps\n' >> "$SETUP_TEST_LOG"
    [ "${SETUP_TEST_FAIL_B:-}" != 1 ] || exit 7
    printf 'B dependencies ready\n'
    ;;
  *--adopt=false*)
    printf 'independent\n' >> "$SETUP_TEST_LOG"
    printf 'OK\nindependent\n'
    ;;
  *--adopt*)
    printf 'adopt\n' >> "$SETUP_TEST_LOG"
    printf 'OK\nadopted\n'
    ;;
  *--doctor*)
    printf 'doctor\n' >> "$SETUP_TEST_LOG"
    printf 'OK\n{"version":"test","os":"linux","arch":"amd64","checks":[],"time_unix_ms":0}\n'
    ;;
  *)
    printf 'version\n' >> "$SETUP_TEST_LOG"
    printf 'OK\ntest-version\n'
    ;;
esac
`)
	t.Setenv("PATH", bin)
	t.Setenv("HOME", home)
	t.Setenv("WALDO_CONFIG_DIR", filepath.Join(root, "config"))
	t.Setenv("WALDO_DATA_DIR", filepath.Join(root, "data"))
	t.Setenv("WALDO_SSH_ARGS", "")
	t.Setenv("SETUP_TEST_LOG", filepath.Join(root, "operations"))
	t.Setenv("SETUP_TEST_BIN", bin)
	t.Setenv("SETUP_TEST_FAIL_A", "")
	t.Setenv("SETUP_TEST_FAIL_B", "")
	return filepath.Join(root, "operations")
}

func TestSetupInstallsBeforeConnecting(t *testing.T) {
	log := setupTestEnv(t, false)
	cfg := &config.Config{}
	if code := setup(cfg, []string{"example"}); code != 0 {
		t.Fatalf("setup exit = %d", code)
	}
	operations, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(operations), "a-update\na-install\nb-deps\nversion\nadopt\ndoctor\n"; got != want {
		t.Fatalf("operations = %q, want %q", got, want)
	}
	if cfg.Host != "example" {
		t.Fatalf("host = %q", cfg.Host)
	}
	if _, err := os.Stat(config.Path()); err != nil {
		t.Fatalf("host configuration was not saved: %v", err)
	}
	// A second initialization checks dependencies without reinstalling packages.
	if err := os.WriteFile(log, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := setup(cfg, nil); code != 0 {
		t.Fatalf("repeat setup exit = %d", code)
	}
	operations, _ = os.ReadFile(log)
	if got, want := string(operations), "b-deps\nversion\nadopt\ndoctor\n"; got != want {
		t.Fatalf("repeat operations = %q, want %q", got, want)
	}
}

func TestSetupStopsOnInstallFailure(t *testing.T) {
	for _, side := range []string{"A", "B"} {
		t.Run(side, func(t *testing.T) {
			log := setupTestEnv(t, side == "B")
			t.Setenv("SETUP_TEST_FAIL_"+side, "1")
			if code := setup(&config.Config{}, []string{"example", "--adopt-login"}); code != 1 {
				t.Fatalf("setup exit = %d, want 1", code)
			}
			operations, _ := os.ReadFile(log)
			want := "a-update\n"
			if side == "B" {
				want = "b-deps\n"
			}
			if string(operations) != want {
				t.Fatalf("operations after failure = %q, want %q", operations, want)
			}
		})
	}
}

func TestSetupSkipDepsStillChecks(t *testing.T) {
	for _, args := range [][]string{{"example", "--skip-deps"}, {"--skip-deps", "example"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			log := setupTestEnv(t, false)
			// Missing bwrap still makes doctor fail, but never triggers installation.
			if code := setup(&config.Config{}, args); code != 1 {
				t.Fatalf("setup exit = %d, want 1", code)
			}
			operations, _ := os.ReadFile(log)
			if got, want := string(operations), "version\nadopt\ndoctor\n"; got != want {
				t.Fatalf("operations = %q, want %q", got, want)
			}
		})
	}
}

func TestSetupLoginReuseOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		mode string
	}{
		{"default", []string{"example", "--skip-deps"}, "adopt"},
		{"legacy-flag", []string{"example", "--adopt-login", "--skip-deps"}, "adopt"},
		{"explicit-true", []string{"--adopt-login=true", "example", "--skip-deps"}, "adopt"},
		{"opt-out-after-host", []string{"example", "--adopt-login=false", "--skip-deps"}, "independent"},
		{"opt-out-before-host", []string{"--adopt-login=false", "example", "--skip-deps"}, "independent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := setupTestEnv(t, true)
			if code := setup(&config.Config{}, tc.args); code != 0 {
				t.Fatalf("setup exit = %d", code)
			}
			operations, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := string(operations), "version\n"+tc.mode+"\ndoctor\n"; got != want {
				t.Fatalf("operations = %q, want %q", got, want)
			}
		})
	}
}

func TestInvalidSetupNeverInstalls(t *testing.T) {
	for _, args := range [][]string{nil, {"--help"}, {"example", "other"}, {"example", "--unknown"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			log := setupTestEnv(t, false)
			if code := setup(&config.Config{}, args); code != 2 {
				t.Fatalf("setup exit = %d, want 2", code)
			}
			if _, err := os.Stat(log); !os.IsNotExist(err) {
				t.Fatalf("invalid setup performed external operations: %v", err)
			}
		})
	}
}
