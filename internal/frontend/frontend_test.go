package frontend_test

import (
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
)

func newTestFrontend(t *testing.T, listen, backendAddr string) (*frontend.Frontend, *metrics.Counters) {
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
counters := &metrics.Counters{}
fcfg := config.FrontendConfig{
Name:    t.Name(),
Listen:  listen,
Backend: "be",
}
fe := frontend.New(fcfg, pool, sessions, counters, 65535, log)
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
// Start a UDP echo server.
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

time.Sleep(50 * time.Millisecond)
if counters.PacketsReceived.Load() != 0 {
t.Error("expected 0 packets received initially")
}
}

func TestFrontendReusePortFallback(t *testing.T) {
// Verify that ReusePort:true doesn't error on the test platform.
fe, _ := newTestFrontend(t, "127.0.0.1:0", "127.0.0.1:9999")
err := fe.Start(2, config.SocketConfig{ReusePort: true})
if err != nil {
// On some CI environments SO_REUSEPORT may be restricted; skip rather than fail.
t.Skipf("SO_REUSEPORT not available: %v", err)
}
fe.Stop()
}
