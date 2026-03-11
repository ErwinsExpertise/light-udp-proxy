// Package frontend implements a UDP listening frontend that routes packets to a backend pool.
package frontend

import (
"context"
"fmt"
"log/slog"
"net"
"net/netip"
"sync"
"time"

"github.com/ErwinsExpertise/light-udp-proxy/internal/backend"
"github.com/ErwinsExpertise/light-udp-proxy/internal/config"
"github.com/ErwinsExpertise/light-udp-proxy/internal/metrics"
"github.com/ErwinsExpertise/light-udp-proxy/internal/session"
)

// pkt is a forwarding work item passed from reader goroutines to forwarding workers.
// The bufPtr field is owned by the work item until the worker returns it to the pool.
type pkt struct {
clientAddr netip.AddrPort // no-alloc address from ReadFromUDPAddrPort
bufPtr     *[]byte        // buffer from sync.Pool; returned after send
n          int            // number of valid bytes in *bufPtr
}

// Frontend listens on a UDP port and proxies packets to a backend pool.
type Frontend struct {
cfg      config.FrontendConfig
pool     *backend.Pool
sessions *session.Table
counters *metrics.Counters
log      *slog.Logger

// listenConns holds one socket per reader goroutine. When SO_REUSEPORT is
// enabled the kernel distributes packets across them; otherwise a single
// socket is used by all readers.
listenConns []*net.UDPConn
// sendConn is a shared unconnected socket used by forwarding workers.
// Using a single shared socket avoids the cost of creating a new connection
// per packet.
sendConn *net.UDPConn

stopOnce sync.Once
stopCh   chan struct{}
wg       sync.WaitGroup

// workCh is the bounded channel from readers to forwarding workers.
workCh chan pkt

// bufPool re-uses packet buffers to eliminate per-packet allocations.
bufPool sync.Pool

maxPacketSize int
}

// New creates a Frontend.
func New(
cfg config.FrontendConfig,
pool *backend.Pool,
sessions *session.Table,
counters *metrics.Counters,
maxPacketSize int,
log *slog.Logger,
) *Frontend {
if cfg.MaxPacketSize > 0 {
maxPacketSize = cfg.MaxPacketSize
}
f := &Frontend{
cfg:           cfg,
pool:          pool,
sessions:      sessions,
counters:      counters,
log:           log,
stopCh:        make(chan struct{}),
maxPacketSize: maxPacketSize,
}
f.bufPool = sync.Pool{
New: func() any {
b := make([]byte, maxPacketSize)
return &b
},
}
return f
}

// Start binds listener socket(s) and launches reader + forwarding worker goroutines.
func (f *Frontend) Start(workerCount int, socketCfg config.SocketConfig) error {
if workerCount < 1 {
workerCount = 1
}

lc := newListenConfig(socketCfg)

// With SO_REUSEPORT we create one socket per reader so the kernel can
// distribute incoming datagrams across cores without a shared lock.
// Without it we share a single socket across all readers.
numListeners := 1
if socketCfg.ReusePort && workerCount > 1 {
numListeners = workerCount
}

for i := 0; i < numListeners; i++ {
pc, err := lc.ListenPacket(context.Background(), "udp", f.cfg.Listen)
if err != nil {
f.closeListeners()
return fmt.Errorf("frontend %q: listen: %w", f.cfg.Name, err)
}
f.listenConns = append(f.listenConns, pc.(*net.UDPConn))
}

// Shared outbound socket for forwarding workers.
sendPC, err := lc.ListenPacket(context.Background(), "udp", "")
if err != nil {
f.closeListeners()
return fmt.Errorf("frontend %q: opening send socket: %w", f.cfg.Name, err)
}
f.sendConn = sendPC.(*net.UDPConn)

// Bounded work channel: if workers fall behind, excess packets are dropped
// rather than accumulating unbounded memory.
fwdWorkers := workerCount * 4
if fwdWorkers < 8 {
fwdWorkers = 8
}
f.workCh = make(chan pkt, fwdWorkers*2)

f.log.Info("frontend started",
"name", f.cfg.Name,
"listen", f.listenConns[0].LocalAddr(),
"readers", numListeners,
"fwd_workers", fwdWorkers,
"reuse_port", socketCfg.ReusePort,
)

// Start forwarding workers.
for i := 0; i < fwdWorkers; i++ {
f.wg.Add(1)
go f.forwardWorker()
}

// Start reader goroutines. If there are multiple listener sockets (SO_REUSEPORT),
// each reader is pinned to its own socket; otherwise all readers share one socket.
for i := 0; i < workerCount; i++ {
conn := f.listenConns[i%len(f.listenConns)]
f.wg.Add(1)
go f.readLoop(conn)
}

return nil
}

