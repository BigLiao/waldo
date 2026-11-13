package host

import (
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"waldo/internal/modelstats"
	"waldo/internal/proto"
	"waldo/internal/util"
)

func TestModelObservationUsesExistingSessionAndScrubsCollectorEnv(t *testing.T) {
	a, b := net.Pipe()
	trusted, err := yamux.Client(a, util.YamuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer trusted.Close()
	remote, err := yamux.Server(b, util.YamuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	h := &host{sess: remote, scrub: map[string]bool{}, hello: proto.Hello{Observe: true, Profile: proto.Profile{Telemetry: "claude", Env: map[string]string{"OTEL_LOG_RAW_API_BODIES": "1", "BETA_TRACING_ENDPOINT": "https://unused.invalid"}}}}
	h.startModelObservation()
	defer h.stopModelObservation()
	if h.modelObservation == nil {
		t.Fatal("collector failed to start")
	}
	st, err := trusted.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.SetReadDeadline(time.Now().Add(5 * time.Second))
	var header proto.Header
	if err := proto.ReadMsg(st, &header); err != nil || header.Kind != proto.KindObserve {
		t.Fatalf("header=%+v err=%v", header, err)
	}
	endpoint, ok := util.EnvGet(h.agentEnv(), "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT")
	if !ok || !h.scrub["OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"] {
		t.Fatal("collector env missing or not scrubbed")
	}
	if _, ok := util.EnvGet(h.agentEnv(), "OTEL_LOG_RAW_API_BODIES"); ok {
		t.Fatal("raw content collection remains enabled")
	}
	if _, ok := util.EnvGet(h.agentEnv(), "BETA_TRACING_ENDPOINT"); ok {
		t.Fatal("external tracing destination remains enabled")
	}
	body := `{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"attributes":[{"key":"event.name","value":{"stringValue":"claude_code.api_request"}},{"key":"duration_ms","value":{"intValue":"125"}}]}]}]}]}`
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Post(endpoint, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("collector status=%d", response.StatusCode)
	}
	var stats modelstats.Stats
	if err := proto.ReadMsg(st, &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Requests != 1 || stats.RequestMS != 125 {
		t.Fatalf("bad counters: %+v", stats)
	}
	h.stopModelObservation()
	h.stopModelObservation() // cleanup remains idempotent
}

func TestModelObservationIsOptIn(t *testing.T) {
	h := &host{hello: proto.Hello{Profile: proto.Profile{Telemetry: "claude"}}}
	h.startModelObservation()
	if h.modelObservation != nil || len(h.observeEnv) != 0 {
		t.Fatal("default session changed telemetry")
	}
	h.stopModelObservation()
}

func TestModelObservationStopsBeforeAgentExit(t *testing.T) {
	remote, client := net.Pipe()
	defer remote.Close()
	defer client.Close()
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	o := &modelObservation{done: make(chan struct{}), stopped: make(chan struct{})}
	go func() { <-o.done; close(o.stopped) }()
	h := &host{modelObservation: o}
	result := make(chan int, 1)
	go func() { result <- h.runAgent(exec.Command("/bin/sh", "-c", "exit 0"), remote) }()
	for {
		kind, _, err := proto.ReadFrame(client)
		if err != nil {
			t.Fatal(err)
		}
		if kind != proto.FExit {
			continue
		}
		select {
		case <-o.stopped:
		default:
			t.Fatal("agent exit was delivered before final observation stopped")
		}
		break
	}
	if code := <-result; code != 0 {
		t.Fatalf("agent exit=%d", code)
	}
}
