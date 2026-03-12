package metrics_test

import (
"encoding/json"
"fmt"
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
counters.PacketsShaped.Store(1)
counters.PacketsDroppedRateLimit.Store(3)
counters.PacketsDroppedFragment.Store(4)
counters.PacketsDroppedAbuse.Store(5)

// Bind to a random port so the test is safe to run in parallel.
srv := metrics.New("127.0.0.1:0", counters, log)
if err := srv.Start(); err != nil {
t.Fatalf("Start: %v", err)
}
defer srv.Stop()

addr := srv.Addr()
if addr == "" {
t.Fatal("Addr() returned empty string after Start")
}

// Allow the server a brief moment to start accepting connections.
time.Sleep(50 * time.Millisecond)

resp, err := http.Get(fmt.Sprintf("http://%s/metrics", addr))
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
if v, ok := body["packets_received"].(float64); !ok || int(v) != 42 {
t.Errorf("packets_received = %v, want 42", body["packets_received"])
}
if v, ok := body["packets_forwarded"].(float64); !ok || int(v) != 40 {
t.Errorf("packets_forwarded = %v, want 40", body["packets_forwarded"])
}
if v, ok := body["packets_shaped"].(float64); !ok || int(v) != 1 {
t.Errorf("packets_shaped = %v, want 1", body["packets_shaped"])
}
if v, ok := body["packets_dropped_rate_limit"].(float64); !ok || int(v) != 3 {
t.Errorf("packets_dropped_rate_limit = %v, want 3", body["packets_dropped_rate_limit"])
}

// /healthz must return 200 ok.
hResp, err := http.Get(fmt.Sprintf("http://%s/healthz", addr))
if err != nil {
t.Fatalf("GET /healthz: %v", err)
}
defer hResp.Body.Close()
if hResp.StatusCode != http.StatusOK {
t.Errorf("healthz status = %d, want 200", hResp.StatusCode)
}
}

func TestMetricsSessionCountFn(t *testing.T) {
log := slog.New(slog.NewTextHandler(os.Stderr, nil))
counters := &metrics.Counters{}
// counters.ActiveSessions is left at 0; sessionCountFn returns 99.
srv := metrics.New("127.0.0.1:0", counters, log)
srv.RegisterSessionCount(func() int64 { return 99 })
if err := srv.Start(); err != nil {
t.Fatalf("Start: %v", err)
}
defer srv.Stop()

time.Sleep(50 * time.Millisecond)

resp, err := http.Get(fmt.Sprintf("http://%s/metrics", srv.Addr()))
if err != nil {
t.Fatalf("GET /metrics: %v", err)
}
defer resp.Body.Close()

var body map[string]interface{}
if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
t.Fatalf("decode: %v", err)
}
if v, ok := body["active_sessions"].(float64); !ok || int(v) != 99 {
t.Errorf("active_sessions = %v, want 99", body["active_sessions"])
}
}

func TestMetricsCounters(t *testing.T) {
counters := &metrics.Counters{}
counters.PacketsReceived.Add(10)
counters.PacketsForwarded.Add(8)
counters.PacketsDropped.Add(2)
counters.PacketsShaped.Add(1)
counters.PacketsDroppedRateLimit.Add(1)
counters.PacketsDroppedFragment.Add(1)
counters.PacketsDroppedAbuse.Add(1)
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
if counters.PacketsDroppedRateLimit.Load() != 1 {
t.Errorf("PacketsDroppedRateLimit = %d, want 1", counters.PacketsDroppedRateLimit.Load())
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

srv := metrics.New("127.0.0.1:0", counters, log)
if err := srv.Start(); err != nil {
t.Fatalf("could not start metrics server: %v", err)
}
defer srv.Stop()

time.Sleep(100 * time.Millisecond)

resp, err := http.Get(fmt.Sprintf("http://%s/metrics", srv.Addr()))
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
