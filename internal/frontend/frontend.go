// Package frontend implements a UDP listening frontend that routes packets to a backend pool.
package frontend

import (
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/ErwinsExpertise/light-udp-proxy/internal/backend"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/config"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/metrics"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/session"
)

// packet is a forwarding work item dispatched from the read loop to a worker.
type packet struct {
	clientAddr *net.UDPAddr
	payload    []byte
}

// Frontend listens on a UDP port and proxies packets to a backend pool.
type Frontend struct {
	cfg      config.FrontendConfig
	pool     *backend.Pool
	sessions *session.Table
	counters *metrics.Counters
	log      *slog.Logger

	// listenConn is the inbound listener socket.
	listenConn *net.UDPConn
	// sendConn is a single shared unconnected socket used to send packets to backends,
	// avoiding the overhead of creating a new connection per packet.
	sendConn *net.UDPConn

	stopOnce sync.Once
	stopCh   chan struct{}
	wg       sync.WaitGroup

	// workCh is the bounded channel feeding forwarding workers.
	workCh chan packet

	// bufPool reduces allocations in the hot path.
	bufPool sync.Pool

	maxPacketSize int
}

// New creates a Frontend.
func New(
	cfg config.FrontendConfig,
	pool *backend.Pool,
	sessions *session.Table,
	counters *metrics.Counters,
	maxPacketSize int,
	log *slog.Logger,
) *Frontend {
	if cfg.MaxPacketSize > 0 {
		maxPacketSize = cfg.MaxPacketSize
	}
	f := &Frontend{
		cfg:           cfg,
		pool:          pool,
		sessions:      sessions,
		counters:      counters,
		log:           log,
		stopCh:        make(chan struct{}),
		maxPacketSize: maxPacketSize,
	}
	f.bufPool = sync.Pool{
		New: func() interface{} {
			b := make([]byte, maxPacketSize)
			return &b
		},
	}
	return f
}

// workerPoolSize returns a reasonable forwarding worker count based on the reader count.
func workerPoolSize(readers int) int {
	n := readers * 4
	if n < 8 {
		n = 8
	}
	return n
}

// Start binds the UDP socket and starts reader + forwarding worker goroutines.
func (f *Frontend) Start(workerCount, readBufSize, writeBufSize int) error {
	addr, err := net.ResolveUDPAddr("udp", f.cfg.Listen)
	if err != nil {
		return fmt.Errorf("frontend %q: resolving listen addr: %w", f.cfg.Name, err)
	}
	listenConn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("frontend %q: listen: %w", f.cfg.Name, err)
	}
	if err := listenConn.SetReadBuffer(readBufSize); err != nil {
		f.log.Warn("failed to set read buffer", "frontend", f.cfg.Name, "err", err)
	}
	if err := listenConn.SetWriteBuffer(writeBufSize); err != nil {
		f.log.Warn("failed to set write buffer", "frontend", f.cfg.Name, "err", err)
	}
	f.listenConn = listenConn

	// Open a single shared outbound socket for sending to backends.
	sendConn, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		listenConn.Close()
		return fmt.Errorf("frontend %q: opening send socket: %w", f.cfg.Name, err)
	}
	if err := sendConn.SetWriteBuffer(writeBufSize); err != nil {
		f.log.Warn("failed to set send write buffer", "frontend", f.cfg.Name, "err", err)
	}
	f.sendConn = sendConn

	if workerCount < 1 {
		workerCount = 1
	}
	// Bounded channel: if workers fall behind, the reader loop drops packets
	// rather than accumulating unbounded memory.
	fwdWorkers := workerPoolSize(workerCount)
	f.workCh = make(chan packet, fwdWorkers*2)

	f.log.Info("frontend started", "name", f.cfg.Name, "listen", listenConn.LocalAddr())

	// Start forwarding workers.
	for i := 0; i < fwdWorkers; i++ {
		f.wg.Add(1)
		go f.forwardWorker()
	}

	// Start reader goroutines.
	for i := 0; i < workerCount; i++ {
		f.wg.Add(1)
		go f.readLoop()
	}
	return nil
}

