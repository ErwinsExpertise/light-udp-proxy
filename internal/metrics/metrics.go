// Package metrics exposes an HTTP endpoint with proxy statistics.
package metrics

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
)

// Counters holds global proxy statistics.
type Counters struct {
	PacketsReceived  atomic.Int64
	PacketsForwarded atomic.Int64
	PacketsDropped   atomic.Int64
	BytesIn          atomic.Int64
	BytesOut         atomic.Int64
	ActiveSessions   atomic.Int64
}

// BackendStatus describes the health of a single backend server.
type BackendStatus struct {
	Address string `json:"address"`
	Healthy bool   `json:"healthy"`
	Conns   int64  `json:"active_conns"`
}

// BackendStatusFn is a callback that returns backend health information.
type BackendStatusFn func() []BackendStatus

// Server is the HTTP metrics server.
type Server struct {
	addr      string
	counters  *Counters
	statusFns []BackendStatusFn
	log       *slog.Logger
	server    *http.Server
}

// New creates a metrics Server bound to addr.
func New(addr string, counters *Counters, log *slog.Logger) *Server {
	return &Server{
		addr:     addr,
		counters: counters,
		log:      log,
	}
}

// RegisterBackend registers a function that returns backend status for the /metrics response.
func (s *Server) RegisterBackend(fn BackendStatusFn) {
	s.statusFns = append(s.statusFns, fn)
}

// Start begins serving the HTTP metrics endpoint on a separate goroutine.
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("metrics server: %w", err)
	}
	s.server = &http.Server{Handler: mux}
	s.log.Info("metrics server started", "addr", ln.Addr().String())
	go s.server.Serve(ln) //nolint:errcheck
	return nil
}

// Stop shuts down the metrics server.
func (s *Server) Stop() {
	if s.server != nil {
		s.server.Close()
	}
}

type metricsResponse struct {
	PacketsReceived  int64           `json:"packets_received"`
	PacketsForwarded int64           `json:"packets_forwarded"`
	PacketsDropped   int64           `json:"packets_dropped"`
	BytesIn          int64           `json:"bytes_in"`
	BytesOut         int64           `json:"bytes_out"`
	ActiveSessions   int64           `json:"active_sessions"`
	Backends         []BackendStatus `json:"backends"`
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	resp := metricsResponse{
		PacketsReceived:  s.counters.PacketsReceived.Load(),
		PacketsForwarded: s.counters.PacketsForwarded.Load(),
		PacketsDropped:   s.counters.PacketsDropped.Load(),
		BytesIn:          s.counters.BytesIn.Load(),
		BytesOut:         s.counters.BytesOut.Load(),
		ActiveSessions:   s.counters.ActiveSessions.Load(),
	}
	for _, fn := range s.statusFns {
		resp.Backends = append(resp.Backends, fn()...)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp) //nolint:errcheck
}
