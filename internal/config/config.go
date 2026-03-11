// Package config loads and validates the YAML configuration file.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration structure.
type Config struct {
	Global    GlobalConfig    `yaml:"global"`
	Frontends []FrontendConfig `yaml:"frontends"`
	Backends  []BackendConfig  `yaml:"backends"`
}

// GlobalConfig contains global proxy settings.
type GlobalConfig struct {
	MaxPacketSize   int           `yaml:"max_packet_size"`
	WorkerThreads   int           `yaml:"worker_threads"`
	ReadBufferSize  int           `yaml:"read_buffer_size"`
	WriteBufferSize int           `yaml:"write_buffer_size"`
	SessionTimeout  time.Duration `yaml:"session_timeout"`
	LogLevel        string        `yaml:"log_level"`
	MetricsAddr     string        `yaml:"metrics_addr"`
}

// FrontendConfig defines a listening frontend.
type FrontendConfig struct {
	Name            string `yaml:"name"`
	Listen          string `yaml:"listen"`
	Backend         string `yaml:"backend"`
	SessionAffinity bool   `yaml:"session_affinity"`
	// Optional rate limiting
	RateLimit      int `yaml:"rate_limit"`       // packets per second per client, 0 = unlimited
	MaxSessions    int `yaml:"max_sessions"`     // max concurrent sessions, 0 = unlimited
	MaxPacketSize  int `yaml:"max_packet_size"`  // override global, 0 = use global
}

// BackendConfig defines a pool of servers.
type BackendConfig struct {
	Name        string        `yaml:"name"`
	LoadBalance string        `yaml:"load_balance"`
	HealthCheck HealthCheckConfig `yaml:"health_check"`
	Servers     []ServerConfig    `yaml:"servers"`
}

// HealthCheckConfig controls periodic backend health checks.
type HealthCheckConfig struct {
	Enabled  bool          `yaml:"enabled"`
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
}

// ServerConfig represents a single backend server.
type ServerConfig struct {
	Address string `yaml:"address"`
	Weight  int    `yaml:"weight"`
}

// defaults applies sensible defaults to the config.
func (c *Config) defaults() {
	if c.Global.MaxPacketSize <= 0 {
		c.Global.MaxPacketSize = 65535
	}
	if c.Global.WorkerThreads <= 0 {
		c.Global.WorkerThreads = 4
	}
	if c.Global.ReadBufferSize <= 0 {
		c.Global.ReadBufferSize = 4 * 1024 * 1024
	}
	if c.Global.WriteBufferSize <= 0 {
		c.Global.WriteBufferSize = 4 * 1024 * 1024
	}
	if c.Global.SessionTimeout <= 0 {
		c.Global.SessionTimeout = 60 * time.Second
	}
	if c.Global.LogLevel == "" {
		c.Global.LogLevel = "info"
	}
	if c.Global.MetricsAddr == "" {
		c.Global.MetricsAddr = "0.0.0.0:9090"
	}
	for i := range c.Backends {
		if c.Backends[i].LoadBalance == "" {
			c.Backends[i].LoadBalance = "round_robin"
		}
		if c.Backends[i].HealthCheck.Interval <= 0 {
			c.Backends[i].HealthCheck.Interval = 10 * time.Second
		}
		if c.Backends[i].HealthCheck.Timeout <= 0 {
			c.Backends[i].HealthCheck.Timeout = 2 * time.Second
		}
		for j := range c.Backends[i].Servers {
			if c.Backends[i].Servers[j].Weight <= 0 {
				c.Backends[i].Servers[j].Weight = 1
			}
		}
	}
}

// validate checks the config for required fields.
func (c *Config) validate() error {
	if len(c.Frontends) == 0 {
		return fmt.Errorf("at least one frontend must be defined")
	}
	if len(c.Backends) == 0 {
		return fmt.Errorf("at least one backend must be defined")
	}

	backendNames := make(map[string]struct{}, len(c.Backends))
	for _, b := range c.Backends {
		if b.Name == "" {
			return fmt.Errorf("backend name must not be empty")
		}
		if len(b.Servers) == 0 {
			return fmt.Errorf("backend %q must have at least one server", b.Name)
		}
		switch b.LoadBalance {
		case "round_robin", "least_conn", "random", "hash":
		default:
			return fmt.Errorf("backend %q: unknown load_balance algorithm %q", b.Name, b.LoadBalance)
		}
		for _, s := range b.Servers {
			if s.Address == "" {
				return fmt.Errorf("backend %q: server address must not be empty", b.Name)
			}
		}
		backendNames[b.Name] = struct{}{}
	}

	frontendNames := make(map[string]struct{}, len(c.Frontends))
	for _, f := range c.Frontends {
		if f.Name == "" {
			return fmt.Errorf("frontend name must not be empty")
		}
		if f.Listen == "" {
			return fmt.Errorf("frontend %q: listen address must not be empty", f.Name)
		}
		if f.Backend == "" {
			return fmt.Errorf("frontend %q: backend must not be empty", f.Name)
		}
		if _, ok := backendNames[f.Backend]; !ok {
			return fmt.Errorf("frontend %q references unknown backend %q", f.Name, f.Backend)
		}
		if _, dup := frontendNames[f.Name]; dup {
			return fmt.Errorf("duplicate frontend name %q", f.Name)
		}
		frontendNames[f.Name] = struct{}{}
	}
	return nil
}

// Load reads a YAML file at path and returns the parsed Config.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}
	return Parse(data)
}

// Parse decodes YAML bytes into a Config.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	cfg.defaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return &cfg, nil
}
