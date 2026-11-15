package session

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"waldo/internal/config"
)

const testNativeID = "12345678-1234-1234-1234-123456789abc"

func routeFixture(t *testing.T, host string) *sessionRoute {
	t.Helper()
	return newSessionRoute(&config.Config{Host: host, SSHArgs: []string{"-i", "private-key-path"}}, host)
}

func TestSessionRoutePinsMachineAcrossRestarts(t *testing.T) {
	t.Setenv("WALDO_DATA_DIR", t.TempDir())
	t.Setenv("WALDO_SSH_ARGS", "")
	one := routeFixture(t, "one")
	if err := one.bind("paseo:agent-1"); err != nil {
		t.Fatal(err)
	}
	if err := routeFixture(t, "one").bind("paseo:agent-1"); err != nil {
		t.Fatalf("same machine after restart: %v", err)
	}
	if err := routeFixture(t, "two").bind("paseo:agent-1"); err == nil {
		t.Fatal("resumed on another machine")
	}
	if err := routeFixture(t, "two").bind("paseo:agent-2"); err != nil {
		t.Fatalf("independent agent: %v", err)
	}
	for _, cfg := range []*config.Config{
		{Host: "one", SSHArgs: []string{"-i", "other-key"}},
		{Host: "one", SSHArgs: []string{"-i", "private-key-path"}, RemoteDir: "~/other-account"},
	} {
		if err := newSessionRoute(cfg, "one").bind("paseo:agent-1"); err == nil {
			t.Fatal("changed SSH or data directory accepted")
		}
	}
	t.Setenv("WALDO_SSH_ARGS", "-p 2222")
	if err := routeFixture(t, "one").bind("paseo:agent-1"); err == nil {
		t.Fatal("changed environment SSH options accepted")
	}
	entries, _ := os.ReadDir(one.dir)
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(one.dir, entry.Name()))
		if err != nil || bytes.Contains(data, []byte("private-key-path")) || strings.Contains(entry.Name(), "agent-") {
			t.Fatal("binding leaked SSH arguments or raw identity")
		}
		info, _ := entry.Info()
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("binding permissions: %v", info.Mode())
		}
	}
}

func TestSessionRouteConcurrentClaimCannotOverwrite(t *testing.T) {
	t.Setenv("WALDO_DATA_DIR", t.TempDir())
	t.Setenv("WALDO_SSH_ARGS", "")
	one, two := routeFixture(t, "one"), routeFixture(t, "two")
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, r := range []*sessionRoute{one, two} {
		wg.Go(func() { <-start; results <- r.bind("paseo:contested") })
	}
	close(start)
	wg.Wait()
	first, second := <-results, <-results
	if (first == nil) == (second == nil) {
		t.Fatalf("expected one winner: %v %v", first, second)
	}
	if (one.bind("paseo:contested") == nil) == (two.bind("paseo:contested") == nil) {
		t.Fatal("binding was not immutable")
	}
}

func TestNativeResumeUsesOriginalRemote(t *testing.T) {
	t.Setenv("WALDO_DATA_DIR", t.TempDir())
	t.Setenv("WALDO_SSH_ARGS", "")
	one, two := routeFixture(t, "one"), routeFixture(t, "two")
	if err := one.checkArgs([]string{"--session-id", testNativeID}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--resume", testNativeID}, {"--resume=" + testNativeID},
		{"-r", testNativeID}, {"--session-id", strings.ToUpper(testNativeID)},
	} {
		if err := one.checkArgs(args); err != nil {
			t.Fatal(err)
		}
		if err := two.checkArgs(args); err == nil {
			t.Fatalf("cross-machine resume accepted: %v", args)
		}
	}
	for _, args := range [][]string{
		{"--", "--resume", testNativeID}, {"-p", "--resume=" + testNativeID + " prompt"},
		{"--continue"}, {"--resume", "named-session"},
	} {
		if err := two.checkArgs(args); err != nil {
			t.Fatalf("misinterpreted non-ID argument %v: %v", args, err)
		}
	}
}

func TestRouteObserverHandlesFragmentedOutputAndLongLines(t *testing.T) {
	t.Setenv("WALDO_DATA_DIR", t.TempDir())
	t.Setenv("WALDO_SSH_ARGS", "")
	one, two := routeFixture(t, "one"), routeFixture(t, "two")
	o := one.observer([]string{"--output-format=stream-json"})
	// Synthetic notices and assistant session IDs do not establish native state.
	for _, line := range []string{
		`{"type":"assistant","session_id":"` + testNativeID + `","message":{"text":"private"}}` + "\n",
		strings.Repeat("x", (1<<20)+1) + "\n",
	} {
		if err := o.observe([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(one.dir)
	if len(entries) != 0 {
		t.Fatal("non-native output established a route")
	}
	line := []byte(`{"type":"system","subtype":"init","session_id":"` + testNativeID + `"}` + "\n")
	for _, chunk := range [][]byte{line[:7], line[7:30], line[30:]} {
		before := bytes.Clone(chunk)
		if err := o.observe(chunk); err != nil || !bytes.Equal(chunk, before) {
			t.Fatalf("output changed or rejected: %v", err)
		}
	}
	if err := two.checkArgs([]string{"--resume", testNativeID}); err == nil {
		t.Fatal("native init did not bind original remote")
	}
	if err := o.observe(line); err != nil {
		t.Fatalf("repeated init: %v", err)
	}
	if one.observer(nil) != nil || one.observer([]string{"--", "--output-format=stream-json"}) != nil || (*sessionRoute)(nil).observer(nil) != nil {
		t.Fatal("observer enabled outside native stream-json")
	}
}

func TestCorruptSessionBindingFailsClosed(t *testing.T) {
	t.Setenv("WALDO_DATA_DIR", t.TempDir())
	t.Setenv("WALDO_SSH_ARGS", "")
	r := routeFixture(t, "one")
	if err := r.bind("paseo:one"); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(r.dir)
	if err := os.WriteFile(filepath.Join(r.dir, entries[0].Name()), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.bind("paseo:one"); err == nil {
		t.Fatal("corrupt binding was ignored")
	}
}

func TestRejectedPaseoLaunchDoesNotClaimNativeSession(t *testing.T) {
	t.Setenv("WALDO_DATA_DIR", t.TempDir())
	t.Setenv("WALDO_SSH_ARGS", "")
	one, two := routeFixture(t, "one"), routeFixture(t, "two")
	if err := one.prepare("paseo:existing", nil); err != nil {
		t.Fatal(err)
	}
	args := []string{"--session-id", testNativeID}
	if err := two.prepare("paseo:existing", args); err == nil {
		t.Fatal("changed remote was accepted")
	}
	if err := one.prepare("paseo:existing", args); err != nil {
		t.Fatalf("rejected launch poisoned native ID: %v", err)
	}
}

func TestNamingLegacyHostDoesNotBreakExistingSession(t *testing.T) {
	t.Setenv("WALDO_DATA_DIR", t.TempDir())
	t.Setenv("WALDO_SSH_ARGS", "")
	cfg := &config.Config{Host: "user@server"}
	if err := newSessionRoute(cfg, "").prepare("paseo:existing", nil); err != nil {
		t.Fatal(err)
	}
	if err := newSessionRoute(cfg, "b1").prepare("paseo:existing", nil); err != nil {
		t.Fatalf("label-only change broke same-machine resume: %v", err)
	}
}
