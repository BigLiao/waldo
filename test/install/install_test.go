package install_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the actual shell installer with local release fixtures. No test
// downloads from GitHub, writes the user's bin directory or invokes setup.
type fixture struct {
	root, bin, assets, tools, tmp, script string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	f := fixture{root: root, bin: filepath.Join(root, "installed bin"), assets: filepath.Join(root, "assets"), tools: filepath.Join(root, "tools"), tmp: filepath.Join(root, "tmp")}
	for _, dir := range []string{f.bin, f.assets, f.tools, f.tmp} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	f.script, err = filepath.Abs("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(f.tools, "uname"), `#!/bin/sh
case "$1" in
  -s) printf '%s\n' "${INSTALL_TEST_OS:-Linux}" ;;
  -m) printf '%s\n' "${INSTALL_TEST_ARCH:-x86_64}" ;;
  *) exit 1 ;;
esac
`, 0o755)
	write(t, filepath.Join(f.tools, "curl"), `#!/bin/sh
previous=''
for arg do
  [ "$previous" != --output ] || output=$arg
  previous=$arg
  url=$arg
done
printf '%s\n' "$url" >> "$INSTALL_TEST_ROOT/downloads"
case "$url" in
  https://github.com/BigLiao/waldo/releases/latest)
    printf '%s' "${INSTALL_TEST_RESOLVED:-https://github.com/BigLiao/waldo/releases/tag/v1.2.3}"
    exit 0
    ;;
  https://github.com/BigLiao/waldo/releases/download/v1.2.3/*|https://github.com/BigLiao/waldo/releases/download/v1.2.3-rc.1/*) asset=${url##*/} ;;
  *) exit 22 ;;
esac
[ "$asset" != "${INSTALL_TEST_FETCH_FAIL:-}" ] || exit 22
exec /bin/cp "$INSTALL_TEST_ROOT/assets/$asset" "$output"
`, 0o755)
	for _, arch := range []string{"amd64", "arm64"} {
		write(t, filepath.Join(f.assets, "waldo-linux-"+arch), fmt.Sprintf(`#!/bin/sh