// Stop signals the frontend to stop and waits for all goroutines to exit.
func (f *Frontend) Stop() {
	f.stopOnce.Do(func() {
		close(f.stopCh)
		if f.listenConn != nil {
			f.listenConn.Close()
		}
		if f.sendConn != nil {
			f.sendConn.Close()
		}
	})
	f.wg.Wait()
	f.log.Info("frontend stopped", "name", f.cfg.Name)
}

// Name returns the frontend name.
func (f *Frontend) Name() string { return f.cfg.Name }

// readLoop reads packets from the UDP socket and enqueues them for forwarding workers.
func (f *Frontend) readLoop() {
	defer f.wg.Done()

	for {
		select {
		case <-f.stopCh:
			return
		default:
		}

		bufPtr := f.bufPool.Get().(*[]byte)
		buf := *bufPtr

		n, clientAddr, err := f.listenConn.ReadFromUDP(buf)
		if err != nil {
			f.bufPool.Put(bufPtr)
			select {
			case <-f.stopCh:
				return
			default:
				f.log.Debug("read error", "frontend", f.cfg.Name, "err", err)
				continue
			}
		}

		f.counters.PacketsReceived.Add(1)
		f.counters.BytesIn.Add(int64(n))

		// Validate packet size.
		if n > f.maxPacketSize {
			f.counters.PacketsDropped.Add(1)
			f.log.Warn("packet too large, dropping",
				"frontend", f.cfg.Name,
				"client", clientAddr,
				"size", n,
				"max", f.maxPacketSize)
			f.bufPool.Put(bufPtr)
			continue
		}

		// Session limit check: drop new-session packets if the cap is reached.
		if f.cfg.MaxSessions > 0 && f.sessions.Len() >= f.cfg.MaxSessions {
			key := session.Key{ClientAddr: clientAddr.String(), FrontendName: f.cfg.Name}
			if f.sessions.Get(key) == nil {
				f.counters.PacketsDropped.Add(1)
				f.bufPool.Put(bufPtr)
				continue
			}
		}

		// Copy payload before returning the buffer to the pool.
		payload := make([]byte, n)
		copy(payload, buf[:n])
		f.bufPool.Put(bufPtr)

		pkt := packet{clientAddr: clientAddr, payload: payload}
		select {
		case f.workCh <- pkt:
		default:
			// Worker pool is saturated; drop packet to preserve stability.
			f.counters.PacketsDropped.Add(1)
		}
	}
}

// forwardWorker processes forwarding requests from workCh.
func (f *Frontend) forwardWorker() {
	defer f.wg.Done()
	for {
		select {
		case <-f.stopCh:
			// Drain remaining work.
			for {
				select {
				case <-f.workCh:
				default:
					return
				}
			}
		case pkt, ok := <-f.workCh:
			if !ok {
				return
			}
			f.forward(pkt.clientAddr, pkt.payload)
		}
	}
}

// forward routes a packet to the appropriate backend server.
func (f *Frontend) forward(clientAddr *net.UDPAddr, payload []byte) {
	key := session.Key{ClientAddr: clientAddr.String(), FrontendName: f.cfg.Name}

	var srv *backend.Server

	if f.cfg.SessionAffinity {
		sess, created := f.sessions.GetOrCreate(key, func() *session.Session {
			s, err := f.pickServer(clientAddr.IP.String())
			if err != nil {
				return nil
			}
			return &session.Session{
				ClientAddr:  clientAddr,
				BackendAddr: s.Address,
				LastSeen:    time.Now(),
			}
		})
		if sess == nil {
			f.counters.PacketsDropped.Add(1)
			return
		}
		if created {
			f.counters.ActiveSessions.Add(1)
		}
		// Look up the server by address.
		srv = f.serverByAddr(sess.BackendAddr)
		if srv == nil || !srv.Healthy.Load() {
			// Session server went unhealthy; re-pick and update session.
			newSrv, err := f.pickServer(clientAddr.IP.String())
			if err != nil {
				f.counters.PacketsDropped.Add(1)
				return
			}
			sess.BackendAddr = newSrv.Address
			srv = newSrv
		}
	} else {
		var err error
		srv, err = f.pickServer(clientAddr.IP.String())
		if err != nil {
			f.counters.PacketsDropped.Add(1)
			f.log.Warn("no healthy backend", "frontend", f.cfg.Name, "err", err)
			return
		}
	}

	if err := f.sendPacket(srv, payload); err != nil {
		f.counters.PacketsDropped.Add(1)
		f.log.Debug("send error", "frontend", f.cfg.Name, "backend", srv.Address, "err", err)
		return
	}

	f.counters.PacketsForwarded.Add(1)
	f.counters.BytesOut.Add(int64(len(payload)))
}

