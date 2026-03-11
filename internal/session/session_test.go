package session

import (
	"net"
	"testing"
	"time"
)

func TestGetOrCreate(t *testing.T) {
	tbl := NewTable(5 * time.Second)
	addr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:12345")
	key := Key{ClientAddr: addr.String(), FrontendName: "fe1"}

	s1, created := tbl.GetOrCreate(key, func() *Session {
		return &Session{ClientAddr: addr, BackendAddr: "10.0.0.1:5000"}
	})
	if !created {
		t.Fatal("expected session to be created")
	}
	if s1.BackendAddr != "10.0.0.1:5000" {
		t.Errorf("BackendAddr = %q, want 10.0.0.1:5000", s1.BackendAddr)
	}

	// Second call should return existing session.
	s2, created := tbl.GetOrCreate(key, func() *Session {
		return &Session{ClientAddr: addr, BackendAddr: "10.0.0.2:5000"}
	})
	if created {
		t.Fatal("expected existing session to be returned")
	}
	if s2.BackendAddr != "10.0.0.1:5000" {
		t.Errorf("BackendAddr = %q, want 10.0.0.1:5000 (should reuse existing)", s2.BackendAddr)
	}
}

func TestGet(t *testing.T) {
	tbl := NewTable(5 * time.Second)
	key := Key{ClientAddr: "127.0.0.1:9999", FrontendName: "fe"}

	if got := tbl.Get(key); got != nil {
		t.Fatal("expected nil for missing session")
	}

	addr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:9999")
	tbl.GetOrCreate(key, func() *Session {
		return &Session{ClientAddr: addr, BackendAddr: "backend:80"}
	})
	if got := tbl.Get(key); got == nil {
		t.Fatal("expected session to be found")
	}
}

func TestDelete(t *testing.T) {
	tbl := NewTable(5 * time.Second)
	key := Key{ClientAddr: "127.0.0.1:1111", FrontendName: "fe"}
	addr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:1111")
	tbl.GetOrCreate(key, func() *Session {
		return &Session{ClientAddr: addr, BackendAddr: "b:80"}
	})
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
		entries: make(map[Key]*Session),
		timeout: 100 * time.Millisecond,
	}
	key := Key{ClientAddr: "127.0.0.1:2222", FrontendName: "fe"}
	addr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:2222")
	s := &Session{ClientAddr: addr, BackendAddr: "b:80", LastSeen: time.Now().Add(-200 * time.Millisecond)}
	tbl.entries[key] = s

	tbl.evictExpired()

	if tbl.Len() != 0 {
		t.Fatalf("Len = %d after expiry eviction, want 0", tbl.Len())
	}
}
