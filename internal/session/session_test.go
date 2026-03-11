package session

import (
"net/netip"
"testing"
"time"
)

func makeKey(addr, frontend string) Key {
ap := netip.MustParseAddrPort(addr)
return Key{Addr: ap, FrontendName: frontend}
}

func TestGetOrCreate(t *testing.T) {
tbl := NewTable(5*time.Second, time.Second)
key := makeKey("127.0.0.1:12345", "fe1")

s1, created := tbl.GetOrCreate(key, func() *Session {
return &Session{BackendAddr: "10.0.0.1:5000"}
})
if !created {
t.Fatal("expected session to be created")
}
if s1.BackendAddr != "10.0.0.1:5000" {
t.Errorf("BackendAddr = %q, want 10.0.0.1:5000", s1.BackendAddr)
}

// Second call should return existing session.
s2, created := tbl.GetOrCreate(key, func() *Session {
return &Session{BackendAddr: "10.0.0.2:5000"}
})
if created {
t.Fatal("expected existing session to be returned")
}
if s2.BackendAddr != "10.0.0.1:5000" {
t.Errorf("BackendAddr = %q, want 10.0.0.1:5000 (should reuse existing)", s2.BackendAddr)
}
}

func TestGet(t *testing.T) {
tbl := NewTable(5*time.Second, time.Second)
key := makeKey("127.0.0.1:9999", "fe")

if got := tbl.Get(key); got != nil {
t.Fatal("expected nil for missing session")
}

tbl.GetOrCreate(key, func() *Session {
return &Session{BackendAddr: "backend:80"}
})
if got := tbl.Get(key); got == nil {
t.Fatal("expected session to be found")
}
}

func TestDelete(t *testing.T) {
tbl := NewTable(5*time.Second, time.Second)
key := makeKey("127.0.0.1:1111", "fe")
tbl.GetOrCreate(key, func() *Session { return &Session{BackendAddr: "b:80"} })
if tbl.Len() != 1 {
t.Fatalf("Len = %d, want 1", tbl.Len())
}
tbl.Delete(key)
if tbl.Len() != 0 {
t.Fatalf("Len = %d after delete, want 0", tbl.Len())
}
}

func TestExpiry(t *testing.T) {
tbl := &Table{
timeout:         100 * time.Millisecond,
cleanupInterval: time.Second,
}
for i := range tbl.shards {
tbl.shards[i].entries = make(map[Key]*Session)
}

key := makeKey("127.0.0.1:2222", "fe")
idx := shardFor(key)
sess := &Session{BackendAddr: "b:80", LastSeen: time.Now().Add(-200 * time.Millisecond)}
tbl.shards[idx].entries[key] = sess

tbl.evictExpired()

if tbl.Len() != 0 {
t.Fatalf("Len = %d after expiry eviction, want 0", tbl.Len())
}
}

func TestTouch(t *testing.T) {
s := &Session{BackendAddr: "b:80", LastSeen: time.Now().Add(-time.Second)}
before := s.LastSeen
s.Touch(100)
if !s.LastSeen.After(before) {
t.Error("Touch did not update LastSeen")
}
if s.PacketCount != 1 {
t.Errorf("PacketCount = %d, want 1", s.PacketCount)
}
if s.ByteCount != 100 {
t.Errorf("ByteCount = %d, want 100", s.ByteCount)
}
}

func TestSharding(t *testing.T) {
tbl := NewTable(time.Minute, 10*time.Second)
// Insert 1000 sessions; verify all can be retrieved.
keys := make([]Key, 1000)
for i := range keys {
port := uint16(10000 + i)
ap := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port)
keys[i] = Key{Addr: ap, FrontendName: "fe"}
tbl.GetOrCreate(keys[i], func() *Session { return &Session{BackendAddr: "b:80"} })
}
if tbl.Len() != 1000 {
t.Errorf("Len = %d, want 1000", tbl.Len())
}
for _, k := range keys {
if tbl.Get(k) == nil {
t.Fatalf("session missing for %v", k)
}
}
}