# fixture architecture: %s
printf '%%s\n' "$1" >> "$INSTALL_TEST_ROOT/executed"
printf '%%s\n' "$0" >> "$INSTALL_TEST_ROOT/executed-path"
[ "$1" = version ] || exit 9
printf '%%s\n' '1.2.3+0123456789ab'
`, arch), 0o755)
	}
	f.checksums(t)
	return f
}

func write(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) checksums(t *testing.T) {
	t.Helper()
	var sums strings.Builder
	for _, asset := range []string{"waldo-linux-amd64", "waldo-linux-arm64"} {
		b, err := os.ReadFile(filepath.Join(f.assets, asset))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(b), asset)
	}
	write(t, filepath.Join(f.assets, "SHA256SUMS"), sums.String(), 0o600)
}

func (f fixture) run(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{f.script}, args...)...)
	cmd.Env = append(os.Environ(),
		"PATH="+f.tools+":"+os.Getenv("PATH"), "TMPDIR="+f.tmp,
		"WALDO_VERSION=latest", "WALDO_INSTALL_DIR="+f.bin,
		"INSTALL_TEST_ROOT="+f.root, "INSTALL_TEST_OS=Linux", "INSTALL_TEST_ARCH=x86_64", "INSTALL_TEST_FETCH_FAIL=",
		"INSTALL_TEST_RESOLVED=https://github.com/BigLiao/waldo/releases/tag/v1.2.3",
	)
	cmd.Env = append(cmd.Env, env...)
	b, err := cmd.CombinedOutput()
	entries, readErr := os.ReadDir(f.tmp)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("temporary downloads leaked: %v %v", entries, readErr)
	}
	stages, _ := filepath.Glob(filepath.Join(f.bin, ".waldo-install.*"))
	if len(stages) != 0 {
		t.Fatalf("temporary destination files leaked: %v", stages)
	}
	return string(b), err
}

func TestInstallLatestAndArchitectureAliases(t *testing.T) {
	for _, tc := range []struct{ uname, arch string }{{"x86_64", "amd64"}, {"amd64", "amd64"}, {"aarch64", "arm64"}, {"arm64", "arm64"}} {
		t.Run(tc.uname, func(t *testing.T) {
			f := newFixture(t)
			output, err := f.run(t, []string{"INSTALL_TEST_ARCH=" + tc.uname})
			if err != nil {
				t.Fatalf("install: %v\n%s", err, output)
			}
			for _, asset := range []string{"waldo-linux-amd64", "waldo-linux-arm64", "waldo"} {
				installed, err := os.ReadFile(filepath.Join(f.bin, asset))
				if err != nil {
					t.Fatal(err)
				}
				want := asset
				if asset == "waldo" {
					want = "waldo-linux-" + tc.arch
				}
				expected, _ := os.ReadFile(filepath.Join(f.assets, want))
				info, _ := os.Stat(filepath.Join(f.bin, asset))
				if string(installed) != string(expected) || info.Mode().Perm() != 0o755 {
					t.Fatalf("wrong binary or mode for %s", asset)
				}
			}
			fetches, _ := os.ReadFile(filepath.Join(f.root, "downloads"))
			if strings.Count(string(fetches), "/releases/latest") != 1 || strings.Contains(string(fetches), "/latest/download/") {
				t.Fatal("latest assets were not pinned to a single tag")
			}
			executed, _ := os.ReadFile(filepath.Join(f.root, "executed"))
			if string(executed) != "version\n" {
				t.Fatalf("installer invoked more than version: %q", executed)
			}
			executedPath, _ := os.ReadFile(filepath.Join(f.root, "executed-path"))
			if !strings.HasPrefix(string(executedPath), f.bin+"/") {
				t.Fatal("installer requires an executable TMPDIR")
			}
		})
	}
}

func TestExplicitVersionAndAtomicMainReplacement(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.bin, "waldo"), "old-running-inode", 0o755)
	if err := os.Link(filepath.Join(f.bin, "waldo"), filepath.Join(f.root, "old-inode")); err != nil {
		t.Fatal(err)
	}
	output, err := f.run(t, nil, "--version", "1.2.3", "--bin-dir", f.bin)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, output)
	}
	fetches, _ := os.ReadFile(filepath.Join(f.root, "downloads"))
	if strings.Contains(string(fetches), "/releases/latest") || strings.Contains(string(fetches), "api.github.com") {
		t.Fatal("explicit version unnecessarily resolved latest")
	}
	old, _ := os.ReadFile(filepath.Join(f.root, "old-inode"))
	if string(old) != "old-running-inode" {
		t.Fatal("installer truncated the old executable instead of renaming")
	}
}

func TestFailuresPreserveExistingInstallation(t *testing.T) {
	for _, mode := range []string{"corrupt-native", "corrupt-companion", "missing-checksum", "duplicate-checksum", "malformed-checksum", "wrong-version", "download-failure", "directory-target"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			for _, asset := range []string{"waldo", "waldo-linux-amd64", "waldo-linux-arm64"} {
				write(t, filepath.Join(f.bin, asset), "old-"+asset, 0o755)
			}
			var env []string
			sums, _ := os.ReadFile(filepath.Join(f.assets, "SHA256SUMS"))
			lines := strings.Split(strings.TrimSpace(string(sums)), "\n")
			switch mode {
			case "corrupt-native":
				write(t, filepath.Join(f.assets, "waldo-linux-amd64"), "corrupt", 0o755)
			case "corrupt-companion":
				write(t, filepath.Join(f.assets, "waldo-linux-arm64"), "corrupt", 0o755)
			case "missing-checksum":
				write(t, filepath.Join(f.assets, "SHA256SUMS"), lines[0]+"\n", 0o600)
			case "duplicate-checksum":
				write(t, filepath.Join(f.assets, "SHA256SUMS"), string(sums)+lines[0]+"\n", 0o600)
			case "malformed-checksum":
				write(t, filepath.Join(f.assets, "SHA256SUMS"), "invalid  waldo-linux-amd64\n"+lines[1]+"\n", 0o600)
			case "wrong-version":
				write(t, filepath.Join(f.assets, "waldo-linux-amd64"), "#!/bin/sh\nprintf '9.9.9+wrong\\n'\n", 0o755)
				f.checksums(t)
			case "download-failure":
				env = []string{"INSTALL_TEST_FETCH_FAIL=waldo-linux-arm64"}
			case "directory-target":
				if err := os.Remove(filepath.Join(f.bin, "waldo")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(f.bin, "waldo"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			output, err := f.run(t, env)
			if err == nil {
				t.Fatalf("invalid install succeeded: %s", output)
			}
			for _, asset := range []string{"waldo", "waldo-linux-amd64", "waldo-linux-arm64"} {
				if mode == "directory-target" && asset == "waldo" {
					continue
				}
				b, err := os.ReadFile(filepath.Join(f.bin, asset))
				if err != nil || string(b) != "old-"+asset {
					t.Fatalf("failure changed %s: %q %v", asset, b, err)
				}
			}
			if strings.HasPrefix(mode, "corrupt-") {
				if _, err := os.Stat(filepath.Join(f.root, "executed")); !os.IsNotExist(err) {
					t.Fatal("executed an unverified download")
				}
			}
		})
	}
}

func TestRejectUnsupportedPlatformsAndInvalidVersionsBeforeDownload(t *testing.T) {
	for _, tc := range []struct{ env, args []string }{
		{env: []string{"INSTALL_TEST_OS=Darwin"}}, {env: []string{"INSTALL_TEST_ARCH=riscv64"}},
		{args: []string{"--version=../../other"}}, {args: []string{"--version=v1.2.3;touch pwned"}},
		{args: []string{"--version="}}, {args: []string{"--bin-dir="}}, {args: []string{"--unknown"}},
		{args: []string{"--bin-dir", "--help"}}, {args: []string{"--version", "--bin-dir"}},
	} {
		f := newFixture(t)
		if output, err := f.run(t, tc.env, tc.args...); err == nil {
			t.Fatalf("accepted invalid request: %s", output)
		}
		if _, err := os.Stat(filepath.Join(f.root, "downloads")); !os.IsNotExist(err) {
			t.Fatal("invalid request downloaded files")
		}
	}
}

func TestFutureManifestEntriesCannotSelectOtherFiles(t *testing.T) {
	f := newFixture(t)
	sums, _ := os.ReadFile(filepath.Join(f.assets, "SHA256SUMS"))
	write(t, filepath.Join(f.assets, "SHA256SUMS"), string(sums)+strings.Repeat("0", 64)+"  install.sh\n"+strings.Repeat("0", 64)+"  ../outside\n", 0o600)
	if output, err := f.run(t, nil); err != nil {
		t.Fatalf("installer checked unrelated manifest entries: %v\n%s", err, output)
	}
}

func TestInstallerHelpDoesNotDownloadOrInstall(t *testing.T) {
	f := newFixture(t)
	if output, err := f.run(t, nil, "--help"); err != nil || !strings.Contains(output, "--version") {
		t.Fatalf("help: %v %s", err, output)
	}
	if _, err := os.Stat(filepath.Join(f.root, "downloads")); !os.IsNotExist(err) {
		t.Fatal("help downloaded files")
	}
}

func TestPinnedPrereleaseAndVersionEnvironment(t *testing.T) {
	for _, prerelease := range []bool{false, true} {
		f := newFixture(t)
		version := "1.2.3"
		if prerelease {
			version = "1.2.3-rc.1"
			for _, asset := range []string{"waldo-linux-amd64", "waldo-linux-arm64"} {
				b, _ := os.ReadFile(filepath.Join(f.assets, asset))
				write(t, filepath.Join(f.assets, asset), strings.ReplaceAll(string(b), "1.2.3+", version+"+"), 0o755)
			}
			f.checksums(t)
		}
		if output, err := f.run(t, []string{"WALDO_VERSION=" + version}); err != nil {
			t.Fatalf("pinned version: %v\n%s", err, output)
		}
		fetches, _ := os.ReadFile(filepath.Join(f.root, "downloads"))
		if strings.Contains(string(fetches), "/releases/latest") || !strings.Contains(string(fetches), "/download/v"+version+"/") {
			t.Fatal("version environment did not pin downloads")
		}
	}
}

func TestUnexpectedLatestRedirectDoesNotInstall(t *testing.T) {
	for _, resolved := range []string{"https://github.com/BigLiao/waldo/releases", "https://other.example/releases/tag/v1.2.3", "https://github.com/BigLiao/waldo/releases/tag/../../bad"} {
		f := newFixture(t)
		if output, err := f.run(t, []string{"INSTALL_TEST_RESOLVED=" + resolved}); err == nil {
			t.Fatalf("accepted unexpected latest redirect: %s", output)
		}
		fetches, _ := os.ReadFile(filepath.Join(f.root, "downloads"))
		if strings.Count(string(fetches), "\n") != 1 {
			t.Fatal("unexpected redirect resulted in asset downloads")
		}
	}
}