// Stop signals the frontend to stop and waits for all goroutines to exit.
func (f *Frontend) Stop() {
f.stopOnce.Do(func() {
close(f.stopCh)
f.closeListeners()
if f.sendConn != nil {
f.sendConn.Close()
}
})
f.wg.Wait()
f.log.Info("frontend stopped", "name", f.cfg.Name)
}

// Name returns the frontend name.
func (f *Frontend) Name() string { return f.cfg.Name }

func (f *Frontend) closeListeners() {
for _, c := range f.listenConns {
c.Close()
}
}

// readLoop reads datagrams from conn and enqueues them for forwarding workers.
// It uses ReadFromUDPAddrPort to obtain the client address without heap allocation.
func (f *Frontend) readLoop(conn *net.UDPConn) {
defer f.wg.Done()

for {
select {
case <-f.stopCh:
return
default:
}

bufPtr := f.bufPool.Get().(*[]byte)
buf := *bufPtr

n, clientAddr, err := conn.ReadFromUDPAddrPort(buf)
if err != nil {
f.bufPool.Put(bufPtr)
select {
case <-f.stopCh:
return
default:
f.log.Debug("read error", "frontend", f.cfg.Name, "err", err)
continue
}
}

f.counters.PacketsReceived.Add(1)
f.counters.BytesIn.Add(int64(n))

// Drop oversized packets.
if n > f.maxPacketSize {
f.counters.PacketsDropped.Add(1)
f.log.Warn("packet too large, dropping",
"frontend", f.cfg.Name,
"client", clientAddr,
"size", n,
"max", f.maxPacketSize)
f.bufPool.Put(bufPtr)
continue
}

// Session cap: drop new-session packets once the limit is reached.
if f.cfg.MaxSessions > 0 && f.sessions.Len() >= f.cfg.MaxSessions {
key := session.Key{Addr: clientAddr, FrontendName: f.cfg.Name}
if f.sessions.Get(key) == nil {
f.counters.PacketsDropped.Add(1)
f.bufPool.Put(bufPtr)
continue
}
}

// Zero-copy: transfer buffer ownership to the work item.
// The forwarding worker returns bufPtr to the pool after sending.
p := pkt{clientAddr: clientAddr, bufPtr: bufPtr, n: n}
select {
case f.workCh <- p:
default:
// Worker pool saturated — drop to preserve stability.
f.counters.PacketsDropped.Add(1)
f.bufPool.Put(bufPtr)
}
}
}

// forwardWorker drains workCh and forwards each packet.
func (f *Frontend) forwardWorker() {
defer f.wg.Done()
for {
select {
case <-f.stopCh:
// Drain any remaining work and return buffers to pool.
for {
select {
case p := <-f.workCh:
f.bufPool.Put(p.bufPtr)
default:
return
}
}
case p, ok := <-f.workCh:
if !ok {
return
}
f.forward(p)
// Return buffer to pool now that the send is done.
f.bufPool.Put(p.bufPtr)
}
}
}

// forward routes a packet to the appropriate backend server.
func (f *Frontend) forward(p pkt) {
key := session.Key{Addr: p.clientAddr, FrontendName: f.cfg.Name}
payload := (*p.bufPtr)[:p.n]

var srv *backend.Server

if f.cfg.SessionAffinity {
clientIP := p.clientAddr.Addr().String()
sess, created := f.sessions.GetOrCreate(key, func() *session.Session {
s, err := f.pool.Pick(clientIP)
if err != nil {
return nil
}
return &session.Session{
BackendAddr: s.Address,
LastSeen:    time.Now(),
}
})
if sess == nil {
f.counters.PacketsDropped.Add(1)
return
}
if created {
f.counters.ActiveSessions.Add(1)
}
sess.Touch(int64(p.n))

srv = f.serverByAddr(sess.BackendAddr)
if srv == nil || !srv.Healthy.Load() {
// Session server is gone; re-pick and update the sticky binding.
newSrv, err := f.pool.Pick(p.clientAddr.Addr().String())
if err != nil {
f.counters.PacketsDropped.Add(1)
return
}
sess.BackendAddr = newSrv.Address
srv = newSrv
}
} else {
var err error
srv, err = f.pool.Pick(p.clientAddr.Addr().String())
if err != nil {
f.counters.PacketsDropped.Add(1)
f.log.Warn("no healthy backend", "frontend", f.cfg.Name, "err", err)
return
}
}

// Use WriteToUDPAddrPort for a zero-allocation send path.
if _, err := f.sendConn.WriteToUDPAddrPort(payload, srv.AddrPort()); err != nil {
f.counters.PacketsDropped.Add(1)
f.log.Debug("send error", "frontend", f.cfg.Name, "backend", srv.Address, "err", err)
return
}

f.counters.PacketsForwarded.Add(1)
f.counters.BytesOut.Add(int64(p.n))
}

func (f *Frontend) serverByAddr(addr string) *backend.Server {
for _, s := range f.pool.Servers() {
if s.Address == addr {
return s
}
}
return nil
}
