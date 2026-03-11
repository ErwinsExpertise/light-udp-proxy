// Package healthcheck implements periodic UDP health checking of backend servers.
package healthcheck

import (
	"context"
	"log/slog"
	"net"
	"time"

	"github.com/ErwinsExpertise/light-udp-proxy/internal/backend"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/config"
)

// Checker runs periodic health checks for a backend pool.
type Checker struct {
	pool     *backend.Pool
	cfg      config.HealthCheckConfig
	log      *slog.Logger
}

// New creates a Checker for the given pool.
func New(pool *backend.Pool, cfg config.HealthCheckConfig, log *slog.Logger) *Checker {
	return &Checker{pool: pool, cfg: cfg, log: log}
}

// Run starts the health checking loop; it blocks until ctx is cancelled.
func (c *Checker) Run(ctx context.Context) {
	if !c.cfg.Enabled {
		return
	}
	ticker := time.NewTicker(c.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.checkAll()
		}
	}
}

func (c *Checker) checkAll() {
	for _, srv := range c.pool.Servers() {
		healthy := c.probe(srv.Address)
		c.pool.SetHealthy(srv.Address, healthy)
	}
}

// probe attempts a UDP dial-and-write to addr.
// Since UDP is connectionless, we verify reachability by attempting a zero-byte
// send; absence of an ICMP port-unreachable within the timeout counts as healthy.
func (c *Checker) probe(addr string) bool {
	conn, err := net.DialTimeout("udp", addr, c.cfg.Timeout)
	if err != nil {
		c.log.Debug("health check dial failed", "addr", addr, "err", err)
		return false
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(c.cfg.Timeout)); err != nil {
		return false
	}
	// Send a zero-byte payload.
	if _, err := conn.Write([]byte{}); err != nil {
		c.log.Debug("health check write failed", "addr", addr, "err", err)
		return false
	}
	// Try a read with short timeout; ICMP unreachable surfaces as an error.
	buf := make([]byte, 1)
	conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)) //nolint:errcheck
	_, err = conn.Read(buf)
	// A timeout is fine (no response expected from most services).
	// An error that is NOT a timeout means the port is unreachable.
	if err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			return true
		}
		c.log.Debug("health check read error", "addr", addr, "err", err)
		return false
	}
	return true
}
