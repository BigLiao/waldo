package host

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"waldo/internal/proto"
)

func TestToolObservationMergesLaunchSettingsWithoutPersistentChanges(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(settings, []byte("persistent-config"), 0600); err != nil {
		t.Fatal(err)
	}
	h := &host{dataDir: dir, hello: proto.Hello{Observe: true, Session: "test-session", Profile: proto.Profile{ToolHooks: "claude", Args: []string{"--verbose"}}, Argv: []string{"--settings", `{"permissions":{"deny":["Bash(rm *)"]}}`}}}
	h.startToolObservation()
	defer h.stopToolObservation()
	if h.toolObservation == nil || h.hello.Profile.Args != nil || !strings.Contains(strings.Join(h.hello.Argv, " "), "Bash(rm *)") || h.hello.Argv[0] != "--verbose" {
		t.Fatal("hooks did not retain full launch configuration")
	}
	data, _ := os.ReadFile(settings)
	if string(data) != "persistent-config" {
		t.Fatal("modified persistent settings")
	}
	h.stopToolObservation()
	h.stopToolObservation()
}

func TestToolObservationOptInAndUnsupportedSettingsKeepLaunchUnchanged(t *testing.T) {
	for _, tc := range []struct {
		observe bool
		adapter string
		args    []string
	}{
		{false, "claude", []string{"-p", "prompt"}},
		{true, "none", []string{"-p", "prompt"}},
		{true, "claude", []string{"--settings", "/settings.json"}},
		{true, "claude", []string{"--settings", `{"disableAllHooks":true}`}},
	} {
		h := &host{dataDir: t.TempDir(), hello: proto.Hello{Observe: tc.observe, Profile: proto.Profile{ToolHooks: tc.adapter}, Argv: append([]string(nil), tc.args...)}}
		h.startToolObservation()
		h.stopToolObservation()
		if h.toolObservation != nil || !reflect.DeepEqual(h.hello.Argv, tc.args) {
			t.Fatal("unavailable observer changed agent launch")
		}
	}
}
