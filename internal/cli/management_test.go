package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"waldo/internal/config"
	"waldo/internal/proto"
	"waldo/internal/session"
)

func managementSessions(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(session.RunDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, state := range []session.RunState{
		{PID: os.Getpid(), Session: "one-session", Remote: "b1", Host: "one", Agent: "claude", Connection: "connected", Ports: []int{3000}},
		{PID: os.Getpid(), Session: "two-session", Remote: "b2", Host: "two", Agent: "claude", Connection: "reconnecting", Ports: []int{4000}},
		{PID: 1 << 30, Session: "stale-session", Remote: "b1", Host: "one"},
	} {
		b, _ := json.Marshal(state)
		if err := os.WriteFile(filepath.Join(session.RunDir(), state.Session+".json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRemoteListAndSessionViewsStayLocal(t *testing.T) {
	log := setupTestEnv(t, true)
	managementSessions(t)
	cfg := &config.Config{Host: "legacy", DefaultRemote: "b2", Remote: map[string]config.RemoteCfg{"b1": {Host: "one"}, "b2": {Host: "two"}}}
	var out bytes.Buffer
	if code := remoteCmd(cfg, []string{"ls", "--json"}, &out); code != 0 {
		t.Fatalf("ls exit %d", code)
	}
	var entries []remoteEntry
	if err := json.Unmarshal(out.Bytes(), &entries); err != nil || len(entries) != 3 || !entries[0].Legacy || entries[1].Sessions != 1 || !entries[2].Default || entries[2].Sessions != 1 {
		t.Fatalf("list: %+v %v", entries, err)
	}
	for _, ports := range []bool{false, true} {
		out.Reset()
		if code := sessionsCmd([]string{"--remote", "b2", "--json"}, &out, ports); code != 0 {
			t.Fatalf("sessions exit %d", code)
		}
		var states []session.RunState
		if err := json.Unmarshal(out.Bytes(), &states); err != nil || len(states) != 1 || states[0].Connection != "reconnecting" || states[0].Ports[0] != 4000 {
			t.Fatalf("session filter: %+v %v", states, err)
		}
		out.Reset()
		if code := sessionsCmd([]string{"--remote=b2"}, &out, ports); code != 0 || !strings.Contains(out.String(), "two-session") || strings.Contains(out.String(), "one-session") {
			t.Fatalf("session table: %d %s", code, out.String())
		}
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("local management command opened SSH")
	}
}

func statusFixture(t *testing.T) *config.Config {
	t.Helper()
	setupTestEnv(t, true)
	t.Setenv("STATUS_VERSION", proto.FullVersion())
	script := `#!/bin/sh
printf '%s\n' "$@" >> "$SETUP_TEST_SSH_LOG"
previous=''
for arg do
  [ "$previous" != -- ] || target=$arg
  previous=$arg
done
case "$target" in
  offline) echo 'Permission denied' >&2; exit 255 ;;
  absent) echo WALDO-STATUS-MISSING; exit 0 ;;
  old) echo 'WALDO-STATUS-VERSION older'; exit 0 ;;
  slow) exec /bin/sleep 5 ;;
esac
printf 'WALDO-STATUS-VERSION %s\n' "$STATUS_VERSION"
case "$target" in
  broken) echo 'not JSON' ;;
  notready) printf '{"version":"test","checks":[{"name":"/dev/fuse","ok":false}]}\n' ;;
  warning) printf '{"version":"test","checks":[{"name":"agent claude 已有状态","ok":false,"warn":true}]}\n' ;;
  *) printf '{"version":"test","checks":[]}\n' ;;
esac
`
	if err := os.WriteFile(filepath.Join(os.Getenv("SETUP_TEST_BIN"), "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return &config.Config{DefaultRemote: "b1", Remote: map[string]config.RemoteCfg{
		"b1": {Host: "one"}, "b2": {Host: "offline"},
		"absent": {Host: "absent"}, "old": {Host: "old"}, "broken": {Host: "broken"},
		"notready": {Host: "notready"}, "warning": {Host: "warning"}, "slow": {Host: "slow"},
	}}
}

func TestRemoteStatusResultsAndFailureIsolation(t *testing.T) {
	cfg := statusFixture(t)
	for _, tc := range []struct {
		name, state string
		code        int
	}{
		{"b1", "ready", 0}, {"b2", "unreachable", 1},
		{"absent", "not-installed", 1}, {"old", "version-mismatch", 1},
		{"broken", "invalid-report", 1}, {"notready", "not-ready", 1}, {"warning", "warning", 0},
	} {
		var out bytes.Buffer
		if code := remoteCmd(cfg, []string{"status", tc.name, "--json"}, &out); code != tc.code {
			t.Fatalf("%s exit %d: %s", tc.name, code, out.String())
		}
		var result []remoteStatus
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || len(result) != 1 || result[0].State != tc.state {
			t.Fatalf("%s: %+v %v", tc.name, result, err)
		}
	}
	// One unreachable/slow machine must not hide the results from healthy ones.
	var out bytes.Buffer
	if code := remoteCmd(cfg, []string{"status", "--all", "--timeout=100ms", "--json"}, &out); code != 1 {
		t.Fatalf("all exit %d", code)
	}
	var results []remoteStatus
	if err := json.Unmarshal(out.Bytes(), &results); err != nil || len(results) != len(cfg.Remote) {
		t.Fatalf("all: %+v %v", results, err)
	}
	for _, r := range results {
		if r.Name == "b1" && r.State != "ready" || r.Name == "slow" && r.State != "unreachable" {
			t.Fatalf("failure leaked between remotes: %+v", r)
		}
	}
}

func TestInvalidManagementArgsNeverConnect(t *testing.T) {
	log := setupTestEnv(t, true)
	cfg := &config.Config{Host: "legacy", Remote: map[string]config.RemoteCfg{"b1": {Host: "one"}}}
	for _, args := range [][]string{
		{"ls", "extra"}, {"status", "missing"}, {"status", "b1", "--all"},
		{"status", "--timeout=0s"}, {"status", "--timeout=2m"}, {"delete", "b1"},
	} {
		var out bytes.Buffer
		if code := remoteCmd(cfg, args, &out); code != 2 {
			t.Fatalf("%v exit %d", args, code)
		}
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("invalid management command opened SSH")
	}
}

func TestRemoteListReportsBrokenConfigurationWithoutFallback(t *testing.T) {
	entries := remoteEntries(&config.Config{Host: "legacy", DefaultRemote: "missing", Remote: map[string]config.RemoteCfg{"": {Host: "invalid-name"}}}, nil)
	if len(entries) != 3 || entries[1].Error == "" || entries[2].Error == "" || !entries[2].Default {
		t.Fatalf("broken configuration hidden by fallback: %+v", entries)
	}
}
