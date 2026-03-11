// Package backend implements backend server pool management and load balancing.
package backend

import (
	"fmt"
	"hash/fnv"
	"log/slog"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"

	"github.com/ErwinsExpertise/light-udp-proxy/internal/config"
)

// Server represents a single backend server.
type Server struct {
	Address  string
	Weight   int
	Healthy  atomic.Bool
	connCount atomic.Int64
	udpAddr  *net.UDPAddr
}

// UDPAddr returns the parsed UDP address (cached).
func (s *Server) UDPAddr() (*net.UDPAddr, error) {
	if s.udpAddr != nil {
		return s.udpAddr, nil
	}
	addr, err := net.ResolveUDPAddr("udp", s.Address)
	if err != nil {
		return nil, err
	}
	s.udpAddr = addr
	return addr, nil
}

// IncrConns increments the active connection count.
func (s *Server) IncrConns() { s.connCount.Add(1) }

// DecrConns decrements the active connection count.
func (s *Server) DecrConns() { s.connCount.Add(-1) }

// ConnCount returns the current active connection count.
func (s *Server) ConnCount() int64 { return s.connCount.Load() }

// Pool manages a set of backend servers and load balancing state.
type Pool struct {
	mu          sync.RWMutex
	servers     []*Server
	name        string
	algorithm   string
	log         *slog.Logger
	rrIndex     atomic.Uint64
}

// NewPool creates a Pool from a BackendConfig.
func NewPool(cfg config.BackendConfig, log *slog.Logger) (*Pool, error) {
	p := &Pool{
		name:      cfg.Name,
		algorithm: cfg.LoadBalance,
		log:       log,
		servers:   make([]*Server, 0, len(cfg.Servers)),
	}
	for _, sc := range cfg.Servers {
		srv := &Server{Address: sc.Address, Weight: sc.Weight}
		srv.Healthy.Store(true)
		// Pre-resolve to catch config errors early.
		if _, err := srv.UDPAddr(); err != nil {
			return nil, fmt.Errorf("backend %q: resolving server %q: %w", cfg.Name, sc.Address, err)
		}
		p.servers = append(p.servers, srv)
	}
	return p, nil
}

// Name returns the pool name.
func (p *Pool) Name() string { return p.name }

// Servers returns a snapshot of all servers (healthy and unhealthy).
func (p *Pool) Servers() []*Server {
	p.mu.RLock()
	cp := make([]*Server, len(p.servers))
	copy(cp, p.servers)
	p.mu.RUnlock()
	return cp
}

// healthyServers returns the current list of healthy servers (caller must NOT hold p.mu).
func (p *Pool) healthyServers() []*Server {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]*Server, 0, len(p.servers))
	for _, s := range p.servers {
		if s.Healthy.Load() {
			result = append(result, s)
		}
	}
	return result
}

// Pick selects a server using the configured algorithm.
// clientIP is used for hash-based selection.
func (p *Pool) Pick(clientIP string) (*Server, error) {
	healthy := p.healthyServers()
	if len(healthy) == 0 {
		return nil, fmt.Errorf("backend %q: no healthy servers available", p.name)
	}

	// Expand by weight.
	weighted := expandByWeight(healthy)
	if len(weighted) == 0 {
		return nil, fmt.Errorf("backend %q: no weighted servers available", p.name)
	}

	switch p.algorithm {
	case "round_robin":
		idx := p.rrIndex.Add(1) - 1
		return weighted[idx%uint64(len(weighted))], nil
	case "least_conn":
		return leastConn(healthy), nil
	case "random":
		return weighted[rand.Intn(len(weighted))], nil //nolint:gosec
	case "hash":
		h := fnv.New32a()
		h.Write([]byte(clientIP))
		idx := int(h.Sum32()) % len(weighted)
		return weighted[idx], nil
	default:
		idx := p.rrIndex.Add(1) - 1
		return weighted[idx%uint64(len(weighted))], nil
	}
}

// expandByWeight creates a slice where each server appears Weight times.
func expandByWeight(servers []*Server) []*Server {
	var result []*Server
	for _, s := range servers {
		for i := 0; i < s.Weight; i++ {
			result = append(result, s)
		}
	}
	return result
}

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
func (p *Pool) SetHealthy(addr string, healthy bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
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
