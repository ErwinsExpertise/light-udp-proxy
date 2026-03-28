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
// If the backend implements RFC 862 UDP echo it will reply; most services will
// not reply and the read-timeout after a successful write still counts as healthy.
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
	c.checkAll(ctx)

	ticker := time.NewTicker(c.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.checkAll(ctx)
		}
	}
}

// checkAll probes every server in the pool concurrently and updates their
// health status. Running probes in parallel prevents a single timed-out server
// from delaying the checks of the remaining servers.
//
// ctx is forwarded to each probe so that context cancellation (e.g. proxy
// shutdown) interrupts in-flight I/O promptly instead of waiting for the full
// configured timeout to expire.
func (c *Checker) checkAll(ctx context.Context) {
	servers := c.pool.Servers()
	var wg sync.WaitGroup
	wg.Add(len(servers))
	for _, srv := range servers {
		srv := srv
		go func() {
			defer wg.Done()
			healthy := c.probe(ctx, srv.Address)
			c.pool.SetHealthy(srv.Address, healthy)
		}()
	}
	wg.Wait()
}

// probe performs an L4 UDP health check against addr.
//
// A small probe payload is sent and the code waits up to the configured timeout
// for any response. The outcome is interpreted as follows:
//   - Response received: the backend is reachable and responded — healthy.
//   - Read deadline exceeded: the port is reachable but the service does not
//     reply (normal for most UDP services) — healthy.
//   - Non-timeout error (e.g. ICMP port-unreachable): the backend is not
//     reachable at the network/transport layer — unhealthy.
//
// ctx is used for the dial phase and to interrupt in-flight I/O on cancellation.
func (c *Checker) probe(ctx context.Context, addr string) bool {
	// Bound the probe by both the configured timeout and the parent context.
	probeCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(probeCtx, "udp", addr)
	if err != nil {
		if ctx.Err() == nil {
			c.log.Debug("health check dial failed", "addr", addr, "err", err)
		}
		return false
	}
	defer conn.Close()

	// Set the connection deadline to the probe context deadline so both Write
	// and Read are bounded by the configured timeout.
	if deadline, ok := probeCtx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			c.log.Debug("health check failed to set connection deadline", "addr", addr, "err", err)
			return false
		}
	}

	// If the parent context is cancelled while I/O is in flight (e.g. proxy
	// shutdown), immediately unblock any pending Read/Write by expiring the
	// connection deadline rather than waiting for the full probe timeout.
	connDone := make(chan struct{})
	defer close(connDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-connDone:
		}
	}()

	// Send the L4 probe payload.
	if _, err := conn.Write(probePayload); err != nil {
		if ctx.Err() == nil {
			c.log.Debug("health check write failed", "addr", addr, "err", err)
		}
		return false
	}

	// Wait for any response within the configured timeout. Any reply indicates
	// L4 reachability; we do not require the response to match probePayload
	// since most UDP services are not RFC 862 echo servers.
	// An ICMP port-unreachable or similar error surfaces here as a non-timeout
	// error, indicating the backend is not reachable.
	buf := make([]byte, len(probePayload))
	_, err = conn.Read(buf)
	if err == nil {
		// The backend responded — L4 reachability confirmed.
		return true
	}
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		if ctx.Err() != nil {
			// Deadline was caused by context cancellation; treat as indeterminate.
			return false
		}
		// Read timed out: the port is reachable but the service doesn't reply.
		return true
	}
	if ctx.Err() == nil {
		c.log.Debug("health check probe error", "addr", addr, "err", err)
	}
	return false
}
