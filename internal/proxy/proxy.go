// Package proxy wires together frontends, backends, health checkers and the metrics server.
package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ErwinsExpertise/light-udp-proxy/internal/backend"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/config"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/frontend"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/healthcheck"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/metrics"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/session"
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
	p.sessions = session.NewTable(p.cfg.Global.SessionTimeout)

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
	if err := p.metrics.Start(); err != nil {
		return err
	}

	// Start health checkers.
	hcCtx, hcCancel := context.WithCancel(context.Background())
	p.hcCancel = hcCancel
	for _, bcfg := range p.cfg.Backends {
		pool := p.backends[bcfg.Name]
		checker := healthcheck.New(pool, bcfg.HealthCheck, p.log)
		go checker.Run(hcCtx)
	}

	// Start frontends.
	for _, fcfg := range p.cfg.Frontends {
		pool, ok := p.backends[fcfg.Backend]
		if !ok {
			return fmt.Errorf("frontend %q: unknown backend %q", fcfg.Name, fcfg.Backend)
		}
		fe := frontend.New(fcfg, pool, p.sessions, p.counters,
			p.cfg.Global.MaxPacketSize, p.log)
		if err := fe.Start(p.cfg.Global.WorkerThreads,
			p.cfg.Global.ReadBufferSize, p.cfg.Global.WriteBufferSize); err != nil {
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
	}
	if p.metrics != nil {
		p.metrics.Stop()
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

