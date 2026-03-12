package abuse

import (
	"net/netip"
	"sync"
	"time"
)

// Config defines lightweight abuse-protection thresholds.
type Config struct {
	Enabled              bool
	MaxPacketsPerSecond  int64
	MaxSessionsPerClient int
	SessionTTL           time.Duration
}

type entry struct {
	windowSecond int64
	packets      int64
	lastSeen     int64
	lastSweep    int64
	sessions     map[uint16]int64
}

type shard struct {
	mu      sync.Mutex
	entries map[netip.Addr]*entry
}

// Protector tracks per-IP traffic and active sessions.
type Protector struct {
	cfg    Config
	shards []shard
}

// New creates a new abuse protector.
func New(cfg Config) *Protector {
	if !cfg.Enabled {
		return nil
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 60 * time.Second
	}
	p := &Protector{
		cfg:    cfg,
		shards: make([]shard, 64),
	}
	for i := range p.shards {
		p.shards[i].entries = make(map[netip.Addr]*entry)
	}
	return p
}

// Allow evaluates per-IP packet rate and active-session limits.
func (p *Protector) Allow(addr netip.AddrPort, now time.Time) bool {
	if p == nil || !p.cfg.Enabled {
		return true
	}
	ip := addr.Addr()
	idx := int(ip.As16()[14]^ip.As16()[15]) % len(p.shards)
	sh := &p.shards[idx]
	nowSec := now.Unix()

	sh.mu.Lock()
	defer sh.mu.Unlock()

	e := sh.entries[ip]
	if e == nil {
		e = &entry{
			windowSecond: nowSec,
			sessions:     make(map[uint16]int64),
		}
		sh.entries[ip] = e
	}
	e.lastSeen = nowSec
	if p.cfg.MaxPacketsPerSecond > 0 {
		if e.windowSecond != nowSec {
			e.windowSecond = nowSec
			e.packets = 0
		}
		e.packets++
		if e.packets > p.cfg.MaxPacketsPerSecond {
			p.sweepLocked(sh, now)
			return false
		}
	}

	if p.cfg.MaxSessionsPerClient > 0 {
		port := addr.Port()
		e.sessions[port] = nowSec
		if nowSec-e.lastSweep >= int64(p.cfg.SessionTTL.Seconds()) {
			p.sweepEntrySessionsLocked(e, now)
			e.lastSweep = nowSec
		}
		if len(e.sessions) > p.cfg.MaxSessionsPerClient {
			p.sweepLocked(sh, now)
			return false
		}
	}

	p.sweepLocked(sh, now)
	return true
}

func (p *Protector) sweepEntrySessionsLocked(e *entry, now time.Time) {
	cutoff := now.Add(-p.cfg.SessionTTL).Unix()
	for port, ts := range e.sessions {
		if ts < cutoff {
			delete(e.sessions, port)
		}
	}
}

func (p *Protector) sweepLocked(sh *shard, now time.Time) {
	cutoff := now.Add(-p.cfg.SessionTTL).Unix()
	for ip, e := range sh.entries {
		if e.lastSeen < cutoff {
			delete(sh.entries, ip)
			continue
		}
		p.sweepEntrySessionsLocked(e, now)
	}
}
