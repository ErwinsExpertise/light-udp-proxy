// Package healthcheck implements periodic UDP health checking of backend servers.
package healthcheck

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/ErwinsExpertise/light-udp-proxy/internal/backend"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/config"
)

// probePayload is the small L4 UDP probe sent to each backend.
// If the backend implements RFC 862 UDP echo it will reply with the same bytes,
// confirming L4 reachability. Most services do not implement echo and will not
// reply; a read-timeout after a successful write still counts as healthy.
var probePayload = []byte{0x00, 0x00, 0x00, 0x01}

// Checker runs periodic health checks for a backend pool.
type Checker struct {
	pool *backend.Pool
	cfg  config.HealthCheckConfig
	log  *slog.Logger
}

// New creates a Checker for the given pool.
func New(pool *backend.Pool, cfg config.HealthCheckConfig, log *slog.Logger) *Checker {
	return &Checker{pool: pool, cfg: cfg, log: log}
}

// Run starts the health checking loop; it blocks until ctx is cancelled.
// An initial check is performed immediately so backends are probed before the
// first interval fires, preventing stale healthy-by-default state at startup.
func (c *Checker) Run(ctx context.Context) {
	if !c.cfg.Enabled {
		return
	}
	// Probe all servers immediately on start.
	c.checkAll()

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

// checkAll probes every server in the pool concurrently and updates their
// health status. Running probes in parallel prevents a single timed-out server
// from delaying the checks of the remaining servers.
func (c *Checker) checkAll() {
	servers := c.pool.Servers()
	var wg sync.WaitGroup
	wg.Add(len(servers))
	for _, srv := range servers {
		srv := srv
		go func() {
			defer wg.Done()
			healthy := c.probe(srv.Address)
			c.pool.SetHealthy(srv.Address, healthy)
		}()
	}
	wg.Wait()
}

// probe performs an L4 UDP echo health check against addr.
//
// A small probe payload is sent and the code waits up to the configured timeout
// for any response. The outcome is interpreted as follows:
//   - Response received: the backend echoed the payload — L4 echo confirmed, healthy.
//   - Read deadline exceeded: the port is reachable but the service does not
//     implement echo (normal for most UDP services) — healthy.
//   - Non-timeout error (e.g. ICMP port-unreachable): the backend is not
//     reachable at the network/transport layer — unhealthy.
func (c *Checker) probe(addr string) bool {
	conn, err := net.DialTimeout("udp", addr, c.cfg.Timeout)
	if err != nil {
		c.log.Debug("health check dial failed", "addr", addr, "err", err)
		return false
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(c.cfg.Timeout)); err != nil {
		c.log.Debug("health check failed to set connection deadline", "addr", addr, "err", err)
		return false
	}

	// Send the L4 probe payload.
	if _, err := conn.Write(probePayload); err != nil {
		c.log.Debug("health check write failed", "addr", addr, "err", err)
		return false
	}

	// Wait for an echo response within the configured timeout.
	// An ICMP port-unreachable or similar error surfaces here as a non-timeout
	// error, indicating the backend is not reachable.
	buf := make([]byte, len(probePayload))
	_, err = conn.Read(buf)
	if err == nil {
		// The backend replied — L4 echo confirmed.
		return true
	}
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		// Read timed out: the port is reachable but the service doesn't echo.
		return true
	}
	c.log.Debug("health check probe error", "addr", addr, "err", err)
	return false
}
