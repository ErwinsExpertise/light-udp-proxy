package frontend_test

import (
"fmt"
"log/slog"
"net"
"os"
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
