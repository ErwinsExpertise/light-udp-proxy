package abuse

import (
	"net/netip"
	"testing"
	"time"
)

func TestProtectorPacketRate(t *testing.T) {
	p := New(Config{
		Enabled:             true,
		MaxPacketsPerSecond: 1,
		SessionTTL:          2 * time.Second,
	})
	client := netip.MustParseAddrPort("192.0.2.1:5000")
	now := time.Now()
	if !p.Allow(client, now) {
		t.Fatal("first packet should pass")
	}
	if p.Allow(client, now) {
		t.Fatal("second packet in same second should be denied")
	}
	if !p.Allow(client, now.Add(time.Second)) {
		t.Fatal("next second packet should pass")
	}
}

func TestProtectorSessionLimit(t *testing.T) {
	p := New(Config{
		Enabled:              true,
		MaxSessionsPerClient: 2,
		SessionTTL:           2 * time.Second,
	})
	now := time.Now()
	if !p.Allow(netip.MustParseAddrPort("198.51.100.9:1000"), now) {
		t.Fatal("session 1 should pass")
	}
	if !p.Allow(netip.MustParseAddrPort("198.51.100.9:1001"), now) {
		t.Fatal("session 2 should pass")
	}
	if p.Allow(netip.MustParseAddrPort("198.51.100.9:1002"), now) {
		t.Fatal("session 3 should be denied")
	}
}
