// Package frontend implements a UDP listening frontend that routes packets to a backend pool.
package frontend

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ErwinsExpertise/light-udp-proxy/internal/abuse"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/backend"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/config"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/fragment"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/metrics"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/session"
	"github.com/ErwinsExpertise/light-udp-proxy/internal/shaping"
)

// fragmentOOBBufferSize is large enough to hold a few small IP control
// messages (including IP_RECVFRAGSIZE) returned by recvmsg.
const fragmentOOBBufferSize = 128

const (
	// pendingRouteTTL bounds how long an unanswered backend request can keep a
	// response route entry before it is expired.
	pendingRouteTTL = 30 * time.Second
	// maxPendingRoutesPerBackend bounds per-backend memory usage for response
	// route tracking.
	maxPendingRoutesPerBackend = 1024
)

// pkt is a forwarding work item passed from reader goroutines to forwarding workers.
// The bufPtr field is owned by the work item until the worker returns it to the pool.
type pkt struct {
	clientAddr netip.AddrPort // no-alloc address from ReadFromUDPAddrPort
	bufPtr     *[]byte        // buffer from sync.Pool; returned after send
	n          int            // number of valid bytes in *bufPtr
	connIndex  int            // listener socket index the packet arrived on
}

type pendingRoute struct {
	id         uint64
	clientAddr netip.AddrPort
	connIndex  int
	queuedAt   time.Time
}

// Frontend listens on a UDP port and proxies packets to a backend pool.
type Frontend struct {
	cfg      config.FrontendConfig
	pool     *backend.Pool
	sessions *session.Table
	counters *metrics.Counters
	log      *slog.Logger

	// listenConns holds one socket per reader goroutine. When SO_REUSEPORT is
	// enabled the kernel distributes packets across them; otherwise a single
	// socket is used by all readers.
	listenConns []*net.UDPConn
	// sendConn is a shared unconnected socket used by forwarding workers.
	// Using a single shared socket avoids the cost of creating a new connection
	// per packet.
	sendConn *net.UDPConn

	stopOnce sync.Once
	stopCh   chan struct{}
	wg       sync.WaitGroup

	// workCh is the bounded channel from readers to forwarding workers.
	workCh chan pkt

	// bufPool re-uses packet buffers to eliminate per-packet allocations.
	bufPool sync.Pool

	maxPacketSize  int
	globalShaper   *shaping.Bucket
	frontendShaper *shaping.Bucket
	backendShaper  *shaping.Bucket
	clientShaper   *shaping.ClientLimiter
	abuseProtector *abuse.Protector
	dropFragments  bool
	fragmentAware  bool

	pendingRoutesMu sync.Mutex
	pendingRoutes   map[string][]pendingRoute
	pendingRouteSeq atomic.Uint64
}

// RuntimeOptions configures packet policy for a frontend.
type RuntimeOptions struct {
	GlobalShaper   *shaping.Bucket
	FrontendShaper *shaping.Bucket
	BackendShaper  *shaping.Bucket
	ClientShaper   *shaping.ClientLimiter
	AbuseProtector *abuse.Protector
	DropFragments  bool
}

// New creates a Frontend.
func New(
	cfg config.FrontendConfig,
	pool *backend.Pool,
	sessions *session.Table,
	counters *metrics.Counters,
	maxPacketSize int,
	opts RuntimeOptions,
	log *slog.Logger,
) *Frontend {
	if cfg.MaxPacketSize > 0 {
		maxPacketSize = cfg.MaxPacketSize
	}
	f := &Frontend{
		cfg:            cfg,
		pool:           pool,
		sessions:       sessions,
		counters:       counters,
		log:            log,
		stopCh:         make(chan struct{}),
		maxPacketSize:  maxPacketSize,
		globalShaper:   opts.GlobalShaper,
		frontendShaper: opts.FrontendShaper,
		backendShaper:  opts.BackendShaper,
		clientShaper:   opts.ClientShaper,
		abuseProtector: opts.AbuseProtector,
		dropFragments:  opts.DropFragments,
		pendingRoutes:  make(map[string][]pendingRoute),
	}
	f.bufPool = sync.Pool{
		New: func() any {
			// Allocate maxPacketSize+1 bytes so we can detect truncated datagrams:
			// if ReadFromUDPAddrPort fills the entire buffer the OS silently
			// truncated the packet, and we can drop it instead of forwarding
			// corrupt data.
			b := make([]byte, maxPacketSize+1)
			return &b
		},
	}
	return f
}

