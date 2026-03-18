package frontend_test

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ErwinsExpertise/light-udp-proxy/internal/backend"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/config"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/frontend"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/metrics"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/session"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/shaping"
)

func newTestFrontend(t *testing.T, listen, backendAddr string) (*frontend.Frontend, *metrics.Counters) {
	return newTestFrontendWithOptions(t, listen, backendAddr, frontend.RuntimeOptions{})
}

func newTestFrontendWithOptions(t *testing.T, listen, backendAddr string, opts frontend.RuntimeOptions) (*frontend.Frontend, *metrics.Counters) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	bcfg := config.BackendConfig{
		Name:        "be",
		LoadBalance: "round_robin",
		Servers:     []config.ServerConfig{{Address: backendAddr, Weight: 1}},
	}
	pool, err := backend.NewPool(bcfg, log)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	sessions := session.NewTable(30*time.Second, 10*time.Second)
	t.Cleanup(sessions.Stop)
	counters := &metrics.Counters{}
	fcfg := config.FrontendConfig{
		Name:    t.Name(),
		Listen:  listen,
		Backend: "be",
	}
	fe := frontend.New(fcfg, pool, sessions, counters, 65535, opts, log)
	return fe, counters
}

func TestFrontendStartStop(t *testing.T) {
	fe, _ := newTestFrontend(t, "127.0.0.1:0", "127.0.0.1:9999")
	if err := fe.Start(1, config.SocketConfig{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	done := make(chan struct{})
	go func() { fe.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("Stop() timed out")
	}
}

func TestFrontendForwardsPacket(t *testing.T) {
	// Start a UDP sink that echoes packets back to the sender.
	echoConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	defer echoConn.Close()
	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, err := echoConn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			echoConn.WriteToUDP(buf[:n], addr) //nolint:errcheck
		}
	}()

	fe, counters := newTestFrontend(t, "127.0.0.1:0", echoConn.LocalAddr().String())
	if err := fe.Start(1, config.SocketConfig{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer fe.Stop()

	// Give goroutines a moment to be scheduled.
	time.Sleep(50 * time.Millisecond)

	// Dial the frontend and send a packet.
	clientConn, err := net.Dial("udp", fe.Addr())
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer clientConn.Close()

	payload := []byte("hello-udp-proxy")
	if _, err := clientConn.Write(payload); err != nil {
		t.Fatalf("client write: %v", err)
	}

	// The backend echoes back to proxy; proxy must forward response to client.
	if err := clientConn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	got := make([]byte, len(payload))
	n, err := clientConn.Read(got)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	if string(got[:n]) != string(payload) {
		t.Fatalf("unexpected response: got %q want %q", string(got[:n]), string(payload))
	}

	// Poll for the counter to be updated (the worker is asynchronous).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if counters.PacketsReceived.Load() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if counters.PacketsReceived.Load() == 0 {
		t.Error("PacketsReceived is 0 after sending a packet")
	}
	if counters.PacketsForwarded.Load() == 0 {
		t.Error("PacketsForwarded is 0 after sending a packet")
	}
}

func TestFrontendReusePortEphemeralPortRejected(t *testing.T) {
	fe, _ := newTestFrontend(t, "127.0.0.1:0", "127.0.0.1:9999")
	err := fe.Start(2, config.SocketConfig{ReusePort: true})
	if err == nil {
		fe.Stop()
		t.Fatal("expected error when using reuse_port with ephemeral port 0")
	}
}

func TestFrontendReusePortFixedPort(t *testing.T) {
	// Allocate a free port then release it so the frontend can bind it.
	l, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Skip("cannot allocate test port")
	}
	port := l.LocalAddr().(*net.UDPAddr).Port
	l.Close()

	fe, _ := newTestFrontend(t, fmt.Sprintf("127.0.0.1:%d", port), "127.0.0.1:9999")
	err = fe.Start(2, config.SocketConfig{ReusePort: true})
	if err != nil {
		// On some CI environments SO_REUSEPORT may be restricted; skip rather than fail.
		t.Skipf("SO_REUSEPORT not available: %v", err)
	}
	fe.Stop()
}

