package session

import (
	"io"
	"math"

	"waldo/internal/audit"
	"waldo/internal/modelstats"
	"waldo/internal/proto"
)

// Only fixed numeric counters cross from B. No model input, response text,
// endpoint, credentials, tool arguments or resource attributes enter the log.
func serveModelStats(st io.ReadWriteCloser, al *audit.Logger, provider string) {
	defer st.Close()
	var previous, logged modelstats.Stats
	emit := func() {
		if previous == logged {
			return
		}
		al.Log("model-call", map[string]any{
			"provider": provider, "count": previous.Requests - logged.Requests,
			"errors":          previous.Errors - logged.Errors,
			"duration_ms":     previous.RequestMS - logged.RequestMS,
			"stream_event_ms": previous.StreamEventMS - logged.StreamEventMS,
		})
		logged = previous
	}
	defer emit()
	for {
		var now modelstats.Stats
		if err := proto.ReadMsg(st, &now); err != nil {
			return
		}
		if !validModelStats(previous, now) {
			return
		}
		previous = now
		// B already coalesces updates. Emit on receipt so a single call is
		// visible even when the agent then remains idle waiting for input.
		emit()
	}
}

func validModelStats(prev, now modelstats.Stats) bool {
	return now.Requests >= prev.Requests && now.Errors >= prev.Errors &&
		now.Requests <= 1e12 && now.Errors <= 1e12 &&
		now.RequestMS >= prev.RequestMS && now.StreamEventMS >= prev.StreamEventMS &&
		!math.IsNaN(now.RequestMS) && !math.IsNaN(now.StreamEventMS) &&
		now.RequestMS <= 1e15 && now.StreamEventMS <= 1e15
}