// Start binds listener socket(s) and launches reader + forwarding worker goroutines.
func (f *Frontend) Start(workerCount int, socketCfg config.SocketConfig) error {
	if workerCount < 1 {
		workerCount = 1
	}

	lc := newListenConfig(socketCfg)

	// With SO_REUSEPORT we create one socket per reader so the kernel can
	// distribute incoming datagrams across cores without a shared lock.
	// Without it we share a single socket across all readers.
	numListeners := 1
	if socketCfg.ReusePort && workerCount > 1 {
		numListeners = workerCount
	}

	// When SO_REUSEPORT is used with multiple listeners, binding to an ephemeral
	// port (port 0) would cause each socket to get a different random port, making
	// the frontend listen on multiple unrelated ports. Reject this combination.
	if numListeners > 1 {
		addr, err := net.ResolveUDPAddr("udp", f.cfg.Listen)
		if err != nil {
			return fmt.Errorf("frontend %q: invalid listen address %q: %w", f.cfg.Name, f.cfg.Listen, err)
		}
		if addr.Port == 0 {
			return fmt.Errorf("frontend %q: reuse_port with multiple workers requires a fixed port, not ephemeral port 0", f.cfg.Name)
		}
	}

	for i := 0; i < numListeners; i++ {
		pc, err := lc.ListenPacket(context.Background(), "udp", f.cfg.Listen)
		if err != nil {
			f.closeListeners()
			return fmt.Errorf("frontend %q: listen: %w", f.cfg.Name, err)
		}
		f.listenConns = append(f.listenConns, pc.(*net.UDPConn))
	}
	if f.dropFragments {
		f.fragmentAware = true
		for _, c := range f.listenConns {
			if !fragment.EnableDetection(c) {
				f.fragmentAware = false
				break
			}
		}
		if !f.fragmentAware {
			f.log.Warn("fragment dropping requested but ancillary fragment detection is unavailable; packets will not be dropped",
				"frontend", f.cfg.Name)
		}
	}

	// Shared outbound socket for forwarding workers.
	sendPC, err := lc.ListenPacket(context.Background(), "udp", "")
	if err != nil {
		f.closeListeners()
		return fmt.Errorf("frontend %q: opening send socket: %w", f.cfg.Name, err)
	}
	f.sendConn = sendPC.(*net.UDPConn)

	// Bounded work channel: if workers fall behind, excess packets are dropped
	// rather than accumulating unbounded memory.
	fwdWorkers := workerCount * 4
	if fwdWorkers < 8 {
		fwdWorkers = 8
	}
	f.workCh = make(chan pkt, fwdWorkers*2)

	f.log.Info("frontend started",
		"name", f.cfg.Name,
		"listen", f.listenConns[0].LocalAddr(),
		"readers", numListeners,
		"fwd_workers", fwdWorkers,
		"reuse_port", socketCfg.ReusePort,
	)

	// Start forwarding workers.
	for i := 0; i < fwdWorkers; i++ {
		f.wg.Add(1)
		go f.forwardWorker()
	}
	f.wg.Add(1)
	go f.backendReadLoop()

	// Start reader goroutines. If there are multiple listener sockets (SO_REUSEPORT),
	// each reader is pinned to its own socket; otherwise all readers share one socket.
	for i := 0; i < workerCount; i++ {
		connIndex := i % len(f.listenConns)
		conn := f.listenConns[connIndex]
		f.wg.Add(1)
		go f.readLoop(conn, connIndex)
	}

	return nil
}

// Stop signals the frontend to stop and waits for all goroutines to exit.
func (f *Frontend) Stop() {
	f.stopOnce.Do(func() {
		close(f.stopCh)
		f.closeListeners()
		if f.sendConn != nil {
			f.sendConn.Close()
		}
	})
	f.wg.Wait()
	f.log.Info("frontend stopped", "name", f.cfg.Name)
}

// Name returns the frontend name.
func (f *Frontend) Name() string { return f.cfg.Name }

// Addr returns the local address the first listener is bound to.
// Useful in tests where the OS assigns the port (":0").
func (f *Frontend) Addr() string {
	if len(f.listenConns) > 0 {
		return f.listenConns[0].LocalAddr().String()
	}
	return ""
}

func (f *Frontend) closeListeners() {
	for _, c := range f.listenConns {
		c.Close()
	}
}