func (f *Frontend) pickServer(clientIP string) (*backend.Server, error) {
	return f.pool.Pick(clientIP)
}

func (f *Frontend) serverByAddr(addr string) *backend.Server {
	for _, s := range f.pool.Servers() {
		if s.Address == addr {
			return s
		}
	}
	return nil
}

// sendPacket writes a payload to the backend server using the shared outbound socket.
func (f *Frontend) sendPacket(srv *backend.Server, payload []byte) error {
	dst, err := srv.UDPAddr()
	if err != nil {
		return fmt.Errorf("resolving backend addr: %w", err)
	}
	_, err = f.sendConn.WriteToUDP(payload, dst)
	return err
}


// Frontend listens on a UDP port and proxies packets to a backend pool.
type Frontend struct {
	cfg      config.FrontendConfig
	pool     *backend.Pool
	sessions *session.Table
	counters *metrics.Counters
	log      *slog.Logger

	conn     *net.UDPConn
	stopOnce sync.Once
	stopCh   chan struct{}
	wg       sync.WaitGroup

	// bufPool reduces allocations in the hot path.
	bufPool sync.Pool

	maxPacketSize int
}

// New creates a Frontend.
func New(
	cfg config.FrontendConfig,
	pool *backend.Pool,
	sessions *session.Table,
	counters *metrics.Counters,
	maxPacketSize int,
	log *slog.Logger,
) *Frontend {
	if cfg.MaxPacketSize > 0 {
		maxPacketSize = cfg.MaxPacketSize
	}
	f := &Frontend{
		cfg:           cfg,
		pool:          pool,
		sessions:      sessions,
		counters:      counters,
		log:           log,
		stopCh:        make(chan struct{}),
		maxPacketSize: maxPacketSize,
	}
	f.bufPool = sync.Pool{
		New: func() interface{} {
			b := make([]byte, maxPacketSize)
			return &b
		},
	}
	return f
}

