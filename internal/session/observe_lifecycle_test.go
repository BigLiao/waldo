package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"waldo/internal/audit"
	"waldo/internal/modelstats"
	"waldo/internal/proto"
	"waldo/internal/util"
)

func observedModelTotals(t *testing.T, dir string) modelstats.Stats {
	t.Helper()
	data, err := os.ReadFile(audit.Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	var totals modelstats.Stats
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		var ev struct {
			Kind          string  `json:"kind"`
			Count         uint64  `json:"count"`
			Errors        uint64  `json:"errors"`
			DurationMS    float64 `json:"duration_ms"`
			StreamEventMS float64 `json:"stream_event_ms"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Kind == "model-call" {
			totals.Requests += ev.Count
			totals.Errors += ev.Errors
			totals.RequestMS += ev.DurationMS
			totals.StreamEventMS += ev.StreamEventMS
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return totals
}

func waitObservedModelTotals(t *testing.T, dir string, want modelstats.Stats) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	for {
		if observedModelTotals(t, dir) == want {
			return
		}
		select {
		case <-ticker.C:
		case <-timeout.C:
			t.Fatalf("model totals = %+v, want %+v", observedModelTotals(t, dir), want)
		}
	}
}

func TestModelStatsFlushWhileStreamIsIdle(t *testing.T) {
	server, peer := net.Pipe()
	dir := t.TempDir()
	logger, err := audit.Open(dir, "idle-model")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { serveModelStats(server, logger, "claude"); close(done) }()
	t.Cleanup(func() {
		peer.Close()
		server.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("idle model observer did not stop")
		}
		logger.Close()
	})
	peer.SetWriteDeadline(time.Now().Add(time.Second))
	want := modelstats.Stats{Requests: 1, RequestMS: 125}
	if err := proto.WriteMsg(peer, want); err != nil {
		t.Fatal(err)
	}
	// There is no second snapshot and no EOF to trigger a deferred flush. A
	// completed call must still become visible while the agent waits for input.
	waitObservedModelTotals(t, dir, want)
	select {
	case <-done:
		t.Fatal("observer closed a healthy idle stream")
	default:
	}
}

func TestAcceptFromBDrainsModelStatsBeforeReturning(t *testing.T) {
	a, b := net.Pipe()
	trusted, err := yamux.Client(a, util.YamuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	remote, err := yamux.Server(b, util.YamuxConfig())
	if err != nil {
		trusted.Close()
		t.Fatal(err)
	}
	dir := t.TempDir()
	logger, err := audit.Open(dir, "drain-model")
	if err != nil {
		trusted.Close()
		remote.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		acceptFromB(trusted, &Sandbox{Observe: true, ModelTelemetry: "codex"}, logger)
		close(done)
	}()
	t.Cleanup(func() {
		remote.Close()
		trusted.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("accept loop did not drain its observers")
		}
		logger.Close()
	})
	stream, err := remote.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	stream.SetWriteDeadline(time.Now().Add(time.Second))
	if err := proto.WriteMsg(stream, proto.Header{Kind: proto.KindObserve}); err != nil {
		t.Fatal(err)
	}
	first := modelstats.Stats{Requests: 1, RequestMS: 10}
	if err := proto.WriteMsg(stream, first); err != nil {
		t.Fatal(err)
	}
	// This is an application-level acceptance barrier: the stream is being
	// served before session shutdown, rather than merely queued in yamux.
	waitObservedModelTotals(t, dir, first)
	want := modelstats.Stats{Requests: 2, Errors: 1, RequestMS: 25, StreamEventMS: 40}
	if err := proto.WriteMsg(stream, want); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	remote.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("accept loop did not return after remote session closed")
	}
	// The owner can now close the audit file without racing an asynchronous
	// final observation. Do not poll here: return itself is the drain barrier.
	if got := observedModelTotals(t, dir); got != want {
		t.Fatalf("model totals after accept loop returned = %+v, want %+v", got, want)
	}
}