// readLoop reads datagrams from conn and enqueues them for forwarding workers.
// It uses ReadFromUDPAddrPort to obtain the client address without heap allocation.
func (f *Frontend) readLoop(conn *net.UDPConn, connIndex int) {
	defer f.wg.Done()

	for {
		select {
		case <-f.stopCh:
			return
		default:
		}

		bufPtr := f.bufPool.Get().(*[]byte)
		buf := *bufPtr

		var (
			n          int
			clientAddr netip.AddrPort
			err        error
			oobN       int
		)
		var oob [fragmentOOBBufferSize]byte
		if f.dropFragments && f.fragmentAware {
			n, oobN, _, clientAddr, err = conn.ReadMsgUDPAddrPort(buf, oob[:])
		} else {
			n, clientAddr, err = conn.ReadFromUDPAddrPort(buf)
		}
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

		// Drop oversized datagrams. Because the buffer is maxPacketSize+1 bytes,
		// n > maxPacketSize means the OS had to truncate the datagram; the extra
		// byte acts as a sentinel so we can reliably detect and drop such packets.
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

		if f.dropFragments && f.fragmentAware && fragment.IsFragmentedOOB(oob[:oobN]) {
			f.counters.PacketsDropped.Add(1)
			f.counters.PacketsDroppedFragment.Add(1)
			f.bufPool.Put(bufPtr)
			continue
		}

		now := time.Now()
		if !f.allowShaped(clientAddr, n, now) {
			f.counters.PacketsDropped.Add(1)
			f.counters.PacketsShaped.Add(1)
			f.counters.PacketsDroppedRateLimit.Add(1)
			f.bufPool.Put(bufPtr)
			continue
		}
		if f.abuseProtector != nil && !f.abuseProtector.Allow(clientAddr, now) {
			f.counters.PacketsDropped.Add(1)
			f.counters.PacketsDroppedAbuse.Add(1)
			f.bufPool.Put(bufPtr)
			continue
		}

		// Session cap: only call Len() (O(1) atomic read) when we would
		// otherwise create a new session. For existing sessions, no check needed.
		if f.cfg.MaxSessions > 0 {
			key := session.Key{Addr: clientAddr, FrontendName: f.cfg.Name}
			if f.sessions.Get(key) == nil && f.sessions.Len() >= f.cfg.MaxSessions {
				f.counters.PacketsDropped.Add(1)
				f.bufPool.Put(bufPtr)
				continue
			}
		}

		// Zero-copy: transfer buffer ownership to the work item.
		// The forwarding worker returns bufPtr to the pool after sending.
		p := pkt{clientAddr: clientAddr, bufPtr: bufPtr, n: n, connIndex: connIndex}
		select {
		case f.workCh <- p:
		default:
			// Worker pool saturated — drop to preserve stability.
			f.counters.PacketsDropped.Add(1)
			f.bufPool.Put(bufPtr)
		}
	}
}

// forwardWorker drains workCh and forwards each packet.
func (f *Frontend) forwardWorker() {
	defer f.wg.Done()
	for {
		select {
		case <-f.stopCh:
			// Drain any remaining work and return buffers to pool.
			for {
				select {
				case p := <-f.workCh:
					f.bufPool.Put(p.bufPtr)
				default:
					return
				}
			}
		case p, ok := <-f.workCh:
			if !ok {
				return
			}
			f.forward(p)
			// Return buffer to pool now that the send is done.
			f.bufPool.Put(p.bufPtr)
		}
	}
}