// Start binds the UDP socket and starts worker goroutines.
func (f *Frontend) Start(workerCount, readBufSize, writeBufSize int) error {
	addr, err := net.ResolveUDPAddr("udp", f.cfg.Listen)
	if err != nil {
		return fmt.Errorf("frontend %q: resolving listen addr: %w", f.cfg.Name, err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("frontend %q: listen: %w", f.cfg.Name, err)
	}
	if err := conn.SetReadBuffer(readBufSize); err != nil {
		f.log.Warn("failed to set read buffer", "frontend", f.cfg.Name, "err", err)
	}
	if err := conn.SetWriteBuffer(writeBufSize); err != nil {
		f.log.Warn("failed to set write buffer", "frontend", f.cfg.Name, "err", err)
	}
	f.conn = conn
	f.log.Info("frontend started", "name", f.cfg.Name, "listen", conn.LocalAddr())

	if workerCount < 1 {
		workerCount = 1
	}
	for i := 0; i < workerCount; i++ {
		f.wg.Add(1)
		go f.readLoop()
	}
	return nil
}

// Stop signals the frontend to stop and waits for all goroutines to exit.
func (f *Frontend) Stop() {
	f.stopOnce.Do(func() {
		close(f.stopCh)
		if f.conn != nil {
			f.conn.Close()
		}
	})
	f.wg.Wait()
	f.log.Info("frontend stopped", "name", f.cfg.Name)
}

// Name returns the frontend name.
func (f *Frontend) Name() string { return f.cfg.Name }

// readLoop reads packets from the UDP socket and forwards them.
func (f *Frontend) readLoop() {
	defer f.wg.Done()

	for {
		select {
		case <-f.stopCh:
			return
		default:
		}

		bufPtr := f.bufPool.Get().(*[]byte)
		buf := *bufPtr

		n, clientAddr, err := f.conn.ReadFromUDP(buf)
		if err != nil {
			f.bufPool.Put(bufPtr)
			select {
			case <-f.stopCh:
				return
			default:
				f.log.Debug("read error", "frontend", f.cfg.Name, "err", err)
				continue
			}
		}

		f.counters.PacketsReceived.Add(1)
		f.counters.BytesIn.Add(int64(n))

		// Validate packet size.
		if n > f.maxPacketSize {
			f.counters.PacketsDropped.Add(1)
			f.log.Warn("packet too large, dropping",
				"frontend", f.cfg.Name,
				"client", clientAddr,
				"size", n,
				"max", f.maxPacketSize)
			f.bufPool.Put(bufPtr)
			continue
		}

		// Rate limiting: if enabled, check per-client rate.
		if f.cfg.MaxSessions > 0 && f.sessions.Len() >= f.cfg.MaxSessions {
			// Check if this client already has a session; if not, drop.
			key := session.Key{ClientAddr: clientAddr.String(), FrontendName: f.cfg.Name}
			if f.sessions.Get(key) == nil {
				f.counters.PacketsDropped.Add(1)
				f.bufPool.Put(bufPtr)
				continue
			}
		}

		// Copy payload before returning buffer to pool.
		payload := make([]byte, n)
		copy(payload, buf[:n])
		f.bufPool.Put(bufPtr)

		go f.forward(clientAddr, payload)
	}
}

// forward routes a packet to the appropriate backend server.
func (f *Frontend) forward(clientAddr *net.UDPAddr, payload []byte) {
	key := session.Key{ClientAddr: clientAddr.String(), FrontendName: f.cfg.Name}

	var srv *backend.Server

	if f.cfg.SessionAffinity {
		sess, created := f.sessions.GetOrCreate(key, func() *session.Session {
			s, err := f.pickServer(clientAddr.IP.String())
			if err != nil {
				return nil
			}
			return &session.Session{
				ClientAddr:  clientAddr,
				BackendAddr: s.Address,
				LastSeen:    time.Now(),
			}
		})
		if sess == nil {
			f.counters.PacketsDropped.Add(1)
			return
		}
		if created {
			f.counters.ActiveSessions.Add(1)
		}
		// Look up the server by address.
		srv = f.serverByAddr(sess.BackendAddr)
		if srv == nil || !srv.Healthy.Load() {
			// Session server went unhealthy; re-pick and update session.
			newSrv, err := f.pickServer(clientAddr.IP.String())
			if err != nil {
				f.counters.PacketsDropped.Add(1)
				return
			}
			sess.BackendAddr = newSrv.Address
			srv = newSrv
		}
	} else {
		var err error
		srv, err = f.pickServer(clientAddr.IP.String())
		if err != nil {
			f.counters.PacketsDropped.Add(1)
			f.log.Warn("no healthy backend", "frontend", f.cfg.Name, "err", err)
			return
		}
	}

	if err := f.sendPacket(srv, payload); err != nil {
		f.counters.PacketsDropped.Add(1)
		f.log.Debug("send error", "frontend", f.cfg.Name, "backend", srv.Address, "err", err)
		return
	}

	f.counters.PacketsForwarded.Add(1)
	f.counters.BytesOut.Add(int64(len(payload)))
}

func (f *Frontend) pickServer(clientIP string) (*backend.Server, error) {
	return f.pool.Pick(clientIP)
}

func (f *Frontend) serverByAddr(addr string) *backend.Server {
	for _, s := range f.pool.Servers() {
		if s.Address == addr {
			return s
		}
	}
	return nil
}

func (f *Frontend) sendPacket(srv *backend.Server, payload []byte) error {
	dst, err := srv.UDPAddr()
	if err != nil {
		return fmt.Errorf("resolving backend addr: %w", err)
	}
	conn, err := net.DialUDP("udp", nil, dst)
	if err != nil {
		return fmt.Errorf("dial backend: %w", err)
	}
	defer conn.Close()
	_, err = conn.Write(payload)
	return err
}
