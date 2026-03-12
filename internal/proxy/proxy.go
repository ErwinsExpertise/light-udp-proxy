// Package proxy wires together frontends, backends, health checkers and the metrics server.
package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"

	"github.com/ErwinsExpertise/light-udp-proxy/internal/abuse"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/backend"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/config"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/frontend"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/healthcheck"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/metrics"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/qos"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/session"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/shaping"
	"github.com/ErwinsExpertise/light-udp-proxy/pkg/logger"
)

// Proxy orchestrates the entire UDP proxy lifecycle.
type Proxy struct {
	configPath string
	cfg        *config.Config
	log        *slog.Logger
	counters   *metrics.Counters
	metrics    *metrics.Server
	frontends  []*frontend.Frontend
	backends   map[string]*backend.Pool
	sessions   *session.Table
	hcCancel   context.CancelFunc
	hcWg       sync.WaitGroup // tracks running health-checker goroutines
}

// New creates a Proxy from a config file path.
func New(configPath string) (*Proxy, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	log := logger.New(cfg.Global.LogLevel)
	return &Proxy{
		configPath: configPath,
		cfg:        cfg,
		log:        log,
		counters:   &metrics.Counters{},
		backends:   make(map[string]*backend.Pool),
	}, nil
}

// Run starts the proxy and blocks until a shutdown signal is received.
func (p *Proxy) Run() error {
	if err := p.start(); err != nil {
		return err
	}
	defer p.stop()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for sig := range sigCh {
		switch sig {
		case syscall.SIGHUP:
			p.log.Info("received SIGHUP, reloading config")
			if err := p.reload(); err != nil {
				p.log.Error("config reload failed", "err", err)
			}
		default:
			p.log.Info("received shutdown signal", "signal", sig)
			return nil
		}
	}
	return nil
}

