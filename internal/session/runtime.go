package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// RunningSessions reads A's live client records. It does not open SSH
// connections or delete stale state; a live client is not proof of B health.
func RunningSessions() ([]RunState, error) {
	states := []RunState{}
	entries, err := os.ReadDir(RunDir())
	if errors.Is(err, os.ErrNotExist) {
		return states, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(RunDir(), e.Name()))
		if err != nil {
			continue
		}
		var state RunState
		if json.Unmarshal(b, &state) != nil || state.PID <= 0 || state.Session == "" {
			continue
		}
		if err := syscall.Kill(state.PID, 0); err != nil && !errors.Is(err, syscall.EPERM) {
			continue
		}
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].Session < states[j].Session })
	return states, nil
}

// Atomic publication prevents management commands from seeing a partial
// state file when the connection or forwarded ports change.
func writeRunState(path string, state RunState) {
	b, err := json.Marshal(state)
	if err != nil {
		return
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	if closeErr := f.Close(); err == nil && closeErr == nil {
		os.Rename(f.Name(), path)
	}
}
