package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRunningSessionsSkipsStaleAndInvalidWithoutMutating(t *testing.T) {
	t.Setenv("WALDO_DATA_DIR", t.TempDir())
	states, err := RunningSessions()
	if err != nil || len(states) != 0 {
		t.Fatalf("missing directory: %v %v", states, err)
	}
	if err := os.MkdirAll(RunDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, state := range map[string]RunState{
		"b.json":         {PID: os.Getpid(), Session: "b", Remote: "b2", Connection: "reconnecting"},
		"a.json":         {PID: os.Getpid(), Session: "a", Host: "legacy"},
		"stale.json":     {PID: 1 << 30, Session: "gone"},
		"group.json":     {PID: 0, Session: "invalid"},
		".state-partial": {PID: os.Getpid(), Session: "unpublished"},
	} {
		b, _ := json.Marshal(state)
		if err := os.WriteFile(filepath.Join(RunDir(), name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(RunDir(), "broken.json"), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	states, err = RunningSessions()
	if err != nil || len(states) != 2 || states[0].Session != "a" || states[1].Remote != "b2" || states[1].Connection != "reconnecting" {
		t.Fatalf("unexpected live sessions: %+v %v", states, err)
	}
	if _, err := os.Stat(filepath.Join(RunDir(), "stale.json")); err != nil {
		t.Fatal("read-only listing removed a stale record")
	}
}

func TestRunStateAtomicUpdates(t *testing.T) {
	t.Setenv("WALDO_DATA_DIR", t.TempDir())
	if err := os.MkdirAll(RunDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(RunDir(), "client.json")
	state := RunState{PID: os.Getpid(), Session: "live", Remote: "b1", Host: "one"}
	for _, connection := range []string{"starting", "connected", "reconnecting", "connected"} {
		state.Connection = connection
		writeRunState(path, state)
		states, err := RunningSessions()
		if err != nil || len(states) != 1 || states[0].Connection != connection {
			t.Fatalf("state update: %+v %v", states, err)
		}
	}
	entries, _ := os.ReadDir(RunDir())
	info, _ := os.Stat(path)
	if len(entries) != 1 || info.Mode().Perm() != 0o600 {
		t.Fatal("temporary state files leaked or permissions changed")
	}
}
