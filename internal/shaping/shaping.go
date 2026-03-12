package shaping

import (
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// Limits defines token-bucket rate and burst settings.
type Limits struct {
	Enabled          bool
	PacketsPerSecond int64
	BytesPerSecond   int64
	BurstPackets     int64
	BurstBytes       int64
}

// Normalize ensures default burst values and enabled behavior are consistent.
func (l Limits) Normalize() Limits {
	if l.BurstPackets <= 0 && l.PacketsPerSecond > 0 {
		l.BurstPackets = l.PacketsPerSecond
	}
	if l.BurstBytes <= 0 && l.BytesPerSecond > 0 {
		l.BurstBytes = l.BytesPerSecond
	}
	if l.PacketsPerSecond > 0 || l.BytesPerSecond > 0 {
		l.Enabled = true
	}
	return l
}

// Bucket is a token bucket limiter.
type Bucket struct {
	limits     Limits
	mu         sync.Mutex
	lastRefill time.Time
	packetTok  float64
	byteTok    float64
}

// NewBucket creates a token bucket; nil is returned when shaping is disabled.
func NewBucket(l Limits) *Bucket {
	l = l.Normalize()
	if !l.Enabled {
		return nil
	}
	b := &Bucket{limits: l, lastRefill: time.Now()}
	if l.PacketsPerSecond > 0 {
		b.packetTok = float64(l.BurstPackets)
	}
	if l.BytesPerSecond > 0 {
		b.byteTok = float64(l.BurstBytes)
	}
	return b
}

// Allow checks whether a packet of size n bytes can pass.
func (b *Bucket) Allow(n int, now time.Time) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed > 0 {
		if b.limits.PacketsPerSecond > 0 {
			b.packetTok += elapsed * float64(b.limits.PacketsPerSecond)
			if b.packetTok > float64(b.limits.BurstPackets) {
				b.packetTok = float64(b.limits.BurstPackets)
			}
		}
		if b.limits.BytesPerSecond > 0 {
			b.byteTok += elapsed * float64(b.limits.BytesPerSecond)
			if b.byteTok > float64(b.limits.BurstBytes) {
				b.byteTok = float64(b.limits.BurstBytes)
			}
		}
		b.lastRefill = now
	}

	if b.limits.PacketsPerSecond > 0 && b.packetTok < 1 {
		return false
	}
	if b.limits.BytesPerSecond > 0 && b.byteTok < float64(n) {
		return false
	}
	if b.limits.PacketsPerSecond > 0 {
		b.packetTok -= 1
	}
	if b.limits.BytesPerSecond > 0 {
		b.byteTok -= float64(n)
	}
	return true
}

type clientEntry struct {
	bucket   *Bucket
	lastSeen int64
}

type clientShard struct {
	mu        sync.Mutex
	entries   map[netip.Addr]*clientEntry
	lastSweep int64
}

// ClientLimiter applies token buckets per source IP.
type ClientLimiter struct {
	ttl       time.Duration
	shards    []clientShard
	enabled   bool
	limitBase Limits
}

// NewClientLimiter creates a per-client limiter.
func NewClientLimiter(l Limits, ttl time.Duration) *ClientLimiter {
	l = l.Normalize()
	if !l.Enabled {
		return nil
	}
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	cl := &ClientLimiter{
		ttl:       ttl,
		shards:    make([]clientShard, 64),
		enabled:   true,
		limitBase: l,
	}
	for i := range cl.shards {
		cl.shards[i].entries = make(map[netip.Addr]*clientEntry)
	}
	return cl
}

// Allow checks whether the client can send this packet.
func (cl *ClientLimiter) Allow(addr netip.Addr, n int, now time.Time) bool {
	if cl == nil || !cl.enabled {
		return true
	}
	idx := int(addr.As16()[14]^addr.As16()[15]) % len(cl.shards)
	sh := &cl.shards[idx]
	nowUnix := now.Unix()

	sh.mu.Lock()
	e := sh.entries[addr]
	if e == nil {
		e = &clientEntry{bucket: NewBucket(cl.limitBase)}
		sh.entries[addr] = e
	}
	atomic.StoreInt64(&e.lastSeen, nowUnix)
	if nowUnix-sh.lastSweep >= int64(cl.ttl.Seconds()) {
		cutoff := now.Add(-cl.ttl).Unix()
		for ip, entry := range sh.entries {
			if atomic.LoadInt64(&entry.lastSeen) < cutoff {
				delete(sh.entries, ip)
			}
		}
		sh.lastSweep = nowUnix
	}
	b := e.bucket
	sh.mu.Unlock()

	return b.Allow(n, now)
}