func TestFrontendDropsShapedPackets(t *testing.T) {
	fe, counters := newTestFrontendWithOptions(t, "127.0.0.1:0", "127.0.0.1:9999", frontend.RuntimeOptions{
		FrontendShaper: shaping.NewBucket(shaping.Limits{
			Enabled:          true,
			PacketsPerSecond: 1,
			BurstPackets:     1,
		}),
	})
	if err := fe.Start(1, config.SocketConfig{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer fe.Stop()

	clientConn, err := net.Dial("udp", fe.Addr())
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer clientConn.Close()
	for i := 0; i < 8; i++ {
		if _, err := clientConn.Write([]byte("shape-test")); err != nil {
			t.Fatalf("client write: %v", err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if counters.PacketsShaped.Load() == 0 {
		t.Fatal("expected shaped packets to be dropped")
	}
	if counters.PacketsDroppedRateLimit.Load() == 0 {
		t.Fatal("expected rate-limit drop counter increment")
	}
}

func TestFrontendFragmentHandlingNoPayloadFalsePositive(t *testing.T) {
	fe, counters := newTestFrontendWithOptions(t, "127.0.0.1:0", "127.0.0.1:9999", frontend.RuntimeOptions{
		DropFragments: true,
	})
	if err := fe.Start(1, config.SocketConfig{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer fe.Stop()

	clientConn, err := net.Dial("udp", fe.Addr())
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer clientConn.Close()
	payload := []byte{
		0x45, 0x00, 0x00, 0x14, 0x00, 0x01, 0x20, 0x00,
		0x40, 0x11, 0x00, 0x00, 0x7f, 0x00, 0x00, 0x01,
		0x7f, 0x00, 0x00, 0x01,
	}
	if _, err := clientConn.Write(payload); err != nil {
		t.Fatalf("client write: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if counters.PacketsDroppedFragment.Load() != 0 {
		t.Fatal("expected no fragment drops for normal UDP payload data")
	}
}

func TestFrontendRoutesResponsesToMatchingClientRequests(t *testing.T) {
	backendConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("backend listen: %v", err)
	}
	defer backendConn.Close()

	type backendRequest struct {
		payload []byte
		addr    *net.UDPAddr
	}
	reqCh := make(chan backendRequest, 2)
	var backendWG sync.WaitGroup
	backendWG.Add(1)
	go func() {
		defer backendWG.Done()
		buf := make([]byte, 65535)
		for i := 0; i < 2; i++ {
			n, addr, readErr := backendConn.ReadFromUDP(buf)
			if readErr != nil {
				return
			}
			reqCh <- backendRequest{
				payload: append([]byte(nil), buf[:n]...),
				addr:    addr,
			}
		}
	}()

	fe, _ := newTestFrontend(t, "127.0.0.1:0", backendConn.LocalAddr().String())
	if err := fe.Start(1, config.SocketConfig{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer fe.Stop()

	client1, err := net.Dial("udp", fe.Addr())
	if err != nil {
		t.Fatalf("client1 dial: %v", err)
	}
	defer client1.Close()
	client2, err := net.Dial("udp", fe.Addr())
	if err != nil {
		t.Fatalf("client2 dial: %v", err)
	}
	defer client2.Close()

	if _, err := client1.Write([]byte("client-one")); err != nil {
		t.Fatalf("client1 write: %v", err)
	}
	if _, err := client2.Write([]byte("client-two")); err != nil {
		t.Fatalf("client2 write: %v", err)
	}

	var first, second backendRequest
	select {
	case first = <-reqCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first backend request")
	}
	select {
	case second = <-reqCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for second backend request")
	}
	backendWG.Wait()

	if _, err := backendConn.WriteToUDP([]byte("reply-"+string(first.payload)), first.addr); err != nil {
		t.Fatalf("backend write first reply: %v", err)
	}
	if _, err := backendConn.WriteToUDP([]byte("reply-"+string(second.payload)), second.addr); err != nil {
		t.Fatalf("backend write second reply: %v", err)
	}

	if err := client1.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("client1 SetReadDeadline: %v", err)
	}
	got1 := make([]byte, 128)
	n1, err := client1.Read(got1)
	if err != nil {
		t.Fatalf("client1 read: %v", err)
	}
	if string(got1[:n1]) != "reply-client-one" {
		t.Fatalf("client1 reply = %q, want %q", string(got1[:n1]), "reply-client-one")
	}

	if err := client2.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("client2 SetReadDeadline: %v", err)
	}
	got2 := make([]byte, 128)
	n2, err := client2.Read(got2)
	if err != nil {
		t.Fatalf("client2 read: %v", err)
	}
	if string(got2[:n2]) != "reply-client-two" {
		t.Fatalf("client2 reply = %q, want %q", string(got2[:n2]), "reply-client-two")
	}
}
