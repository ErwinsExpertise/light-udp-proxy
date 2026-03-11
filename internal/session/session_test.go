package session

import (
"net/netip"
"sync"
"testing"
"time"
)

func makeKey(addr, frontend string) Key {
ap := netip.MustParseAddrPort(addr)
return Key{Addr: ap, FrontendName: frontend}
}

func TestGetOrCreate(t *testing.T) {
tbl := NewTable(5*time.Second, time.Second)
defer tbl.Stop()
key := makeKey("127.0.0.1:12345", "fe1")

s1, created := tbl.GetOrCreate(key, func() *Session {
return NewSession("10.0.0.1:5000")
})
if !created {
t.Fatal("expected session to be created")
}
if s1.BackendAddr() != "10.0.0.1:5000" {
t.Errorf("BackendAddr = %q, want 10.0.0.1:5000", s1.BackendAddr())
}

// Second call should return existing session.
s2, created := tbl.GetOrCreate(key, func() *Session {
return NewSession("10.0.0.2:5000")
})
if created {
t.Fatal("expected existing session to be returned")
}
if s2.BackendAddr() != "10.0.0.1:5000" {
t.Errorf("BackendAddr = %q, want 10.0.0.1:5000 (should reuse existing)", s2.BackendAddr())
}
}

func TestGet(t *testing.T) {
tbl := NewTable(5*time.Second, time.Second)
defer tbl.Stop()
key := makeKey("127.0.0.1:9999", "fe")

if got := tbl.Get(key); got != nil {
t.Fatal("expected nil for missing session")
}

tbl.GetOrCreate(key, func() *Session { return NewSession("backend:80") })
if got := tbl.Get(key); got == nil {
t.Fatal("expected session to be found")
}
}

func TestDelete(t *testing.T) {
tbl := NewTable(5*time.Second, time.Second)
defer tbl.Stop()
key := makeKey("127.0.0.1:1111", "fe")
tbl.GetOrCreate(key, func() *Session { return NewSession("b:80") })
if tbl.Len() != 1 {
t.Fatalf("Len = %d, want 1", tbl.Len())
}
tbl.Delete(key)
if tbl.Len() != 0 {
t.Fatalf("Len = %d after delete, want 0", tbl.Len())
}
}

func TestDeleteCallsEvict(t *testing.T) {
tbl := NewTable(5*time.Second, time.Second)
defer tbl.Stop()
key := makeKey("127.0.0.1:4444", "fe")
var called bool
sess := NewSession("b:80")
sess.UpdateServer("b:80", func() { called = true })
tbl.GetOrCreate(key, func() *Session { return sess })
tbl.Delete(key)
if !called {
t.Error("evict callback not called on Delete")
}
}

func TestExpiry(t *testing.T) {
// Build a table without starting the background reaper so we can call
// evictExpired directly and keep the test deterministic.
tbl := &Table{
timeout:         100 * time.Millisecond,
cleanupInterval: time.Second,
stopCh:          make(chan struct{}),
}
for i := range tbl.shards {
tbl.shards[i].entries = make(map[Key]*Session)
}

key := makeKey("127.0.0.1:2222", "fe")
idx := shardFor(key)
sess := NewSession("b:80")
sess.LastSeen = time.Now().Add(-200 * time.Millisecond)
tbl.shards[idx].entries[key] = sess
tbl.count.Store(1)

tbl.evictExpired()
// evictExpired fires callEvict in a goroutine; give it a moment.
time.Sleep(10 * time.Millisecond)

if tbl.Len() != 0 {
t.Fatalf("Len = %d after expiry eviction, want 0", tbl.Len())
}
}

func TestExpiryCallsEvict(t *testing.T) {
tbl := &Table{
timeout:         100 * time.Millisecond,
cleanupInterval: time.Second,
stopCh:          make(chan struct{}),
}
for i := range tbl.shards {
tbl.shards[i].entries = make(map[Key]*Session)
}

var called bool
var mu sync.Mutex
key := makeKey("127.0.0.1:5555", "fe")
idx := shardFor(key)
sess := NewSession("b:80")
sess.LastSeen = time.Now().Add(-200 * time.Millisecond)
sess.UpdateServer("b:80", func() {
mu.Lock()
called = true
mu.Unlock()
})
tbl.shards[idx].entries[key] = sess
tbl.count.Store(1)

tbl.evictExpired()
time.Sleep(50 * time.Millisecond) // wait for the goroutine

mu.Lock()
c := called
mu.Unlock()
if !c {
t.Error("evict callback not called after expiry")
}
}

func TestTouch(t *testing.T) {
s := NewSession("b:80")
s.LastSeen = time.Now().Add(-time.Second)
before := s.LastSeen
s.Touch(100)
if !s.LastSeen.After(before) {
t.Error("Touch did not update LastSeen")
}
if s.PacketCount != 1 {
t.Errorf("PacketCount = %d after first Touch, want 1", s.PacketCount)
}
if s.ByteCount != 100 {
t.Errorf("ByteCount = %d, want 100", s.ByteCount)
}
}

func TestPacketCountNotDoubled(t *testing.T) {
// GetOrCreate initialises PacketCount=0; the first Touch call must set it to 1.
tbl := NewTable(5*time.Second, time.Second)
defer tbl.Stop()
key := makeKey("127.0.0.1:7777", "fe")

sess, created := tbl.GetOrCreate(key, func() *Session { return NewSession("b:80") })
if !created {
t.Fatal("expected new session")
}
if sess.PacketCount != 0 {
t.Errorf("PacketCount after GetOrCreate = %d, want 0", sess.PacketCount)
}
sess.Touch(50)
if sess.PacketCount != 1 {
t.Errorf("PacketCount after first Touch = %d, want 1", sess.PacketCount)
}
}

func TestUpdateServer(t *testing.T) {
s := NewSession("old:80")
if s.BackendAddr() != "old:80" {
t.Fatalf("BackendAddr = %q, want old:80", s.BackendAddr())
}
var evictCalled bool
s.UpdateServer("new:80", func() { evictCalled = true })
if s.BackendAddr() != "new:80" {
t.Fatalf("BackendAddr = %q after UpdateServer, want new:80", s.BackendAddr())
}
s.callEvict()
if !evictCalled {
t.Error("evict callback not called")
}
}

func TestStop(t *testing.T) {
tbl := NewTable(time.Minute, 10*time.Second)
tbl.Stop()
tbl.Stop() // second call must be safe (idempotent)
}

func TestAtomicCount(t *testing.T) {
tbl := NewTable(time.Minute, 10*time.Second)
defer tbl.Stop()

keys := []Key{
makeKey("127.0.0.1:1001", "fe"),
makeKey("127.0.0.1:1002", "fe"),
makeKey("127.0.0.1:1003", "fe"),
}
for _, k := range keys {
tbl.GetOrCreate(k, func() *Session { return NewSession("b:80") })
}
if tbl.Count() != 3 {
t.Errorf("Count = %d, want 3", tbl.Count())
}
tbl.Delete(keys[0])
if tbl.Count() != 2 {
t.Errorf("Count = %d after delete, want 2", tbl.Count())
}
}

func TestSharding(t *testing.T) {
tbl := NewTable(time.Minute, 10*time.Second)
defer tbl.Stop()
keys := make([]Key, 1000)
for i := range keys {
port := uint16(10000 + i)
ap := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port)
keys[i] = Key{Addr: ap, FrontendName: "fe"}
tbl.GetOrCreate(keys[i], func() *Session { return NewSession("b:80") })
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
