// Package backend implements backend server pool management and load balancing.
package backend

import (
"fmt"
"hash/fnv"
"log/slog"
"math/rand"
"net"
"net/netip"
"sync/atomic"

"github.com/ErwinsExpertise/light-udp-proxy/internal/config"
)

// Server represents a single backend server.
type Server struct {
Address   string
Weight    int
Healthy   atomic.Bool
connCount atomic.Int64
addrPort  netip.AddrPort // pre-resolved; used for zero-alloc sends
}

// AddrPort returns the pre-resolved netip.AddrPort for zero-allocation sending.
func (s *Server) AddrPort() netip.AddrPort { return s.addrPort }

// UDPAddr returns the backend address as *net.UDPAddr (for callers that need it).
func (s *Server) UDPAddr() *net.UDPAddr {
return net.UDPAddrFromAddrPort(s.addrPort)
}

// IncrConns increments the active connection count.
func (s *Server) IncrConns() { s.connCount.Add(1) }

// DecrConns decrements the active connection count.
func (s *Server) DecrConns() { s.connCount.Add(-1) }

// ConnCount returns the current active connection count.
func (s *Server) ConnCount() int64 { return s.connCount.Load() }

// Pool manages a set of backend servers and load balancing state.
//
// servers and serverMap are immutable after NewPool returns: no servers are
// added or removed at runtime. Individual server health is tracked by each
// Server.Healthy (atomic.Bool), so no Pool-level lock is needed for reads or
// health updates.
type Pool struct {
servers   []*Server
serverMap map[string]*Server // address → *Server for O(1) lookup
name      string
algorithm string
log       *slog.Logger
rrIndex   atomic.Uint64
}

// NewPool creates a Pool from a BackendConfig.
func NewPool(cfg config.BackendConfig, log *slog.Logger) (*Pool, error) {
p := &Pool{
name:      cfg.Name,
algorithm: cfg.LoadBalance,
log:       log,
servers:   make([]*Server, 0, len(cfg.Servers)),
serverMap: make(map[string]*Server, len(cfg.Servers)),
}
for _, sc := range cfg.Servers {
ap, err := resolveAddrPort(sc.Address)
if err != nil {
return nil, fmt.Errorf("backend %q: resolving server %q: %w", cfg.Name, sc.Address, err)
}
srv := &Server{Address: sc.Address, Weight: sc.Weight, addrPort: ap}
srv.Healthy.Store(true)
p.servers = append(p.servers, srv)
p.serverMap[sc.Address] = srv
}
return p, nil
}

// resolveAddrPort parses a host:port string to netip.AddrPort.
// It tries a direct numeric parse first; falls back to DNS resolution.
func resolveAddrPort(addr string) (netip.AddrPort, error) {
if ap, err := netip.ParseAddrPort(addr); err == nil {
return ap, nil
}
udpAddr, err := net.ResolveUDPAddr("udp", addr)
if err != nil {
return netip.AddrPort{}, err
}
ip, ok := netip.AddrFromSlice(udpAddr.IP)
if !ok {
return netip.AddrPort{}, fmt.Errorf("invalid IP in address %q", addr)
}
return netip.AddrPortFrom(ip.Unmap(), uint16(udpAddr.Port)), nil
}

// Name returns the pool name.
func (p *Pool) Name() string { return p.name }

// Servers returns a snapshot of all servers (healthy and unhealthy).
// Safe to call without a lock because p.servers is immutable after NewPool.
func (p *Pool) Servers() []*Server {
cp := make([]*Server, len(p.servers))
copy(cp, p.servers)
return cp
}

// ServerByAddr returns the server with the given address in O(1), or nil.
// Safe without a lock because p.serverMap is immutable after NewPool.
func (p *Pool) ServerByAddr(addr string) *Server {
return p.serverMap[addr]
}

// healthyServers returns the current list of healthy servers.
// No lock needed: p.servers is immutable; Server.Healthy is atomic.
func (p *Pool) healthyServers() []*Server {
result := make([]*Server, 0, len(p.servers))
for _, s := range p.servers {
if s.Healthy.Load() {
result = append(result, s)
}
}
return result
}

// Pick selects a server using the configured algorithm.
// It uses inline weighted selection to avoid allocations on the hot path.
// clientIP is used for hash-based selection.
func (p *Pool) Pick(clientIP string) (*Server, error) {
healthy := p.healthyServers()
if len(healthy) == 0 {
return nil, fmt.Errorf("backend %q: no healthy servers available", p.name)
}

// Compute total weight without allocating an expanded slice.
totalWeight := 0
for _, s := range healthy {
if s.Weight > 0 {
totalWeight += s.Weight
}
}
if totalWeight == 0 {
return nil, fmt.Errorf("backend %q: no weighted servers available", p.name)
}

switch p.algorithm {
case "round_robin":
idx := int(p.rrIndex.Add(1)-1) % totalWeight
return pickByWeightedIndex(healthy, idx), nil
case "least_conn":
return leastConn(healthy), nil
case "random":
return pickByWeightedIndex(healthy, rand.Intn(totalWeight)), nil //nolint:gosec
case "hash":
h := fnv.New32a()
h.Write([]byte(clientIP))
idx := int(h.Sum32()) % totalWeight
return pickByWeightedIndex(healthy, idx), nil
default:
idx := int(p.rrIndex.Add(1)-1) % totalWeight
return pickByWeightedIndex(healthy, idx), nil
}
}

// pickByWeightedIndex returns the server at position idx in the conceptual
// weight-expanded list (no allocation). idx must be in [0, totalWeight).
func pickByWeightedIndex(servers []*Server, idx int) *Server {
for _, s := range servers {
if s.Weight <= 0 {
continue
}
if idx < s.Weight {
return s
}
idx -= s.Weight
}
// This path indicates a caller bug (idx out of range for totalWeight).
// In tests this signals an implementation error and should be caught by
// code review; in production we fall back gracefully to avoid dropping
// traffic rather than panic-ing a live proxy process.
if len(servers) > 0 {
return servers[0]
}
return nil
}

// leastConn selects the server with the fewest active connections.
// connCount is incremented on session creation and decremented on session
// eviction/deletion, so this reflects the actual number of active sessions
// mapped to each server.
func leastConn(servers []*Server) *Server {
var best *Server
for _, s := range servers {
if best == nil || s.ConnCount() < best.ConnCount() {
best = s
}
}
return best
}

// SetHealthy marks a server healthy or unhealthy by address.
// No lock needed: p.servers is immutable; Server.Healthy is atomic.
func (p *Pool) SetHealthy(addr string, healthy bool) {
for _, s := range p.servers {
if s.Address == addr {
old := s.Healthy.Swap(healthy)
if old != healthy {
if healthy {
p.log.Info("backend server recovered", "backend", p.name, "server", addr)
} else {
p.log.Warn("backend server unhealthy", "backend", p.name, "server", addr)
}
}
return
}
}
}

