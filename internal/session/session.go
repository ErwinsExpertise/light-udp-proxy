// Package session implements a high-performance, sharded UDP session table.
package session

import (
"hash/fnv"
"net/netip"
"sync"
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
BackendAddr string
LastSeen    time.Time
PacketCount int64
ByteCount   int64
}

// Touch updates the last-seen timestamp and packet/byte counters.
func (s *Session) Touch(bytes int64) {
s.mu.Lock()
s.LastSeen = time.Now()
s.PacketCount++
s.ByteCount += bytes
s.mu.Unlock()
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
}

// NewTable creates a Table with the given session timeout and cleanup interval.
func NewTable(timeout, cleanupInterval time.Duration) *Table {
t := &Table{timeout: timeout, cleanupInterval: cleanupInterval}
for i := range t.shards {
t.shards[i].entries = make(map[Key]*Session)
}
go t.reaper()
return t
}

// shardFor returns the shard index for a key using FNV-1a.
func shardFor(key Key) uint32 {
h := fnv.New32a()
ab := key.Addr.Addr().As16()
h.Write(ab[:])
port := key.Addr.Port()
h.Write([]byte{byte(port >> 8), byte(port)})
h.Write([]byte(key.FrontendName))
return h.Sum32() % numShards
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
newSess.PacketCount = 1

s.mu.Lock()
// Check again after acquiring write lock (double-checked locking).
if existing, ok := s.entries[key]; ok {
s.mu.Unlock()
return existing, false
}
s.entries[key] = newSess
s.mu.Unlock()
return newSess, true
}

// Delete removes the session for key.
func (t *Table) Delete(key Key) {
s := &t.shards[shardFor(key)]
s.mu.Lock()
delete(s.entries, key)
s.mu.Unlock()
}

// Len returns the total number of active sessions across all shards.
func (t *Table) Len() int {
total := 0
for i := range t.shards {
t.shards[i].mu.RLock()
total += len(t.shards[i].entries)
t.shards[i].mu.RUnlock()
}
return total
}

// reaper runs at cleanupInterval and evicts expired sessions.
func (t *Table) reaper() {
ticker := time.NewTicker(t.cleanupInterval)
defer ticker.Stop()
for range ticker.C {
t.evictExpired()
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
}
}
s.mu.Unlock()
}
}
