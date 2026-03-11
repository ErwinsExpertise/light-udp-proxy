package backend

import (
	"log/slog"
	"os"
	"testing"

	"github.com/ErwinsExpertise/light-udp-proxy/internal/config"
)

func testPool(t *testing.T, algo string, servers []config.ServerConfig) *Pool {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg := config.BackendConfig{
		Name:        "test",
		LoadBalance: algo,
		Servers:     servers,
	}
	p, err := NewPool(cfg, log)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	return p
}

func twoServers() []config.ServerConfig {
	return []config.ServerConfig{
		{Address: "127.0.0.1:5001", Weight: 1},
		{Address: "127.0.0.1:5002", Weight: 1},
	}
}

func TestRoundRobin(t *testing.T) {
	p := testPool(t, "round_robin", twoServers())
	counts := map[string]int{}
	for i := 0; i < 10; i++ {
		s, err := p.Pick("1.2.3.4")
		if err != nil {
			t.Fatalf("Pick: %v", err)
		}
		counts[s.Address]++
	}
	if counts["127.0.0.1:5001"] != 5 || counts["127.0.0.1:5002"] != 5 {
		t.Errorf("round_robin distribution uneven: %v", counts)
	}
}

func TestLeastConn(t *testing.T) {
	p := testPool(t, "least_conn", twoServers())
	// Artificially raise connections on server 1.
	p.servers[0].IncrConns()
	p.servers[0].IncrConns()

	s, err := p.Pick("1.2.3.4")
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if s.Address != "127.0.0.1:5002" {
		t.Errorf("least_conn picked %q, want 127.0.0.1:5002", s.Address)
	}
}

func TestRandom(t *testing.T) {
	p := testPool(t, "random", twoServers())
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		s, err := p.Pick("1.2.3.4")
		if err != nil {
			t.Fatalf("Pick: %v", err)
		}
		seen[s.Address] = true
	}
	if !seen["127.0.0.1:5001"] || !seen["127.0.0.1:5002"] {
		t.Errorf("random did not pick both servers over 100 trials: %v", seen)
	}
}

func TestHash(t *testing.T) {
	p := testPool(t, "hash", twoServers())
	// Same client IP must always go to same server.
	first, err := p.Pick("192.168.1.100")
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	for i := 0; i < 10; i++ {
		s, _ := p.Pick("192.168.1.100")
		if s.Address != first.Address {
			t.Errorf("hash not stable: got %q, want %q", s.Address, first.Address)
		}
	}
}

func TestWeightedRoundRobin(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg := config.BackendConfig{
		Name:        "weighted",
		LoadBalance: "round_robin",
		Servers: []config.ServerConfig{
			{Address: "127.0.0.1:5001", Weight: 1},
			{Address: "127.0.0.1:5002", Weight: 3},
		},
	}
	p, err := NewPool(cfg, log)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	counts := map[string]int{}
	for i := 0; i < 40; i++ {
		s, _ := p.Pick("1.2.3.4")
		counts[s.Address]++
	}
	if counts["127.0.0.1:5001"] != 10 || counts["127.0.0.1:5002"] != 30 {
		t.Errorf("weighted distribution wrong: %v", counts)
	}
}

func TestSetHealthy(t *testing.T) {
	p := testPool(t, "round_robin", twoServers())
	p.SetHealthy("127.0.0.1:5001", false)
	if p.servers[0].Healthy.Load() {
		t.Error("server 0 should be unhealthy")
	}

	// With server 0 unhealthy, all picks should return server 1.
	for i := 0; i < 5; i++ {
		s, err := p.Pick("1.2.3.4")
		if err != nil {
			t.Fatalf("Pick: %v", err)
		}
		if s.Address != "127.0.0.1:5002" {
			t.Errorf("expected 127.0.0.1:5002, got %q", s.Address)
		}
	}

	p.SetHealthy("127.0.0.1:5001", true)
	if !p.servers[0].Healthy.Load() {
		t.Error("server 0 should be healthy again")
	}
}

func TestNoHealthyServers(t *testing.T) {
	p := testPool(t, "round_robin", twoServers())
	p.SetHealthy("127.0.0.1:5001", false)
	p.SetHealthy("127.0.0.1:5002", false)

	_, err := p.Pick("1.2.3.4")
	if err == nil {
		t.Fatal("expected error when no healthy servers")
	}
}
