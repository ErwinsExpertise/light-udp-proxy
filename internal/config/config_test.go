package config

import (
	"testing"
	"time"
)

const validYAML = `
global:
  max_packet_size: 1500
  worker_threads: 2
  session_timeout: 30s
  log_level: debug
  metrics_addr: "127.0.0.1:9090"
  traffic_shaping:
    enabled: true
    packets_per_second: 200000
    bytes_per_second: 200MB
    burst_packets: 50000
  client_limits:
    packets_per_second: 500
    burst_packets: 100
  abuse_protection:
    enabled: true
    max_packets_per_second_per_ip: 20000
    max_sessions_per_ip: 200
  fragmentation:
    drop_fragments: true

frontends:
  - name: test_fe
    listen: "0.0.0.0:9000"
    backend: test_be
    session_affinity: true
    priority: high
    traffic_shaping:
      packets_per_second: 50000
      burst_packets: 10000

backends:
  - name: test_be
    load_balance: round_robin
    traffic_shaping:
      packets_per_second: 100000
    health_check:
      enabled: true
      interval: 5s
      timeout: 1s
    servers:
      - address: "127.0.0.1:9001"
        weight: 1
      - address: "127.0.0.1:9002"
        weight: 2
`

func TestParseValidConfig(t *testing.T) {
	cfg, err := Parse([]byte(validYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Global.MaxPacketSize != 1500 {
		t.Errorf("MaxPacketSize = %d, want 1500", cfg.Global.MaxPacketSize)
	}
	if cfg.Global.WorkerThreads != 2 {
		t.Errorf("WorkerThreads = %d, want 2", cfg.Global.WorkerThreads)
	}
	if cfg.Global.SessionTimeout != 30*time.Second {
		t.Errorf("SessionTimeout = %v, want 30s", cfg.Global.SessionTimeout)
	}
	if cfg.Global.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.Global.LogLevel)
	}
	if !cfg.Global.TrafficShaping.Enabled {
		t.Errorf("TrafficShaping.Enabled = false, want true")
	}
	wantBytesPerSecond := int64(200 * 1024 * 1024)
	if int64(cfg.Global.TrafficShaping.BytesPerSecond) != wantBytesPerSecond {
		t.Errorf("TrafficShaping.BytesPerSecond = %d, want %d", cfg.Global.TrafficShaping.BytesPerSecond, wantBytesPerSecond)
	}
	if cfg.Global.ClientLimits.PacketsPerSecond != 500 {
		t.Errorf("ClientLimits.PacketsPerSecond = %d, want 500", cfg.Global.ClientLimits.PacketsPerSecond)
	}
	if !cfg.Global.Fragmentation.DropFragments {
		t.Errorf("Fragmentation.DropFragments = false, want true")
	}
	if len(cfg.Frontends) != 1 {
		t.Fatalf("len(Frontends) = %d, want 1", len(cfg.Frontends))
	}
	fe := cfg.Frontends[0]
	if fe.Name != "test_fe" {
		t.Errorf("Frontend.Name = %q, want test_fe", fe.Name)
	}
	if fe.Listen != "0.0.0.0:9000" {
		t.Errorf("Frontend.Listen = %q, want 0.0.0.0:9000", fe.Listen)
	}
	if !fe.SessionAffinity {
		t.Errorf("Frontend.SessionAffinity = false, want true")
	}
	if fe.Priority != "high" {
		t.Errorf("Frontend.Priority = %q, want high", fe.Priority)
	}
	if fe.TrafficShaping.PacketsPerSecond != 50000 {
		t.Errorf("Frontend.TrafficShaping.PacketsPerSecond = %d, want 50000", fe.TrafficShaping.PacketsPerSecond)
	}
	if len(cfg.Backends) != 1 {
		t.Fatalf("len(Backends) = %d, want 1", len(cfg.Backends))
	}
	be := cfg.Backends[0]
	if be.Name != "test_be" {
		t.Errorf("Backend.Name = %q, want test_be", be.Name)
	}
	if be.LoadBalance != "round_robin" {
		t.Errorf("Backend.LoadBalance = %q, want round_robin", be.LoadBalance)
	}
	if be.TrafficShaping.PacketsPerSecond != 100000 {
		t.Errorf("Backend.TrafficShaping.PacketsPerSecond = %d, want 100000", be.TrafficShaping.PacketsPerSecond)
	}
	if len(be.Servers) != 2 {
		t.Fatalf("len(Servers) = %d, want 2", len(be.Servers))
	}
	if be.Servers[1].Weight != 2 {
		t.Errorf("Server[1].Weight = %d, want 2", be.Servers[1].Weight)
	}
}

