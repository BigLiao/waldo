package toolhooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func readRecords(t *testing.T, data []byte) []record {
	t.Helper()
	var rows []record
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var r record
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, r)
	}
	return rows
}

func TestHTTPHooksLogOnlyTimingAndStopWithSession(t *testing.T) {
	dir := t.TempDir()
	c, err := Start(dir, "waldo-session")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: time.Second}
	defer client.CloseIdleConnections()
	post := func(body string) {
		t.Helper()
		resp, err := client.Post(c.URL, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 204 || len(out) != 0 {
			t.Fatalf("hook changed agent behavior: status=%d body=%q", resp.StatusCode, out)
		}
	}
	post(`{"session_id":"agent-a","tool_use_id":"call-1","tool_name":"Bash","hook_event_name":"PreToolUse","tool_input":{"command":"private-command"},"transcript_path":"private-path"}`)
	post(`{"session_id":"agent-a","tool_use_id":"call-1","tool_name":"Bash","hook_event_name":"PostToolUse","duration_ms":12.5,"tool_response":{"stdout":"private-output"}}`)
	post(`{"session_id":"agent-a","tool_use_id":"call-2","tool_name":"Read","hook_event_name":"PostToolUseFailure","duration_ms":0,"is_interrupt":true,"error":"private-error"}`)
	c.Close()
	c.Close()
	data, err := os.ReadFile(filepath.Join(dir, "tool-timing.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-") || strings.Contains(string(data), "/hooks/") {
		t.Fatal("content or endpoint leaked to timing log")
	}
	rows := readRecords(t, data)
	if len(rows) != 3 || rows[0].Kind != "tool-start" || rows[1].Kind != "tool-end" || rows[1].Session != "waldo-session" || rows[1].Agent != "agent-a" {
		t.Fatalf("unexpected rows: %+v", rows)
	}
	if rows[1].Duration == nil || *rows[1].Duration != 12.5 || rows[1].HookElapsed == nil || rows[1].Matched == nil || !*rows[1].Matched {
		t.Fatalf("lost timing: %+v", rows[1])
	}
	if rows[2].Status != "interrupted" || rows[2].Duration == nil || *rows[2].Duration != 0 || rows[2].HookElapsed != nil || *rows[2].Matched {
		t.Fatalf("invented interval for unmatched completion: %+v", rows[2])
	}
	if resp, err := client.Post(c.URL, "application/json", strings.NewReader(`{}`)); err == nil {
		resp.Body.Close()
		t.Fatal("hook listener survived session")
	}
}

type bufferCloser struct{ bytes.Buffer }

func (b *bufferCloser) Close() error { return nil }

func TestPairingUsesAgentSessionAndReportsMissingPost(t *testing.T) {
	var out bufferCloser
	c := newCollector(&out, "waldo-session")
	base := time.Now()
	c.queue <- input{Event: "PreToolUse", Session: "a", Call: "same-id", Tool: "Read", at: base}
	c.queue <- input{Event: "PreToolUse", Session: "b", Call: "same-id", Tool: "Read", at: base.Add(time.Second)}
	c.queue <- input{Event: "PostToolUse", Session: "a", Call: "same-id", Tool: "Read", at: base.Add(2 * time.Second)}
	c.Close()
	rows := readRecords(t, out.Bytes())
	if len(rows) != 4 || rows[2].HookElapsed == nil || *rows[2].HookElapsed != 2000 || rows[2].Duration != nil {
		t.Fatalf("bad pairing or invented native duration: %+v", rows)
	}
	if rows[3].Kind != "tool-incomplete" || rows[3].Agent != "b" || rows[3].Duration != nil || rows[3].HookElapsed != nil {
		t.Fatalf("incomplete tool reported as completed: %+v", rows[3])
	}
}

func TestMalformedAndOversizedHooksStayNonblocking(t *testing.T) {
	var out bufferCloser
	c := newCollector(&out, "waldo-session")
	c.path = "/hooks/secret"
	for _, body := range []string{
		`not-json`, `{}`, `{} {}`,
		`{"session_id":"a","tool_use_id":"x","tool_name":"Bash","hook_event_name":"PostToolUse","duration_ms":-1}`,
		`{"padding":"` + strings.Repeat("x", maxBody) + `"}`,
	} {
		r := httptest.NewRequest("POST", c.path, strings.NewReader(body))
		w := httptest.NewRecorder()
		c.ServeHTTP(w, r)
		if w.Code != 204 || w.Body.Len() != 0 {
			t.Fatal("malformed observation changed hook decision")
		}
	}
	for _, path := range []string{"/hooks/wrong", c.path + "?unexpected=1"} {
		w := httptest.NewRecorder()
		c.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(`{}`)))
		if w.Code != 404 {
			t.Fatal("accepted another endpoint")
		}
	}
	c.Close()
	rows := readRecords(t, out.Bytes())
	if len(rows) != 1 || rows[0].Kind != "tool-stats-dropped" || rows[0].Dropped != 5 {
		t.Fatalf("lost drop accounting: %+v", rows)
	}
}

type blockedWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}
func (*blockedWriter) Close() error { return nil }

func TestSlowDiskCannotBlockHooksOrShutdown(t *testing.T) {
	w := &blockedWriter{entered: make(chan struct{}), release: make(chan struct{})}
	c := newCollector(w, "waldo-session")
	c.path = "/hooks/secret"
	defer close(w.release)
	defer c.Close()
	c.queue <- input{Event: "PreToolUse", Session: "a", Call: "first", Tool: "Bash", at: time.Now()}
	<-w.entered
	for i := range queueSize + 1 {
		body := fmt.Sprintf(`{"session_id":"a","tool_use_id":"call-%d","tool_name":"Bash","hook_event_name":"PreToolUse"}`, i)
		r := httptest.NewRequest("POST", c.path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		c.ServeHTTP(rec, r)
		if rec.Code != 204 {
			t.Fatal("overload blocked hook")
		}
	}
	if c.dropped.Load() == 0 {
		t.Fatal("queue overflow not recorded")
	}
	closed := make(chan struct{})
	go func() { c.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("slow log disk blocked shutdown")
	}
}