// start initialises and starts all components.
func (p *Proxy) start() error {
	// Build backend pools.
	for _, bcfg := range p.cfg.Backends {
		pool, err := backend.NewPool(bcfg, p.log)
		if err != nil {
			return err
		}
		p.backends[bcfg.Name] = pool
		p.log.Info("backend pool created", "name", bcfg.Name, "servers", len(bcfg.Servers))
	}

	// Session table (shared across frontends).
	p.sessions = session.NewTable(p.cfg.Global.SessionTimeout, p.cfg.Global.SessionCleanupInterval)

	// Metrics server.
	p.metrics = metrics.New(p.cfg.Global.MetricsAddr, p.counters, p.log)
	for _, pool := range p.backends {
		pool := pool // capture
		p.metrics.RegisterBackend(func() []metrics.BackendStatus {
			var statuses []metrics.BackendStatus
			for _, srv := range pool.Servers() {
				statuses = append(statuses, metrics.BackendStatus{
					Address: srv.Address,
					Healthy: srv.Healthy.Load(),
					Conns:   srv.ConnCount(),
				})
			}
			return statuses
		})
	}
	// Feed active_sessions from the session table so the count is always accurate
	// (incremented on create, decremented on eviction/deletion).
	sessions := p.sessions
	p.metrics.RegisterSessionCount(func() int64 { return sessions.Count() })
	if err := p.metrics.Start(); err != nil {
		return err
	}

	// Start health checkers.
	hcCtx, hcCancel := context.WithCancel(context.Background())
	p.hcCancel = hcCancel
	for _, bcfg := range p.cfg.Backends {
		pool := p.backends[bcfg.Name]
		checker := healthcheck.New(pool, bcfg.HealthCheck, p.log)
		p.hcWg.Add(1)
		go func() {
			defer p.hcWg.Done()
			checker.Run(hcCtx)
		}()
	}

	// Start frontends.
	globalShaper := shaping.NewBucket(limitsFromTrafficConfig(p.cfg.Global.TrafficShaping))
	clientShaper := shaping.NewClientLimiter(shaping.Limits{
		PacketsPerSecond: p.cfg.Global.ClientLimits.PacketsPerSecond,
		BurstPackets:     p.cfg.Global.ClientLimits.BurstPackets,
	}, p.cfg.Global.SessionTimeout)
	abuseProtector := abuse.New(abuse.Config{
		Enabled:              p.cfg.Global.AbuseProtection.Enabled,
		MaxPacketsPerSecond:  p.cfg.Global.AbuseProtection.MaxPacketsPerSecondPerIP,
		MaxSessionsPerClient: p.cfg.Global.AbuseProtection.MaxSessionsPerIP,
		SessionTTL:           p.cfg.Global.SessionTimeout,
	})
	backendShapers := make(map[string]*shaping.Bucket, len(p.cfg.Backends))
	for _, bcfg := range p.cfg.Backends {
		backendShapers[bcfg.Name] = shaping.NewBucket(limitsFromTrafficConfig(bcfg.TrafficShaping))
	}

	type frontendEntry struct {
		cfg      config.FrontendConfig
		priority qos.Priority
	}
	ordered := make([]frontendEntry, 0, len(p.cfg.Frontends))
	for _, fcfg := range p.cfg.Frontends {
		pv, err := qos.ParsePriority(fcfg.Priority)
		if err != nil {
			return fmt.Errorf("frontend %q: %w", fcfg.Name, err)
		}
		ordered = append(ordered, frontendEntry{cfg: fcfg, priority: pv})
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].priority > ordered[j].priority
	})
	for _, item := range ordered {
		fcfg := item.cfg
		pool, ok := p.backends[fcfg.Backend]
		if !ok {
			return fmt.Errorf("frontend %q: unknown backend %q", fcfg.Name, fcfg.Backend)
		}
		workers := p.cfg.Global.WorkerThreads * item.priority.WorkerMultiplier() / qos.PriorityNormal.WorkerMultiplier()
		if workers < 1 {
			workers = 1
		}
		fe := frontend.New(fcfg, pool, p.sessions, p.counters,
			p.cfg.Global.MaxPacketSize,
			frontend.RuntimeOptions{
				GlobalShaper:   globalShaper,
				FrontendShaper: shaping.NewBucket(limitsFromTrafficConfig(fcfg.TrafficShaping)),
				BackendShaper:  backendShapers[fcfg.Backend],
				ClientShaper:   clientShaper,
				AbuseProtector: abuseProtector,
				DropFragments:  p.cfg.Global.Fragmentation.DropFragments,
			},
			p.log)
		if err := fe.Start(workers, p.cfg.Global.Socket); err != nil {
			return err
		}
		p.frontends = append(p.frontends, fe)
	}
	return nil
}

// stop shuts down all components in reverse order.
func (p *Proxy) stop() {
	for _, fe := range p.frontends {
		fe.Stop()
	}
	if p.hcCancel != nil {
		p.hcCancel()
		p.hcWg.Wait() // wait for all checker goroutines to exit
	}
	if p.metrics != nil {
		p.metrics.Stop()
	}
	if p.sessions != nil {
		p.sessions.Stop()
	}
	p.log.Info("proxy stopped")
}

// reload re-reads the config and restarts frontends/backends.
func (p *Proxy) reload() error {
	newCfg, err := config.Load(p.configPath)
	if err != nil {
		return err
	}
	p.log.Info("config reloaded; restarting components")
	p.stop()
	p.cfg = newCfg
	p.log = logger.New(newCfg.Global.LogLevel)
	p.frontends = nil
	p.backends = make(map[string]*backend.Pool)
	p.counters = &metrics.Counters{}
	return p.start()
}

func limitsFromTrafficConfig(cfg config.TrafficShapingConfig) shaping.Limits {
	return shaping.Limits{
		Enabled:          cfg.Enabled,
		PacketsPerSecond: cfg.PacketsPerSecond,
		BytesPerSecond:   int64(cfg.BytesPerSecond),
		BurstPackets:     cfg.BurstPackets,
		BurstBytes:       int64(cfg.BurstBytes),
	}
}
