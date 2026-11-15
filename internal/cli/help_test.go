package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"waldo/internal/config"
	"waldo/internal/session"
)

func TestHelpWorksWithoutValidConfigOrSSH(t *testing.T) {
	log := setupTestEnv(t, false)
	if err := os.MkdirAll(config.ConfigDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.Path(), []byte("[broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--help"}, {"help"}, {"help", "remote"}, {"help", "remote", "status"},
		{"remote", "--help"}, {"remote", "ls", "--help"}, {"remote", "status", "--help"},
		{"remote", "help", "status"}, {"setup", "--help"}, {"doctor", "--help"},
		{"sessions", "--help"}, {"ports", "--help"}, {"log", "--help"}, {"help", "claude"},
	} {
		if code := Main(args); code != 0 {
			t.Fatalf("%v: help exit %d", args, code)
		}
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("help attempted setup or SSH")
	}
	var out bytes.Buffer
	if code := showHelp([]string{"remote", "status"}, &out); code != 0 || !strings.Contains(out.String(), "--timeout") || !strings.Contains(out.String(), "--json") {
		t.Fatal("incomplete status help")
	}
}

func TestWrapperHelpDoesNotConsumeAgentArguments(t *testing.T) {
	for _, args := range [][]string{
		{"claude", "--help"}, {"claude", "-p", "--help"},
		{"run", "--", "bash", "--help"}, {"remote", "status", "help"},
	} {
		if _, ok := helpRequest(args); ok {
			t.Fatalf("consumed agent arguments: %v", args)
		}
	}
}

func TestDefaultSetupAndDoctorLeaveSingleHostConfigUnchanged(t *testing.T) {
	for _, tc := range []struct{ name, toml, host string }{
		{"legacy", "host = \"legacy\"\n", "legacy"},
		{"sole remote", "[remote.b1]\nhost = \"one\"\n", "one"},
		{"legacy priority", "host = \"legacy\"\n[remote.b1]\nhost = \"one\"\n", "legacy"},
		{"named default", "default_remote = \"b1\"\n[remote.b1]\nhost = \"one\"\n", "one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupTestEnv(t, true)
			if err := os.MkdirAll(config.ConfigDir(), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(config.Path(), []byte(tc.toml), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"setup", "--skip-deps"}, {"doctor"}} {
				if code := Main(args); code != 0 {
					t.Fatalf("%v exit %d", args, code)
				}
			}
			b, _ := os.ReadFile(os.Getenv("SETUP_TEST_SSH_LOG"))
			if !strings.Contains(string(b), "\n"+tc.host+"\n") {
				t.Fatal("default command connected to wrong host")
			}
			after, _ := os.ReadFile(config.Path())
			if string(after) != tc.toml {
				t.Fatal("default single-host configuration rewritten")
			}
			cfg, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			entries := remoteEntries(cfg, nil)
			defaults := 0
			for _, entry := range entries {
				if entry.Default {
					defaults++
					if entry.Host != tc.host {
						t.Fatal("wrong default marker")
					}
				}
			}
			if defaults != 1 {
				t.Fatalf("expected one default marker: %+v", entries)
			}
		})
	}
}

func TestLegacyPortsKeepsTextFormat(t *testing.T) {
	setupTestEnv(t, true)
	if err := os.MkdirAll(session.RunDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	state := session.RunState{PID: os.Getpid(), Session: "legacy-session", Agent: "claude", Project: "/project  with spaces", Host: "legacy", Ports: []int{3000, 5173}}
	b, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(session.RunDir(), "legacy.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := sessionsCmd(nil, &out, true); code != 0 {
		t.Fatalf("ports exit %d", code)
	}
	want := "legacy-session  claude   /project  with spaces\n    127.0.0.1:3000  127.0.0.1:5173\n"
	if out.String() != want {
		t.Fatalf("legacy output changed: got %q, want %q", out.String(), want)
	}
}
