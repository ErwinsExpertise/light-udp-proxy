package shaping

import (
	"net/netip"
	"testing"
	"time"
)

func TestBucketAllowAndRefill(t *testing.T) {
	b := NewBucket(Limits{
		Enabled:          true,
		PacketsPerSecond: 2,
		BurstPackets:     2,
	})
	now := time.Now()
	if !b.Allow(100, now) || !b.Allow(100, now) {
		t.Fatal("expected initial burst packets to pass")
	}
	if b.Allow(100, now) {
		t.Fatal("expected packet to be denied after burst exhaustion")
	}
	if !b.Allow(100, now.Add(600*time.Millisecond)) {
		t.Fatal("expected refill to allow packet")
	}
}

func TestClientLimiter(t *testing.T) {
	cl := NewClientLimiter(Limits{
		PacketsPerSecond: 1,
		BurstPackets:     1,
	}, 2*time.Second)
	ip := netip.MustParseAddr("192.0.2.10")
	now := time.Now()
	if !cl.Allow(ip, 64, now) {
		t.Fatal("first packet should pass")
	}
	if cl.Allow(ip, 64, now) {
		t.Fatal("second packet in same instant should be limited")
	}
}