func TestParseDefaults(t *testing.T) {
	minimal := `
frontends:
  - name: fe
    listen: "0.0.0.0:1234"
    backend: be
backends:
  - name: be
    servers:
      - address: "127.0.0.1:5000"
`
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Global.MaxPacketSize != 65535 {
		t.Errorf("default MaxPacketSize = %d, want 65535", cfg.Global.MaxPacketSize)
	}
	if cfg.Global.WorkerThreads != 4 {
		t.Errorf("default WorkerThreads = %d, want 4", cfg.Global.WorkerThreads)
	}
	if cfg.Global.SessionTimeout != 60*time.Second {
		t.Errorf("default SessionTimeout = %v, want 60s", cfg.Global.SessionTimeout)
	}
	if cfg.Global.LogLevel != "info" {
		t.Errorf("default LogLevel = %q, want info", cfg.Global.LogLevel)
	}
	if cfg.Backends[0].LoadBalance != "round_robin" {
		t.Errorf("default LoadBalance = %q, want round_robin", cfg.Backends[0].LoadBalance)
	}
	if cfg.Backends[0].Servers[0].Weight != 1 {
		t.Errorf("default Server.Weight = %d, want 1", cfg.Backends[0].Servers[0].Weight)
	}
	if cfg.Frontends[0].Priority != "normal" {
		t.Errorf("default Frontend.Priority = %q, want normal", cfg.Frontends[0].Priority)
	}
}

func TestParseInvalidYAML(t *testing.T) {
	_, err := Parse([]byte(":::invalid yaml"))
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
}

func TestValidateMissingFrontends(t *testing.T) {
	_, err := Parse([]byte(`
backends:
  - name: be
    servers:
      - address: "127.0.0.1:5000"
`))
	if err == nil {
		t.Fatal("expected error for missing frontends")
	}
}

func TestValidateMissingBackends(t *testing.T) {
	_, err := Parse([]byte(`
frontends:
  - name: fe
    listen: "0.0.0.0:1234"
    backend: be
`))
	if err == nil {
		t.Fatal("expected error for missing backends")
	}
}

func TestValidateUnknownBackendRef(t *testing.T) {
	_, err := Parse([]byte(`
frontends:
  - name: fe
    listen: "0.0.0.0:1234"
    backend: nonexistent
backends:
  - name: be
    servers:
      - address: "127.0.0.1:5000"
`))
	if err == nil {
		t.Fatal("expected error for unknown backend reference")
	}
}

func TestValidateInvalidLoadBalance(t *testing.T) {
	_, err := Parse([]byte(`
frontends:
  - name: fe
    listen: "0.0.0.0:1234"
    backend: be
backends:
  - name: be
    load_balance: bogus
    servers:
      - address: "127.0.0.1:5000"
`))
	if err == nil {
		t.Fatal("expected error for invalid load_balance")
	}
}

func TestValidateInvalidPriority(t *testing.T) {
	_, err := Parse([]byte(`
frontends:
  - name: fe
    listen: "0.0.0.0:1234"
    backend: be
    priority: urgent
backends:
  - name: be
    servers:
      - address: "127.0.0.1:5000"
`))
	if err == nil {
		t.Fatal("expected error for invalid priority")
	}
}
