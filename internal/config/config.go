// Package config loads and validates the YAML configuration file.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ErwinsExpertise/light-udp-proxy/internal/qos"
	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration structure.
type Config struct {
	Global    GlobalConfig     `yaml:"global"`
	Frontends []FrontendConfig `yaml:"frontends"`
	Backends  []BackendConfig  `yaml:"backends"`
}

// GlobalConfig contains global proxy settings.
type GlobalConfig struct {
	MaxPacketSize          int                   `yaml:"max_packet_size"`
	WorkerThreads          int                   `yaml:"worker_threads"`
	SessionTimeout         time.Duration         `yaml:"session_timeout"`
	SessionCleanupInterval time.Duration         `yaml:"session_cleanup_interval"`
	LogLevel               string                `yaml:"log_level"`
	MetricsAddr            string                `yaml:"metrics_addr"`
	Socket                 SocketConfig          `yaml:"socket"`
	TrafficShaping         TrafficShapingConfig  `yaml:"traffic_shaping"`
	ClientLimits           ClientLimitsConfig    `yaml:"client_limits"`
	AbuseProtection        AbuseProtectionConfig `yaml:"abuse_protection"`
	Fragmentation          FragmentationConfig   `yaml:"fragmentation"`
}

// SocketConfig holds kernel-level socket tuning options.
type SocketConfig struct {
	// RcvBuf sets SO_RCVBUF (kernel receive buffer) in bytes. 0 = OS default.
	RcvBuf int `yaml:"rcvbuf"`
	// SndBuf sets SO_SNDBUF (kernel send buffer) in bytes. 0 = OS default.
	SndBuf int `yaml:"sndbuf"`
	// ReusePort enables SO_REUSEPORT, allowing multiple goroutines to bind the
	// same port and letting the kernel distribute packets across them.
	ReusePort bool `yaml:"reuse_port"`
}

// FrontendConfig defines a listening frontend.
type FrontendConfig struct {
	Name            string               `yaml:"name"`
	Listen          string               `yaml:"listen"`
	Backend         string               `yaml:"backend"`
	SessionAffinity bool                 `yaml:"session_affinity"`
	MaxSessions     int                  `yaml:"max_sessions"`    // max concurrent sessions, 0 = unlimited
	MaxPacketSize   int                  `yaml:"max_packet_size"` // override global, 0 = use global
	Priority        string               `yaml:"priority"`
	TrafficShaping  TrafficShapingConfig `yaml:"traffic_shaping"`
}

// BackendConfig defines a pool of servers.
type BackendConfig struct {
	Name           string               `yaml:"name"`
	LoadBalance    string               `yaml:"load_balance"`
	HealthCheck    HealthCheckConfig    `yaml:"health_check"`
	Servers        []ServerConfig       `yaml:"servers"`
	TrafficShaping TrafficShapingConfig `yaml:"traffic_shaping"`
}

// ByteSize stores a size in bytes and supports YAML values like "200MB".
type ByteSize int64

// UnmarshalYAML parses byte-size values from YAML.
func (b *ByteSize) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		if value.Tag == "!!int" {
			n, err := strconv.ParseInt(value.Value, 10, 64)
			if err != nil {
				return fmt.Errorf("invalid byte size %q: %w", value.Value, err)
			}
			*b = ByteSize(n)
			return nil
		}
		n, err := parseByteSize(value.Value)
		if err != nil {
			return err
		}
		*b = ByteSize(n)
		return nil
	default:
		return fmt.Errorf("invalid byte size value %q", value.Value)
	}
}

func parseByteSize(value string) (int64, error) {
	s := strings.TrimSpace(value)
	if s == "" {
		return 0, nil
	}
	end := len(s)
	for end > 0 && ((s[end-1] >= 'A' && s[end-1] <= 'Z') || (s[end-1] >= 'a' && s[end-1] <= 'z')) {
		end--
	}
	numPart := strings.TrimSpace(s[:end])
	unitPart := strings.ToUpper(strings.TrimSpace(s[end:]))
	n, err := strconv.ParseInt(numPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid byte size %q: %w", value, err)
	}
	switch unitPart {
	case "", "B":
		return n, nil
	case "KB":
		return n * 1000, nil
	case "MB":
		return n * 1000 * 1000, nil
	case "GB":
		return n * 1000 * 1000 * 1000, nil
	case "TB":
		return n * 1000 * 1000 * 1000 * 1000, nil
	case "KIB":
		return n * 1024, nil
	case "MIB":
		return n * 1024 * 1024, nil
	case "GIB":
		return n * 1024 * 1024 * 1024, nil
	case "TIB":
		return n * 1024 * 1024 * 1024 * 1024, nil
	default:
		return 0, fmt.Errorf("unsupported byte size unit %q", unitPart)
	}
}

// TrafficShapingConfig configures token-bucket shaping.
type TrafficShapingConfig struct {
	Enabled          bool     `yaml:"enabled"`
	PacketsPerSecond int64    `yaml:"packets_per_second"`
	BytesPerSecond   ByteSize `yaml:"bytes_per_second"`
	BurstPackets     int64    `yaml:"burst_packets"`
	BurstBytes       ByteSize `yaml:"burst_bytes"`
}

// ClientLimitsConfig configures per-client shaping.
type ClientLimitsConfig struct {
	PacketsPerSecond int64 `yaml:"packets_per_second"`
	BurstPackets     int64 `yaml:"burst_packets"`
}

// AbuseProtectionConfig configures temporary per-IP abuse limits.
type AbuseProtectionConfig struct {
	Enabled                  bool  `yaml:"enabled"`
	MaxPacketsPerSecondPerIP int64 `yaml:"max_packets_per_second_per_ip"`
	MaxSessionsPerIP         int   `yaml:"max_sessions_per_ip"`
}

// FragmentationConfig configures packet fragment handling.
type FragmentationConfig struct {
	DropFragments bool `yaml:"drop_fragments"`
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
	if c.Global.SessionTimeout <= 0 {
		c.Global.SessionTimeout = 60 * time.Second
	}
	if c.Global.SessionCleanupInterval <= 0 {
		c.Global.SessionCleanupInterval = 10 * time.Second
	}
	if c.Global.LogLevel == "" {
		c.Global.LogLevel = "info"
	}
	if c.Global.MetricsAddr == "" {
		c.Global.MetricsAddr = "0.0.0.0:9090"
	}
	for i := range c.Frontends {
		if c.Frontends[i].Priority == "" {
			c.Frontends[i].Priority = qos.PriorityNormal.String()
		}
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
		if _, dup := backendNames[b.Name]; dup {
			return fmt.Errorf("duplicate backend name %q", b.Name)
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
		if _, err := qos.ParsePriority(f.Priority); err != nil {
			return fmt.Errorf("frontend %q: %w", f.Name, err)
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
