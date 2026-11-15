package hostbash

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestExecutionObservationMetadataOnly(t *testing.T) {
	got := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v map[string]any
		json.NewDecoder(r.Body).Decode(&v)
		got <- v
		w.WriteHeader(204)
	}))
	defer server.Close()
	o := newExecutionObserver(server.URL + "/hooks/test")
	if o == nil {
		t.Fatal("no observer")
	}
	o.record("Read", "agent", 5*time.Millisecond, false)
	o.close()
	select {
	case row := <-got:
		if row["location"] != "agent" || row["duration_ms"] != float64(5) || row["tool_name"] != "mcp__waldo__Read" || len(row) != 7 {
			t.Fatal(row)
		}
	case <-time.After(time.Second):
		t.Fatal("no timing record")
	}
	for _, endpoint := range []string{"https://example.com/hooks/x", "http://127.0.0.1:123/hooks/x?token=x", "http://example.com:123/hooks/x"} {
		if newExecutionObserver(endpoint) != nil {
			t.Fatal("accepted non-loopback observer")
		}
	}
}
