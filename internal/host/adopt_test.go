package host

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostAdoptFlagMustBeExplicit(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		set  bool
		on   bool
	}{
		{name: "normal host session"},
		{name: "other flags", args: []string{"--data-dir", "/tmp/waldo-test"}},
		{name: "legacy enable", args: []string{"--adopt"}, set: true, on: true},
		{name: "explicit enable", args: []string{"--adopt=true"}, set: true, on: true},
		{name: "explicit disable", args: []string{"--adopt=false"}, set: true},
		{name: "last value wins", args: []string{"--adopt", "--adopt=false"}, set: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fs := flag.NewFlagSet("host", flag.ContinueOnError)
			on := fs.Bool("adopt", false, "")
			fs.String("data-dir", "", "")
			if err := fs.Parse(tt.args); err != nil {
				t.Fatal(err)
			}
			if got := flagWasSet(fs, "adopt"); got != tt.set || *on != tt.on {
				t.Fatalf("adopt explicit=%v enabled=%v; want explicit=%v enabled=%v", got, *on, tt.set, tt.on)
			}
		})
	}
}

func TestHostUnadoptClearsBindingsAndPreservesState(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "native-home")
	dataDir := filepath.Join(root, "waldo")
	t.Setenv("HOME", home)
	state := map[string]string{
		filepath.Join(home, ".claude", ".credentials.json"):            "native credentials placeholder",
		filepath.Join(home, ".claude.json"):                            "native preferences placeholder",
		filepath.Join(dataDir, "home", ".claude", ".credentials.json"): "private credentials placeholder",
		filepath.Join(dataDir, "home", ".claude.json"):                 "copied preferences placeholder",
	}
	for path, data := range state {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeAdopted(dataDir, []string{"~/.claude"}); err != nil {
		t.Fatal(err)
	}
	if len(adopted(dataDir)) != 1 {
		t.Fatal("test did not start with an active directory binding")
	}
	for i := 0; i < 2; i++ {
		// Exercise the real dispatch: explicit false must return without
		// reading SSH protocol input or paths from stdin, including on repeat.
		if code := Main([]string{"--data-dir", dataDir, "--adopt=false"}); code != 0 {
			t.Fatalf("disable attempt %d returned %d", i, code)
		}
		if list := adopted(dataDir); len(list) != 0 {
			t.Fatalf("directory bindings remain enabled: %v", list)
		}
		registry, err := os.ReadFile(adoptedPath(dataDir))
		if err != nil || strings.TrimSpace(string(registry)) != "[]" {
			t.Fatalf("binding registry = %q, %v; want []", registry, err)
		}
		for path, want := range state {
			got, err := os.ReadFile(path)
			if err != nil || string(got) != want {
				t.Fatalf("state changed at %s: %q, %v", path, got, err)
			}
		}
	}
	st, err := os.Stat(adoptedPath(dataDir))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("registry permissions = %v, %v; want 0600", st, err)
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".adopted-") {
			t.Fatalf("left a temporary registry file: %s", e.Name())
		}
	}
}

func TestHostUnadoptDoesNotFollowRegistrySymlink(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "waldo")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	credentialFile := filepath.Join(root, "credentials-placeholder")
	const original = "native credentials placeholder"
	if err := os.WriteFile(credentialFile, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(credentialFile, adoptedPath(dataDir)); err != nil {
		t.Fatal(err)
	}
	if code := Main([]string{"--data-dir", dataDir, "--adopt=false"}); code != 0 {
		t.Fatalf("disable returned %d", code)
	}
	got, err := os.ReadFile(credentialFile)
	if err != nil || string(got) != original {
		t.Fatalf("followed registry symlink and changed its target: %q, %v", got, err)
	}
	st, err := os.Lstat(adoptedPath(dataDir))
	if err != nil || !st.Mode().IsRegular() {
		t.Fatalf("registry was not replaced by a regular file: %v, %v", st, err)
	}
}

func TestHostUnadoptCreatesEmptyRegistry(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "new", "waldo")
	if code := Main([]string{"--adopt=false", "--data-dir", dataDir}); code != 0 {
		t.Fatalf("disable without previous adoption returned %d", code)
	}
	b, err := os.ReadFile(adoptedPath(dataDir))
	if err != nil || strings.TrimSpace(string(b)) != "[]" {
		t.Fatalf("new binding registry = %q, %v", b, err)
	}
}
