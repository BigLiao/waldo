package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestResolveRemoteSelectionAndIsolation(t *testing.T) {
	c := &Config{Host: "legacy", DefaultRemote: "b1", SSHArgs: []string{"-p", "2222"}, RemoteDir: "~/global",
		Remote: map[string]RemoteCfg{
			"b1": {Host: "user@one"},
			"b2": {Host: "user@two", SSHArgs: []string{"-i", "/key-two"}, RemoteDir: "~/two"},
			"b3": {Host: "three", SSHArgs: []string{}},
		}}
	for _, tc := range []struct {
		name, wantName, host, dir string
		args                      []string
	}{
		{"", "b1", "user@one", "~/global", []string{"-p", "2222"}},
		{"b2", "b2", "user@two", "~/two", []string{"-i", "/key-two"}},
		{"b3", "b3", "three", "~/global", nil},
	} {
		got, name, err := c.ResolveRemote(tc.name)
		if err != nil || name != tc.wantName || got.Host != tc.host || got.RemoteDir != tc.dir || !reflect.DeepEqual(got.SSHArgs, tc.args) {
			t.Fatalf("%q: got %+v %q %v", tc.name, got, name, err)
		}
		if len(got.SSHArgs) > 0 {
			got.SSHArgs[0] = "mutated"
		}
	}
	if c.Host != "legacy" || c.SSHArgs[0] != "-p" || c.Remote["b2"].SSHArgs[0] != "-i" {
		t.Fatal("selection mutated shared configuration")
	}
	c.DefaultRemote = ""
	got, name, err := c.ResolveRemote("")
	if err != nil || got.Host != "legacy" || name != "" {
		t.Fatalf("legacy host: %+v %q %v", got, name, err)
	}
}

func TestRemoteNeverFallsBack(t *testing.T) {
	c := &Config{Host: "legacy", DefaultRemote: "missing", Remote: map[string]RemoteCfg{"empty": {}}}
	for _, name := range []string{"", "unknown", "empty", "../bad", "--paseo"} {
		if _, _, err := c.ResolveRemote(name); err == nil {
			t.Fatalf("accepted invalid selection %q", name)
		}
	}
	if _, _, err := (&Config{}).ResolveRemote(""); err == nil {
		t.Fatal("accepted unconfigured host")
	}
}

func TestLoadRemoteTOML(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WALDO_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(`host = "legacy"
default_remote = "b1"
ssh_args = ["-p", "2222"]
[remote.b1]
host = "one"
[remote.b2]
host = "two"
ssh_args = []
remote_dir = "~/separate"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	b1, _, err := c.ResolveRemote("")
	if err != nil || b1.Host != "one" || len(b1.SSHArgs) != 2 {
		t.Fatalf("default inheritance: %+v %v", b1, err)
	}
	b2, _, err := c.ResolveRemote("b2")
	if err != nil || b2.Host != "two" || len(b2.SSHArgs) != 0 || b2.RemoteDir != "~/separate" {
		t.Fatalf("explicit overrides: %+v %v", b2, err)
	}
}

func TestDefaultSingleHostCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		cfg                  Config
		wantHost, wantRemote string
		wantError            bool
	}{
		{"legacy", Config{Host: "legacy"}, "legacy", "", false},
		{"legacy wins over sole named remote", Config{Host: "legacy", Remote: map[string]RemoteCfg{"b1": {Host: "one"}}}, "legacy", "", false},
		{"sole named remote", Config{Remote: map[string]RemoteCfg{"b1": {Host: "one"}}}, "one", "b1", false},
		{"multiple require selection", Config{Remote: map[string]RemoteCfg{"b1": {Host: "one"}, "b2": {Host: "two"}}}, "", "", true},
		{"explicit default", Config{Host: "legacy", DefaultRemote: "b2", Remote: map[string]RemoteCfg{"b1": {Host: "one"}, "b2": {Host: "two"}}}, "two", "b2", false},
		{"invalid default never falls back", Config{Host: "legacy", DefaultRemote: "missing", Remote: map[string]RemoteCfg{"b1": {Host: "one"}}}, "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, remote, err := tc.cfg.ResolveRemote("")
			if tc.wantError {
				if err == nil {
					t.Fatal("accepted ambiguous or invalid selection")
				}
				return
			}
			if err != nil || got.Host != tc.wantHost || remote != tc.wantRemote {
				t.Fatalf("selection: %+v %q %v", got, remote, err)
			}
		})
	}
}
