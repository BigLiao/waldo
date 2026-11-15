package audit

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestRemoteContextOnToolAndTimingEvents(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenRemote(dir, "session", "b2", "user@two")
	if err != nil {
		t.Fatal(err)
	}
	l.Log("exec", map[string]any{"argv": []string{"true"}, "cwd": "/project"})
	l.Log("exec-end", map[string]any{"duration_ms": 12})
	l.Log("write", map[string]any{"path": "/project/file"})
	l.Close()
	b, _ := os.ReadFile(Path(dir))
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil || ev["remote"] != "b2" || ev["remote_host"] != "user@two" {
			t.Fatalf("missing remote context: %s (%v)", line, err)
		}
		if !strings.Contains(Format(line), "remote=b2") {
			t.Fatalf("remote missing from formatted tool log: %s", Format(line))
		}
	}
}

func TestRemoteContextPreservesNetworkDestination(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenRemote(dir, "session", "b1", "user@server")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"net", "net-deny"} {
		l.Log(kind, map[string]any{"host": "api.example.com", "port": 443})
	}
	l.Close()
	b, _ := os.ReadFile(Path(dir))
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil || event["host"] != "api.example.com" || event["remote_host"] != "user@server" {
			t.Fatalf("network destination overwritten: %s (%v)", line, err)
		}
	}
}

func TestRemoteLogFilterMatchesMetadataAndTailsAfterFiltering(t *testing.T) {
	dir := t.TempDir()
	for _, remote := range []string{"b1", "b2", "b1", "b2"} {
		l, err := OpenRemote(dir, "session-"+remote, remote, remote)
		if err != nil {
			t.Fatal(err)
		}
		l.Log("exec-end", map[string]any{"duration_ms": 1})
		l.Close()
	}
	var out bytes.Buffer
	if err := PrintFiltered(&out, dir, 1, Filter{Remote: "b1"}); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "\n") != 1 || !strings.Contains(out.String(), "remote=b1") || strings.Contains(out.String(), "b2") {
		t.Fatalf("incorrect filtered tail: %s", out.String())
	}
	if (Filter{Remote: "b1", Session: "wrong"}).Matches(`{"remote":"b1","s":"session-b1"}`) {
		t.Fatal("session and remote filters were not combined")
	}
	if (Filter{Remote: "b1"}).Matches(`{"remote":"b2","argv":[{"remote":"b1"}]}`) {
		t.Fatal("nested command data spoofed a remote match")
	}
	if !(Filter{Remote: "b1"}).Matches(`{ "remote": "b1", "s": "old" }`) {
		t.Fatal("valid JSON with spaces rejected")
	}
}
