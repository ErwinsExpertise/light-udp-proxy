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

// Session holds state for a single client session.
type Session struct {
mu          sync.Mutex
backendAddr string
onEvict     func() // called when session is evicted or deleted
LastSeen    time.Time
PacketCount int64
ByteCount   int64
}

// NewSession creates a Session targeting backendAddr.
func NewSession(backendAddr string) *Session {
return &Session{backendAddr: backendAddr}
}

// BackendAddr returns the current backend address under the session mutex.
func (s *Session) BackendAddr() string {
s.mu.Lock()
defer s.mu.Unlock()
return s.backendAddr
}

// UpdateServer atomically updates the backend address and the eviction callback.
func (s *Session) UpdateServer(addr string, onEvict func()) {
s.mu.Lock()
s.backendAddr = addr
s.onEvict = onEvict
s.mu.Unlock()
}

// Touch updates the last-seen timestamp and packet/byte counters.
func (s *Session) Touch(bytes int64) {
s.mu.Lock()
s.LastSeen = time.Now()
s.PacketCount++
s.ByteCount += bytes
s.mu.Unlock()
}

// callEvict invokes the eviction callback if one is registered. Must be called
// without holding s.mu.
func (s *Session) callEvict() {
s.mu.Lock()
fn := s.onEvict
s.mu.Unlock()
if fn != nil {
fn()
}
}

// shard is a single stripe of the session table.
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
// Hash frontend name bytes without converting to []byte.
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

// Produce the new session outside any lock.
newSess := newFn()
if newSess == nil {
return nil, false
}
newSess.LastSeen = time.Now()
	newSess.LastSeen = time.Now()
	// PacketCount starts at its zero value; Touch() counts the first packet,
	// avoiding a double-count.

s.mu.Lock()
// Double-checked locking: another goroutine may have inserted while we held no lock.
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
sess.mu.Lock()
expired := sess.LastSeen.Before(cutoff)
sess.mu.Unlock()
if expired {
delete(s.entries, k)
t.count.Add(-1)
// Call eviction hook outside the shard lock to avoid deadlocks.
go sess.callEvict()
}
}
s.mu.Unlock()
}
}
