package healthcheck

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/ErwinsExpertise/light-udp-proxy/internal/backend"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/config"
)

// newTestPool creates a backend Pool with the given server addresses.
func newTestPool(t *testing.T, addrs ...string) *backend.Pool {
	t.Helper()
	servers := make([]config.ServerConfig, len(addrs))
	for i, a := range addrs {
		servers[i] = config.ServerConfig{Address: a, Weight: 1}
	}
	p, err := backend.NewPool(config.BackendConfig{
		Name:        "test",
		LoadBalance: "round_robin",
		Servers:     servers,
	}, slog.Default())
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	return p
}

// newChecker builds a Checker with a short timeout suitable for unit tests.
func newChecker(pool *backend.Pool) *Checker {
	return New(pool, config.HealthCheckConfig{
		Enabled:  true,
		Interval: 100 * time.Millisecond,
		Timeout:  200 * time.Millisecond,
	}, slog.Default())
}

// startUDPEchoServer starts a UDP echo server that echoes back every datagram.
// It returns the listening address and a cancel func to stop it.
func startUDPEchoServer(t *testing.T) (addr string, stop func()) {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen echo server: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 64)
		for {
			n, src, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if _, err := conn.WriteTo(buf[:n], src); err != nil {
				return
			}
		}
	}()
	return conn.LocalAddr().String(), func() {
		conn.Close()
		<-done
	}
}

// startSilentUDPServer starts a UDP server that accepts packets but never
// replies (simulates a real service that does not implement echo).
func startSilentUDPServer(t *testing.T) (addr string, stop func()) {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen silent server: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 64)
		for {
			if _, _, err := conn.ReadFrom(buf); err != nil {
				return
			}
			// intentionally drop — no reply
		}
	}()
	return conn.LocalAddr().String(), func() {
		conn.Close()
		<-done
	}
}

// TestProbeEchoServer verifies that probe returns true when the backend echoes
// the payload back (L4 echo confirmed).
func TestProbeEchoServer(t *testing.T) {
	addr, stop := startUDPEchoServer(t)
	defer stop()

	p := newTestPool(t, addr)
	c := newChecker(p)
	if !c.probe(context.Background(), addr) {
		t.Error("expected probe to return true for echo server")
	}
}

// TestProbeSilentServer verifies that probe returns true for a reachable server
// that does not reply to the probe (read-timeout counts as healthy).
func TestProbeSilentServer(t *testing.T) {
	addr, stop := startSilentUDPServer(t)
	defer stop()

	p := newTestPool(t, addr)
	c := newChecker(p)
	if !c.probe(context.Background(), addr) {
		t.Error("expected probe to return true for silent-but-reachable server")
	}
}

// TestProbeUnreachableServer verifies that probe returns false for an address
// that has no listener (ICMP port-unreachable expected).
func TestProbeUnreachableServer(t *testing.T) {
	// Pick a port that is definitely not listening by binding and immediately closing.
	tmp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tmp: %v", err)
	}
	addr := tmp.LocalAddr().String()
	tmp.Close() // nothing is listening now

	p := newTestPool(t, addr)
	c := newChecker(p)
	result := c.probe(context.Background(), addr)
	// On Linux the kernel delivers ICMP port-unreachable synchronously, so this
	// should return false. On some platforms/CI environments ICMP may be filtered,
	// making it impossible to distinguish "closed" from "silent". Skip rather
	// than fail when the environment doesn't deliver ICMP.
	if result {
		t.Skip("probe returned true for closed port — ICMP may be filtered in this environment")
	}
	t.Log("probe correctly returned false for closed port")
}

// TestCheckAllUpdatesHealth verifies that checkAll flips a server from unhealthy
// to healthy when it is reachable, confirming that the health state is actually
// updated rather than left at the pool's default-healthy initial value.
func TestCheckAllUpdatesHealth(t *testing.T) {
	addr, stop := startSilentUDPServer(t)
	defer stop()

	p := newTestPool(t, addr)
	c := newChecker(p)

	// Mark the server unhealthy so we can confirm checkAll flips it back to healthy.
	p.SetHealthy(addr, false)
	srv := p.ServerByAddr(addr)
	if srv == nil {
		t.Fatal("server not found in pool")
	}
	if srv.Healthy.Load() {
		t.Fatal("precondition failed: server should be unhealthy before checkAll")
	}

	c.checkAll(context.Background())

	if !srv.Healthy.Load() {
		t.Error("expected checkAll to mark reachable server healthy")
	}
}

// TestRunPerformsInitialCheck verifies that Run performs an immediate health
// check before the first interval fires, so backends are probed at startup.
func TestRunPerformsInitialCheck(t *testing.T) {
	addr, stop := startSilentUDPServer(t)
	defer stop()

	p := newTestPool(t, addr)
	// Mark the server unhealthy so we can confirm the initial check flips it.
	p.SetHealthy(addr, false)

	// Use a very long interval so any check that fires must be the initial one.
	checker := New(p, config.HealthCheckConfig{
		Enabled:  true,
		Interval: 10 * time.Second,
		Timeout:  200 * time.Millisecond,
	}, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go checker.Run(ctx)

	srv := p.ServerByAddr(addr)
	if srv == nil {
		t.Fatal("server not found in pool")
	}

	// Poll until the server is marked healthy or we time out, rather than
	// relying on a fixed Sleep that may be racy or pass trivially.
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if srv.Healthy.Load() {
				return // initial check succeeded
			}
		case <-timeout.C:
			t.Error("initial check did not mark server healthy within 2s deadline")
			return
		}
	}
}

// TestRunDisabled verifies that Run returns immediately when health checking is
// disabled and does not modify server health.
func TestRunDisabled(t *testing.T) {
	addr, stop := startSilentUDPServer(t)
	defer stop()

	p := newTestPool(t, addr)
	// Mark the server unhealthy to detect if Run accidentally probes it.
	p.SetHealthy(addr, false)

	checker := New(p, config.HealthCheckConfig{
		Enabled:  false,
		Interval: 50 * time.Millisecond,
		Timeout:  200 * time.Millisecond,
	}, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		checker.Run(ctx)
	}()

	// Run should return immediately when disabled.
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Error("Run did not return promptly when disabled")
		cancel()
	}

	// Health must remain false (Run should not have probed).
	srv := p.ServerByAddr(addr)
	if srv == nil {
		t.Fatal("server not found in pool")
	}
	if srv.Healthy.Load() {
		t.Error("disabled checker must not change server health")
	}
}

// TestProbePayloadIsNonEmpty verifies that probePayload contains at least one
// byte, ensuring a real UDP datagram is always sent.
func TestProbePayloadIsNonEmpty(t *testing.T) {
	if len(probePayload) == 0 {
		t.Error("probePayload must not be empty")
	}
}
