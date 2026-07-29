package main

import (
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"tailscale.com/tsnet"
)

const defaultBackendDialTimeout = 500 * time.Millisecond

// copyBufSize is the per-direction copy buffer. The client side is a netstack
// endpoint rather than an *os.File or *net.TCPConn, so the kernel cannot splice
// and every byte moves through this buffer.
const copyBufSize = 64 << 10

var (
	backendDialTimeout = defaultBackendDialTimeout
	proxyIdleTimeout   time.Duration
	reachCache         *reachabilityCache

	copyBufPool = sync.Pool{
		New: func() any {
			b := make([]byte, copyBufSize)
			return &b
		},
	}

	// dialBackend is a variable so tests can inject failure modes.
	dialBackend = func(dst netip.AddrPort) (net.Conn, error) {
		return net.DialTimeout("tcp", dst.String(), backendDialTimeout)
	}
)

// registerSubnetTCPForwarder installs a TCP handler for traffic to advertised
// subnets. Call after srv.Up() so netstack is initialized.
//
// We hook netstack.GetTCPHandlerForFlow directly instead of
// RegisterFallbackTCPHandler so dial-first probes run on gVisor's per-connection
// goroutines without blocking on tsnet's global mutex.
//
// The backend is dialed before netstack completes the client handshake.
// Otherwise scanners (e.g. nmap -sS) see SYN-ACK on every port because we used
// to accept first and only dial afterward.
//
// This callback runs while netstack still holds a gVisor forwarder slot and a
// per-client in-flight slot; both are released later, in getConnOrReset. Once
// the forwarder table fills, gVisor drops further SYNs without a RST, which
// clients read as rate limiting. Everything here must therefore be bounded: the
// reachability cache answers repeat traffic without dialing at all, and the
// dial slot wait has a deadline.
func registerSubnetTCPForwarder(srv *tsnet.Server, routes []netip.Prefix) error {
	ns, err := netstackForServer(srv)
	if err != nil {
		return err
	}

	orig := ns.GetTCPHandlerForFlow
	ns.GetTCPHandlerForFlow = func(src, dst netip.AddrPort) (func(net.Conn), bool) {
		if orig != nil {
			if h, intercept := orig(src, dst); intercept && h != nil {
				return h, true
			}
		}
		return tcpHandlerForFlow(routes, dst)
	}
	return nil
}

// flowVerdict is what should happen to one inbound flow.
type flowVerdict int

const (
	// verdictProxy hands the flow to a handler, so netstack completes the
	// client handshake.
	verdictProxy flowVerdict = iota

	// verdictReset answers with a RST. A client reports that as "connection
	// refused", so it is only honest when the target itself refused.
	verdictReset

	// verdictDrop answers with nothing and lets the client time out. This is
	// the honest answer whenever we did not get a refusal from the target:
	// the port may be filtered, or we may have run out of our own capacity,
	// and neither is proof that the port is closed.
	//
	// netstack cannot express it. Every path through its acceptTCP ends in
	// either Complete(true), which resets, or a created endpoint, which
	// completes the handshake and makes the port look open. Until that hook
	// grows a drop, tcpHandlerForFlow downgrades this to a reset and counts
	// it in metricResetAmbiguous, so the accuracy lost is at least visible.
	verdictDrop
)

// tcpHandlerForFlow adapts a verdict to netstack's hook, which takes a handler
// and an intercept flag: a nil handler with intercept=true resets the flow.
func tcpHandlerForFlow(routes []netip.Prefix, dst netip.AddrPort) (func(net.Conn), bool) {
	handler, verdict := classifyFlow(routes, dst)
	switch verdict {
	case verdictProxy:
		return handler, true
	case verdictDrop:
		metricResetAmbiguous.Add(1)
	}
	return nil, true
}

// classifyFlow decides the fate of one flow to an advertised subnet.
//
// A refusal from the target is the only failure that proves a port is closed,
// so it is the only one that earns a reset. Timeouts, unreachable errors, and
// our own capacity ceilings all return verdictDrop: reporting those as closed
// turns a firewalled port, or a busy moment on this node, into a wrong answer
// for whatever is scanning through it.
func classifyFlow(routes []netip.Prefix, dst netip.AddrPort) (func(net.Conn), flowVerdict) {
	if !containsAddr(routes, dst.Addr()) {
		// Not traffic we route; a reset is the honest answer.
		return nil, verdictReset
	}

	if open, cached := reachCache.get(dst); cached {
		if !open {
			// Only refusals are cached as closed, so this reset is backed by
			// an RST the target sent us within the negative TTL.
			metricResetRefused.Add(1)
			return nil, verdictReset
		}
		// Known reachable: accept now and dial from the handler, which runs
		// after netstack releases its in-flight slots.
		return func(client net.Conn) { proxyDeferred(client, dst) }, verdictProxy
	}

	// Unknown target: dial here so a closed port still gets a RST rather than a
	// completed handshake.
	if !acquireConnSlot() {
		metricConnLimited.Add(1)
		return nil, verdictDrop
	}
	if !acquireDialSlot() {
		releaseConnSlot()
		metricDialSlotTimeout.Add(1)
		return nil, verdictDrop
	}
	metricDialsTotal.Add(1)
	backend, err := dialBackend(dst)
	releaseDialSlot()
	if err != nil {
		releaseConnSlot()
		recordDialFailure(err)
		if isDefinitiveRefusal(err) {
			reachCache.markClosed(dst)
			metricResetRefused.Add(1)
			return nil, verdictReset
		}
		return nil, verdictDrop
	}
	metricDialsOK.Add(1)
	reachCache.markOpen(dst)

	// netstack skips the handler if the client handshake fails, so the backend
	// is parked where the sweeper can reclaim it.
	h := newBackendHandoff(backend)
	return func(client net.Conn) {
		conn := h.claim()
		if conn == nil {
			client.Close()
			return
		}
		defer releaseConnSlot()
		metricConnsAccepted.Add(1)
		proxyTCP(client, conn)
	}, verdictProxy
}