// forward routes a packet to the appropriate backend server.
func (f *Frontend) forward(p pkt) {
	key := session.Key{Addr: p.clientAddr, FrontendName: f.cfg.Name}
	payload := (*p.bufPtr)[:p.n]

	var (
		srv  *backend.Server
		sess *session.Session
	)

	if f.cfg.SessionAffinity {
		clientIP := p.clientAddr.Addr().String()
		var created bool
		sess, created = f.sessions.GetOrCreate(key, func() *session.Session {
			s, err := f.pool.Pick(clientIP)
			if err != nil {
				return nil
			}
			return session.NewSession(s.Address)
		})
		if sess == nil {
			f.counters.PacketsDropped.Add(1)
			return
		}

		if created {
			// Wire connCount: increment the chosen server's active session count
			// and register a callback so it decrements on session eviction/deletion.
			if s := f.pool.ServerByAddr(sess.BackendAddr()); s != nil {
				s.IncrConns()
				sess.UpdateServer(s.Address, func() { s.DecrConns() })
			}
		}

		sess.Touch(int64(p.n))

		// BackendAddr is an atomic load; no mutex needed.
		backendAddr := sess.BackendAddr()
		srv = f.pool.ServerByAddr(backendAddr)
		if srv == nil || !srv.Healthy.Load() {
			// Session server went unhealthy; re-pick and update the sticky binding.
			newSrv, err := f.pool.Pick(clientIP)
			if err != nil {
				f.counters.PacketsDropped.Add(1)
				return
			}
			// Transfer connCount from old server to new one.
			if srv != nil {
				srv.DecrConns()
			}
			newSrv.IncrConns()
			sess.UpdateServer(newSrv.Address, func() { newSrv.DecrConns() })
			srv = newSrv
		}
	} else {
		var err error
		srv, err = f.pool.Pick(p.clientAddr.Addr().String())
		if err != nil {
			f.counters.PacketsDropped.Add(1)
			f.log.Warn("no healthy backend", "frontend", f.cfg.Name, "err", err)
			return
		}

		var createdNonAffinity bool
		sess, createdNonAffinity = f.sessions.GetOrCreate(key, func() *session.Session {
			return session.NewSession(srv.Address)
		})
		if sess == nil {
			f.counters.PacketsDropped.Add(1)
			return
		}
		if !createdNonAffinity && sess.BackendAddr() != srv.Address {
			// Keep reverse-path mapping up-to-date for non-affinity mode.
			sess.UpdateServer(srv.Address, nil)
		}
		sess.Touch(int64(p.n))
	}
	sess.SetConnIndex(int32(p.connIndex))

	routeID, enqueued := f.enqueuePendingRoute(srv.Address, p.clientAddr, p.connIndex, time.Now())
	if !enqueued {
		f.counters.PacketsDropped.Add(1)
		f.log.Debug("dropping packet due to full pending response queue",
			"frontend", f.cfg.Name,
			"backend", srv.Address)
		return
	}

	// Use WriteToUDPAddrPort for a zero-allocation send path.
	if f.backendShaper != nil && !f.backendShaper.Allow(p.n, time.Now()) {
		f.cancelPendingRoute(srv.Address, routeID, time.Now())
		f.counters.PacketsDropped.Add(1)
		f.counters.PacketsShaped.Add(1)
		f.counters.PacketsDroppedRateLimit.Add(1)
		return
	}
	if _, err := f.sendConn.WriteToUDPAddrPort(payload, srv.AddrPort()); err != nil {
		f.cancelPendingRoute(srv.Address, routeID, time.Now())
		f.counters.PacketsDropped.Add(1)
		f.log.Debug("send error", "frontend", f.cfg.Name, "backend", srv.Address, "err", err)
		return
	}

	f.counters.PacketsForwarded.Add(1)
	f.counters.BytesOut.Add(int64(p.n))
}

// backendReadLoop receives backend responses on the shared outbound socket and
// forwards them to the oldest pending client route queued for that backend.
func (f *Frontend) backendReadLoop() {
	defer f.wg.Done()
	for {
		select {
		case <-f.stopCh:
			return
		default:
		}

		bufPtr := f.bufPool.Get().(*[]byte)
		buf := *bufPtr

		n, backendAddr, err := f.sendConn.ReadFromUDPAddrPort(buf)
		if err != nil {
			f.bufPool.Put(bufPtr)
			select {
			case <-f.stopCh:
				return
			default:
				f.log.Debug("backend read error", "frontend", f.cfg.Name, "err", err)
				continue
			}
		}

		srv := f.pool.ServerByAddrPort(backendAddr)
		if srv == nil {
			f.log.Debug("dropping backend response from unknown source",
				"frontend", f.cfg.Name,
				"backend", backendAddr)
			f.bufPool.Put(bufPtr)
			continue
		}

		route, ok := f.dequeuePendingRoute(srv.Address)
		if !ok {
			f.log.Debug("dropping backend response with no pending client route",
				"frontend", f.cfg.Name,
				"backend", srv.Address)
			f.bufPool.Put(bufPtr)
			continue
		}
		clientAddr := route.clientAddr
		connIndex := route.connIndex
		conn := f.listenerForIndex(connIndex)
		if conn == nil {
			conn = f.listenerForClient(clientAddr)
		}
		if conn == nil {
			f.bufPool.Put(bufPtr)
			continue
		}
		if _, err := conn.WriteToUDPAddrPort(buf[:n], clientAddr); err != nil {
			f.log.Debug("client send error", "frontend", f.cfg.Name, "client", clientAddr, "err", err)
		}
		f.bufPool.Put(bufPtr)
	}
}

