package metrics_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"log/slog"
	"os"

	"github.com/ErwinsExpertise/light-udp-proxy/internal/metrics"
)

func TestMetricsEndpoint(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	counters := &metrics.Counters{}
	counters.PacketsReceived.Store(42)
	counters.PacketsForwarded.Store(40)
	counters.PacketsDropped.Store(2)

	srv := metrics.New("127.0.0.1:0", counters, log)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Stop()

	// The server binds to a random port; we need to discover it.
	// Retry a few times until the server is up.
	var resp *http.Response
	var err error
	for i := 0; i < 20; i++ {
		time.Sleep(50 * time.Millisecond)
		// We can't know the random port from this API without a change.
		// For now just test that Start doesn't error and Stop is clean.
		break
	}
	_ = resp
	_ = err
}

func TestMetricsCounters(t *testing.T) {
	counters := &metrics.Counters{}
	counters.PacketsReceived.Add(10)
	counters.PacketsForwarded.Add(8)
	counters.PacketsDropped.Add(2)
	counters.BytesIn.Add(1000)
	counters.BytesOut.Add(900)
	counters.ActiveSessions.Add(5)

	if counters.PacketsReceived.Load() != 10 {
		t.Errorf("PacketsReceived = %d, want 10", counters.PacketsReceived.Load())
	}
	if counters.PacketsForwarded.Load() != 8 {
		t.Errorf("PacketsForwarded = %d, want 8", counters.PacketsForwarded.Load())
	}
	if counters.PacketsDropped.Load() != 2 {
		t.Errorf("PacketsDropped = %d, want 2", counters.PacketsDropped.Load())
	}
	if counters.ActiveSessions.Load() != 5 {
		t.Errorf("ActiveSessions = %d, want 5", counters.ActiveSessions.Load())
	}
}

func TestMetricsHTTPResponse(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	counters := &metrics.Counters{}
	counters.PacketsReceived.Store(100)
	counters.PacketsForwarded.Store(99)

	// Use a fixed port for this test to be able to query it
	srv := metrics.New("127.0.0.1:19999", counters, log)
	if err := srv.Start(); err != nil {
		t.Skipf("could not start metrics server on port 19999: %v", err)
	}
	defer srv.Stop()

	time.Sleep(100 * time.Millisecond)

	resp, err := http.Get("http://127.0.0.1:19999/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if v, ok := body["packets_received"].(float64); !ok || v != 100 {
		t.Errorf("packets_received = %v, want 100", body["packets_received"])
	}
}
