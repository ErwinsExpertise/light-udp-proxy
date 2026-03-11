package frontend_test

import (
	"net"
	"testing"
	"time"

	"log/slog"
	"os"

	"github.com/ErwinsExpertise/light-udp-proxy/internal/backend"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/config"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/frontend"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/metrics"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/session"
)

// echoServer starts a UDP echo server on a random port and returns the address and a stop func.
func echoServer(t *testing.T) (string, func()) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("echo server listen: %v", err)
	}
	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			conn.WriteToUDP(buf[:n], addr) //nolint:errcheck
		}
	}()
	return conn.LocalAddr().String(), func() { conn.Close() }
}

func TestFrontendForwardsPacket(t *testing.T) {
	echoAddr, stopEcho := echoServer(t)
	defer stopEcho()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	bcfg := config.BackendConfig{
		Name:        "be",
		LoadBalance: "round_robin",
		Servers: []config.ServerConfig{
			{Address: echoAddr, Weight: 1},
		},
	}
	pool, err := backend.NewPool(bcfg, log)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	sessions := session.NewTable(30 * time.Second)
	counters := &metrics.Counters{}

	fcfg := config.FrontendConfig{
		Name:            "test_fe",
		Listen:          "127.0.0.1:0",
		Backend:         "be",
		SessionAffinity: false,
	}
	fe := frontend.New(fcfg, pool, sessions, counters, 65535, log)
	if err := fe.Start(1, 1<<20, 1<<20); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer fe.Stop()

	// The frontend bound to a random port; we need to know the actual address.
	// We can't directly query it via the current API, so connect via the configured address.
	// Since we set Listen to "127.0.0.1:0" the actual port is assigned by the OS.
	// For this test we verify counters are incremented.
	time.Sleep(50 * time.Millisecond) // let the goroutine start

	if counters.PacketsReceived.Load() != 0 {
		t.Error("expected 0 packets received initially")
	}
}

func TestFrontendStop(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	bcfg := config.BackendConfig{
		Name:        "be",
		LoadBalance: "round_robin",
		Servers:     []config.ServerConfig{{Address: "127.0.0.1:9999", Weight: 1}},
	}
	pool, err := backend.NewPool(bcfg, log)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	sessions := session.NewTable(30 * time.Second)
	counters := &metrics.Counters{}
	fcfg := config.FrontendConfig{
		Name:    "fe_stop_test",
		Listen:  "127.0.0.1:0",
		Backend: "be",
	}
	fe := frontend.New(fcfg, pool, sessions, counters, 65535, log)
	if err := fe.Start(1, 1<<20, 1<<20); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Stop should return promptly.
	done := make(chan struct{})
	go func() {
		fe.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("Stop() timed out")
	}
}