// proxyDeferred dials the backend from the connection handler. netstack has
// already released the forwarder and in-flight slots by the time a handler
// runs, so waiting here costs only this flow.
func proxyDeferred(client net.Conn, dst netip.AddrPort) {
	if !acquireConnSlot() {
		metricConnLimited.Add(1)
		client.Close()
		return
	}
	defer releaseConnSlot()

	acquireDialSlotBlocking()
	metricDialsTotal.Add(1)
	backend, err := dialBackend(dst)
	releaseDialSlot()
	if err != nil {
		recordDialFailure(err)
		metricLazyDialFail.Add(1)
		if isDefinitiveRefusal(err) {
			reachCache.markClosed(dst)
		}
		client.Close()
		return
	}
	metricDialsOK.Add(1)
	metricConnsAccepted.Add(1)
	proxyTCP(client, backend)
}

// registerSubnetForwarders installs TCP and UDP handlers for advertised subnets.
// Call after srv.Up().
func registerSubnetForwarders(srv *tsnet.Server, routes []netip.Prefix) error {
	if err := registerSubnetTCPForwarder(srv, routes); err != nil {
		return err
	}
	return registerSubnetUDPForwarder(srv, routes)
}

func containsAddr(routes []netip.Prefix, addr netip.Addr) bool {
	for _, p := range routes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// isDefinitiveRefusal reports whether a dial error proves the port is closed.
//
// Only ECONNREFUSED qualifies, because only an RST comes from the target's own
// stack. A timeout is ambiguous: the target may be filtering, or it may simply
// be saturated, and remembering "closed" for a saturated target turns a
// transient stall into a run of wrong answers for the whole negative TTL.
//
// EHOSTUNREACH and ENETUNREACH look definitive but are not. They are raised
// locally by ARP resolution or by an intermediate router's ICMP unreachable,
// and under connection churn against a filtering host they show up on a few
// percent of dials to ports that are merely filtered. Caching those would
// report a filtered port as closed. Ambiguous failures are re-dialed instead.
func isDefinitiveRefusal(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}

// closeWriter is satisfied by *net.TCPConn and by netstack's *gonet.TCPConn.
type closeWriter interface {
	CloseWrite() error
}

func proxyTCP(client, backend net.Conn) {
	if proxyIdleTimeout > 0 {
		client = newIdleConn(client, proxyIdleTimeout)
		backend = newIdleConn(backend, proxyIdleTimeout)
	}

	defer client.Close()
	defer backend.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		copyDir(backend, client, metricBytesToBackend)
	}()
	go func() {
		defer wg.Done()
		copyDir(client, backend, metricBytesToClient)
	}()
	wg.Wait()
}

// copyDir copies one direction and then half-closes the destination so the peer
// observes the FIN. Without that, a backend that replies only after the client
// finishes writing never sees end-of-stream, and both sides wait until the
// netstack keepalive expires hours later.
func copyDir(dst, src net.Conn, sink *counter) {
	bufp := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bufp)

	// readerOnly and writerOnly hide the ReaderFrom and WriterTo
	// implementations so io.CopyBuffer uses the pooled buffer instead of
	// allocating its own; splice is unavailable here because one side is
	// always a netstack endpoint.
	n, _ := io.CopyBuffer(writerOnly{dst}, readerOnly{src}, *bufp)
	sink.Add(n)

	if cw, ok := dst.(closeWriter); ok {
		cw.CloseWrite()
		return
	}
	dst.Close()
}

type readerOnly struct{ io.Reader }
type writerOnly struct{ io.Writer }

// idleConn closes a flow that goes quiet for too long. netstack's own keepalive
// defaults to roughly two hours, which is far too long to hold a descriptor on
// a busy node.
type idleConn struct {
	net.Conn
	timeout time.Duration
	lastSet atomic.Int64
}

func newIdleConn(c net.Conn, timeout time.Duration) *idleConn {
	ic := &idleConn{Conn: c, timeout: timeout}
	now := time.Now()
	ic.lastSet.Store(now.UnixNano())
	c.SetDeadline(now.Add(timeout))
	return ic
}

// touch pushes the deadline out, but only once per half-window so that a
// high-throughput flow does not pay a deadline update per read.
func (c *idleConn) touch() {
	now := time.Now().UnixNano()
	last := c.lastSet.Load()
	if now-last < int64(c.timeout/2) {
		return
	}
	if c.lastSet.CompareAndSwap(last, now) {
		c.Conn.SetDeadline(time.Unix(0, now).Add(c.timeout))
	}
}

func (c *idleConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.touch()
	}
	return n, err
}

func (c *idleConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.touch()
	}
	return n, err
}

func (c *idleConn) CloseWrite() error {
	if cw, ok := c.Conn.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}
