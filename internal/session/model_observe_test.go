package session

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"

	"waldo/internal/audit"
	"waldo/internal/modelstats"
	"waldo/internal/proto"
)

func TestModelStatsFlushOnlyNumericSummary(t *testing.T) {
	a, b := net.Pipe()
	dir := t.TempDir()
	logger, err := audit.Open(dir, "model-test")
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()
	done := make(chan struct{})
	go func() { serveModelStats(a, logger, "codex"); close(done) }()
	if err := proto.WriteMsg(b, map[string]any{"requests": 2, "errors": 1, "request_ms": 120, "stream_event_ms": 500, "prompt": "must-not-be-logged"}); err != nil {
		t.Fatal(err)
	}
	if err := proto.WriteMsg(b, map[string]any{"requests": 3, "errors": 1, "request_ms": 150, "stream_event_ms": 800}); err != nil {
		t.Fatal(err)
	}
	b.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("model observer did not stop")
	}
	data, err := os.ReadFile(audit.Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("must-not-be-logged")) {
		t.Fatal("model text leaked")
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte{'\n'})
	var ev map[string]any
	if err := json.Unmarshal(lines[len(lines)-1], &ev); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || ev["kind"] != "model-call" || ev["count"] != float64(1) || ev["duration_ms"] != float64(30) || ev["stream_event_ms"] != float64(300) {
		t.Fatalf("bad model summary: %s", data)
	}
}

func TestModelStatsRejectRegressionAndInvalidNumbers(t *testing.T) {
	prev := modelstats.Stats{Requests: 2, Errors: 1, RequestMS: 10, StreamEventMS: 30}
	for _, now := range []modelstats.Stats{
		{Requests: 1, Errors: 1, RequestMS: 10, StreamEventMS: 30},
		{Requests: 2, Errors: 0, RequestMS: 10, StreamEventMS: 30},
		{Requests: 2, Errors: 1, RequestMS: 9, StreamEventMS: 30},
		{Requests: 2, Errors: 1, RequestMS: 10, StreamEventMS: 29},
		{Requests: 2, Errors: 1, RequestMS: 1e16, StreamEventMS: 30},
	} {
		if validModelStats(prev, now) {
			t.Fatalf("accepted invalid snapshot %+v", now)
		}
	}
}
