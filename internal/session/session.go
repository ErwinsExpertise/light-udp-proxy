// Package session implements UDP session tracking for the proxy.
package session

import (
	"net"
	"sync"
	"time"
)

// Key uniquely identifies a client session per frontend.
type Key struct {
	ClientAddr   string
	FrontendName string
}

// Session holds state for a single client session.
type Session struct {
	mu          sync.Mutex
	ClientAddr  *net.UDPAddr
	BackendAddr string // selected backend server address
	LastSeen    time.Time
	PacketCount int64
}

func (s *Session) touch() {
	s.mu.Lock()
	s.LastSeen = time.Now()
	s.PacketCount++
	s.mu.Unlock()
}

// Table is a concurrent map of active UDP sessions.
type Table struct {
	mu      sync.RWMutex
	entries map[Key]*Session
	timeout time.Duration
}

// NewTable creates a Table with the given session timeout.
func NewTable(timeout time.Duration) *Table {
	t := &Table{
		entries: make(map[Key]*Session),
		timeout: timeout,
	}
	go t.reaper()
	return t
}

// Get returns the existing session for key, or nil if not found.
func (t *Table) Get(key Key) *Session {
	t.mu.RLock()
	s := t.entries[key]
	t.mu.RUnlock()
	if s != nil {
		s.touch()
	}
	return s
}

// GetOrCreate returns the existing session or creates a new one using newFn.
// newFn is called without any locks held.
func (t *Table) GetOrCreate(key Key, newFn func() *Session) (*Session, bool) {
	t.mu.RLock()
	s := t.entries[key]
	t.mu.RUnlock()
	if s != nil {
		s.touch()
		return s, false
	}

	s = newFn()
	s.LastSeen = time.Now()
	s.PacketCount = 1

	t.mu.Lock()
	// Check again after acquiring write lock.
	if existing, ok := t.entries[key]; ok {
		t.mu.Unlock()
		existing.touch()
		return existing, false
	}
	t.entries[key] = s
	t.mu.Unlock()
	return s, true
}

// Delete removes a session.
func (t *Table) Delete(key Key) {
	t.mu.Lock()
	delete(t.entries, key)
	t.mu.Unlock()
}

// Len returns the number of active sessions.
func (t *Table) Len() int {
	t.mu.RLock()
	n := len(t.entries)
	t.mu.RUnlock()
	return n
}

// reaper periodically removes expired sessions.
func (t *Table) reaper() {
	ticker := time.NewTicker(t.timeout / 2)
	defer ticker.Stop()
	for range ticker.C {
		t.evictExpired()
	}
}

func (t *Table) evictExpired() {
	cutoff := time.Now().Add(-t.timeout)
	t.mu.Lock()
	for k, s := range t.entries {
		s.mu.Lock()
		expired := s.LastSeen.Before(cutoff)
		s.mu.Unlock()
		if expired {
			delete(t.entries, k)
		}
	}
	t.mu.Unlock()
}
