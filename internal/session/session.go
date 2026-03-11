// Package session implements a high-performance, sharded UDP session table.
package session

import (
"net/netip"
"sync"
"sync/atomic"
"time"
)

// numShards is the number of independent map shards. Must be a power of 2.
const numShards = 256

// Key uniquely identifies a client session per frontend.
// Using netip.AddrPort avoids heap allocations for the client address.
type Key struct {
Addr         netip.AddrPort
FrontendName string
}

// serverBinding bundles the backend address and eviction callback into a single
// value that can be replaced atomically. The struct is immutable once stored;
// swapping the pointer in Session.binding is the only mutation.
type serverBinding struct {
addr    string
onEvict func()
}

// Session holds state for a single client session.
// All fields are accessed via lock-free atomic operations; no mutex is needed.
type Session struct {
// binding stores the current backend address and eviction callback as a
// single atomic pointer to an immutable serverBinding struct. Replacing
// both fields is a single pointer swap, which is safe and wait-free.
binding atomic.Pointer[serverBinding]

// LastSeen is the Unix nanosecond timestamp of the most recent packet.
// Stored atomically so the reaper can read it without acquiring any lock.
LastSeen atomic.Int64

// PacketCount and ByteCount are updated with atomic.Add on every packet.
PacketCount atomic.Int64
ByteCount   atomic.Int64
}

// NewSession creates a Session targeting backendAddr.
func NewSession(backendAddr string) *Session {
s := &Session{}
s.binding.Store(&serverBinding{addr: backendAddr})
s.LastSeen.Store(time.Now().UnixNano())
return s
}

// BackendAddr returns the current backend address with a single atomic load.
func (s *Session) BackendAddr() string {
if b := s.binding.Load(); b != nil {
return b.addr
}
return ""
}

// UpdateServer atomically replaces the backend address and eviction callback.
// The swap is a single pointer store — no lock, no blocking.
func (s *Session) UpdateServer(addr string, onEvict func()) {
s.binding.Store(&serverBinding{addr: addr, onEvict: onEvict})
}

// Touch records the current time and updates packet/byte counters.
// All three updates are independent atomic operations; no lock is held.
func (s *Session) Touch(bytes int64) {
s.LastSeen.Store(time.Now().UnixNano())
s.PacketCount.Add(1)
s.ByteCount.Add(bytes)
}

// callEvict invokes the eviction callback registered via UpdateServer.
func (s *Session) callEvict() {
if b := s.binding.Load(); b != nil && b.onEvict != nil {
b.onEvict()
}
}

// LastSeenTime returns the last-seen timestamp as a time.Time.
func (s *Session) LastSeenTime() time.Time {
return time.Unix(0, s.LastSeen.Load())
}

// shard is a single stripe of the session table.
// The RWMutex is required to protect the Go map; with 256 shards contention
// is negligible. Everything inside the Session values is lock-free.
type shard struct {
mu      sync.RWMutex
entries map[Key]*Session
}

// Table is a sharded, concurrent map of active UDP sessions.
type Table struct {
shards          [numShards]shard
timeout         time.Duration
cleanupInterval time.Duration
count           atomic.Int64
stopCh          chan struct{}
stopOnce        sync.Once
}

// NewTable creates a Table with the given session timeout and cleanup interval.
func NewTable(timeout, cleanupInterval time.Duration) *Table {
t := &Table{
timeout:         timeout,
cleanupInterval: cleanupInterval,
stopCh:          make(chan struct{}),
}
for i := range t.shards {
t.shards[i].entries = make(map[Key]*Session)
}
go t.reaper()
return t
}

// Stop terminates the background reaper goroutine. Safe to call multiple times.
func (t *Table) Stop() {
t.stopOnce.Do(func() { close(t.stopCh) })
}

// shardFor returns the shard index for a key using an allocation-free inline
// FNV-1a hash over the address, port, and frontend name bytes.
func shardFor(key Key) uint32 {
const (
offset32 = 2166136261
prime32  = 16777619
)
h := uint32(offset32)
// Hash the 16-byte IP representation.
ab := key.Addr.Addr().As16()
for _, b := range ab {
h ^= uint32(b)
h *= prime32
}
// Hash the 2-byte port.
port := key.Addr.Port()
h ^= uint32(port >> 8)
h *= prime32
h ^= uint32(port & 0xff)
h *= prime32
// Hash frontend name bytes without allocating a []byte.
for i := 0; i < len(key.FrontendName); i++ {
h ^= uint32(key.FrontendName[i])
h *= prime32
}
return h % numShards
}

// Get returns the existing session for key, or nil if not found.
// It does NOT update the last-seen time; call Touch explicitly when needed.
func (t *Table) Get(key Key) *Session {
s := &t.shards[shardFor(key)]
s.mu.RLock()
sess := s.entries[key]
s.mu.RUnlock()
return sess
}

// GetOrCreate returns the existing session or creates a new one using newFn.
// newFn is called without any locks held.
// Returns (session, true) when a new session was created.
func (t *Table) GetOrCreate(key Key, newFn func() *Session) (*Session, bool) {
idx := shardFor(key)
s := &t.shards[idx]

s.mu.RLock()
sess := s.entries[key]
s.mu.RUnlock()
if sess != nil {
return sess, false
}

// Produce the new session outside any lock. NewSession sets LastSeen.
newSess := newFn()
if newSess == nil {
return nil, false
}

s.mu.Lock()
// Double-checked locking: another goroutine may have inserted while we
// held no lock.
if existing, ok := s.entries[key]; ok {
s.mu.Unlock()
return existing, false
}
s.entries[key] = newSess
s.mu.Unlock()

t.count.Add(1)
return newSess, true
}

// Delete removes the session for key, calling its eviction callback if set.
func (t *Table) Delete(key Key) {
s := &t.shards[shardFor(key)]
s.mu.Lock()
sess, ok := s.entries[key]
if ok {
delete(s.entries, key)
}
s.mu.Unlock()
if ok {
t.count.Add(-1)
sess.callEvict()
}
}

// Len returns the number of active sessions in O(1) via an atomic counter.
func (t *Table) Len() int {
return int(t.count.Load())
}

// Count returns the number of active sessions as int64.
func (t *Table) Count() int64 {
return t.count.Load()
}

// reaper runs at cleanupInterval and evicts expired sessions.
func (t *Table) reaper() {
ticker := time.NewTicker(t.cleanupInterval)
defer ticker.Stop()
for {
select {
case <-t.stopCh:
return
case <-ticker.C:
t.evictExpired()
}
}
}

func (t *Table) evictExpired() {
cutoff := time.Now().Add(-t.timeout)
for i := range t.shards {
s := &t.shards[i]
s.mu.Lock()
for k, sess := range s.entries {
// LastSeen is an atomic.Int64 — read it without a per-session lock.
if sess.LastSeenTime().Before(cutoff) {
delete(s.entries, k)
t.count.Add(-1)
// Run the eviction callback outside the shard lock.
go sess.callEvict()
}
}
s.mu.Unlock()
}
}