func (f *Frontend) enqueuePendingRoute(backendAddr string, clientAddr netip.AddrPort, connIndex int, now time.Time) (uint64, bool) {
	f.pendingRoutesMu.Lock()
	defer f.pendingRoutesMu.Unlock()

	queue := pruneExpiredRoutes(f.pendingRoutes[backendAddr], now)
	if len(queue) >= maxPendingRoutesPerBackend {
		f.pendingRoutes[backendAddr] = queue
		return 0, false
	}
	id := f.pendingRouteSeq.Add(1)
	queue = append(queue, pendingRoute{
		id:         id,
		clientAddr: clientAddr,
		connIndex:  connIndex,
		queuedAt:   now,
	})
	f.pendingRoutes[backendAddr] = queue
	return id, true
}

func (f *Frontend) cancelPendingRoute(backendAddr string, routeID uint64, now time.Time) {
	f.pendingRoutesMu.Lock()
	defer f.pendingRoutesMu.Unlock()

	queue := pruneExpiredRoutes(f.pendingRoutes[backendAddr], now)
	for i, route := range queue {
		if route.id == routeID {
			queue = append(queue[:i], queue[i+1:]...)
			break
		}
	}
	if len(queue) == 0 {
		delete(f.pendingRoutes, backendAddr)
		return
	}
	f.pendingRoutes[backendAddr] = queue
}

func (f *Frontend) dequeuePendingRoute(backendAddr string) (pendingRoute, bool) {
	f.pendingRoutesMu.Lock()
	defer f.pendingRoutesMu.Unlock()

	queue := pruneExpiredRoutes(f.pendingRoutes[backendAddr], time.Now())
	if len(queue) == 0 {
		delete(f.pendingRoutes, backendAddr)
		return pendingRoute{}, false
	}
	route := queue[0]
	queue = queue[1:]
	if len(queue) == 0 {
		delete(f.pendingRoutes, backendAddr)
	} else {
		f.pendingRoutes[backendAddr] = queue
	}
	return route, true
}

func pruneExpiredRoutes(queue []pendingRoute, now time.Time) []pendingRoute {
	cutoff := now.Add(-pendingRouteTTL)
	firstValid := 0
	for firstValid < len(queue) && queue[firstValid].queuedAt.Before(cutoff) {
		firstValid++
	}
	if firstValid == 0 {
		return queue
	}
	return queue[firstValid:]
}

// listenerForIndex returns the listener socket for connIndex, or nil if the
// index is outside the current listener socket slice bounds.
func (f *Frontend) listenerForIndex(connIndex int) *net.UDPConn {
	if connIndex < 0 || connIndex >= len(f.listenConns) {
		return nil
	}
	return f.listenConns[connIndex]
}

// listenerForClient picks a listener socket deterministically from the client
// address, so response packets use a stable socket when SO_REUSEPORT is enabled.
func (f *Frontend) listenerForClient(clientAddr netip.AddrPort) *net.UDPConn {
	if len(f.listenConns) == 0 {
		return nil
	}
	if len(f.listenConns) == 1 {
		return f.listenConns[0]
	}
	const (
		// FNV-1a 32-bit constants for deterministic client->socket mapping.
		offset32 = 2166136261
		prime32  = 16777619
	)
	h := uint32(offset32)
	ip := clientAddr.Addr().As16()
	for _, b := range ip {
		h ^= uint32(b)
		h *= prime32
	}
	port := clientAddr.Port()
	h ^= uint32(port >> 8)
	h *= prime32
	h ^= uint32(port & 0xff)
	h *= prime32
	return f.listenConns[int(h)%len(f.listenConns)]
}

func (f *Frontend) allowShaped(clientAddr netip.AddrPort, packetSize int, now time.Time) bool {
	if f.globalShaper != nil && !f.globalShaper.Allow(packetSize, now) {
		return false
	}
	if f.frontendShaper != nil && !f.frontendShaper.Allow(packetSize, now) {
		return false
	}
	if f.clientShaper != nil && !f.clientShaper.Allow(clientAddr.Addr(), packetSize, now) {
		return false
	}
	return true
}
