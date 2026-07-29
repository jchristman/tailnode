package main

import (
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"tailscale.com/tsnet"
	"tailscale.com/types/nettype"
	"tailscale.com/wgengine/netstack"
)

// udpBufSize is the largest UDP payload we relay. Buffers are pooled because a
// busy node can hold thousands of flows and this would otherwise be two 64 KiB
// allocations per flow.
const udpBufSize = 65535

const (
	defaultUDPIdleTimeout = 2 * time.Minute
	dnsUDPIdleTimeout     = 30 * time.Second
)

var (
	// udpPreserveSourcePort binds the backend socket to the client's source
	// port. Off by default: ports below 1024 need privileges, and two clients
	// using the same source port collide.
	udpPreserveSourcePort bool

	udpBufPool = sync.Pool{
		New: func() any {
			b := make([]byte, udpBufSize)
			return &b
		},
	}
)

// registerSubnetUDPForwarder installs a UDP handler for traffic to advertised
// subnets. Call after srv.Up() so netstack is initialized.
//
// tsnet drops unmatched UDP flows by default; there is no
// RegisterFallbackUDPHandler yet, so we chain onto netstack's handler directly
// (same post-Up reflection pattern as registerSubnetTCPForwarder).
func registerSubnetUDPForwarder(srv *tsnet.Server, routes []netip.Prefix) error {
	ns, err := netstackForServer(srv)
	if err != nil {
		return err
	}

	orig := ns.GetUDPHandlerForFlow
	ns.GetUDPHandlerForFlow = func(src, dst netip.AddrPort) (func(nettype.ConnPacketConn), bool) {
		if orig != nil {
			if h, intercept := orig(src, dst); intercept && h != nil {
				return h, true
			}
		}
		if !containsAddr(routes, dst.Addr()) {
			return nil, true
		}
		return func(client nettype.ConnPacketConn) {
			proxyUDP(client, src, dst)
		}, true
	}
	return nil
}

// netstackForServer returns the netstack instance backing srv.
//
// Sys() is the accessor Tailscale's own tools use, but it is documented as
// unstable, so fall back to reading the unexported field directly.
func netstackForServer(srv *tsnet.Server) (*netstack.Impl, error) {
	// Sys() is nil until the server starts.
	if sys := srv.Sys(); sys != nil {
		if ns, ok := sys.Netstack.GetOK(); ok {
			if impl, ok := ns.(*netstack.Impl); ok && impl != nil {
				return impl, nil
			}
		}
	}
	return netstackByReflection(srv)
}

func netstackByReflection(srv *tsnet.Server) (*netstack.Impl, error) {
	v := reflect.ValueOf(srv).Elem()
	f := v.FieldByName("netstack")
	if !f.IsValid() {
		return nil, fmt.Errorf("tsnet.Server.netstack field not found")
	}
	f = reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem()
	if f.IsNil() {
		return nil, fmt.Errorf("netstack not initialized; call after srv.Up()")
	}
	ns, ok := f.Interface().(*netstack.Impl)
	if !ok {
		return nil, fmt.Errorf("unexpected netstack type %T", f.Interface())
	}
	return ns, nil
}

func proxyUDP(client net.PacketConn, clientAddr, dstAddr netip.AddrPort) {
	defer client.Close()

	if !acquireConnSlot() {
		metricUDPLimited.Add(1)
		return
	}
	defer releaseConnSlot()

	backendConn, err := bindBackendUDP(clientAddr, dstAddr)
	if err != nil {
		metricUDPFlowFail.Add(1)
		return
	}
	defer backendConn.Close()
	metricUDPFlows.Add(1)

	idleTimeout := defaultUDPIdleTimeout
	if dstAddr.Port() == 53 {
		idleTimeout = dnsUDPIdleTimeout
	}

	f := &udpFlow{
		client:        client,
		clientAddr:    net.UDPAddrFromAddrPort(clientAddr),
		backend:       backendConn,
		backendAddr:   net.UDPAddrFromAddrPort(dstAddr),
		backendExpect: dstAddr,
		idleTimeout:   idleTimeout,
	}
	f.run()
}

// bindBackendUDP opens the socket used to reach the backend. An ephemeral port
// is used unless source-port preservation is requested, in which case a
// collision falls back to ephemeral rather than failing the flow.
func bindBackendUDP(clientAddr, dstAddr netip.AddrPort) (*net.UDPConn, error) {
	listenAddr := &net.UDPAddr{}
	if dstAddr.Addr().Is4() {
		listenAddr.IP = net.IPv4zero
	} else {
		listenAddr.IP = net.IPv6zero
	}

	if udpPreserveSourcePort {
		listenAddr.Port = int(clientAddr.Port())
		if c, err := net.ListenUDP("udp", listenAddr); err == nil {
			return c, nil
		}
	}
	listenAddr.Port = 0
	return net.ListenUDP("udp", listenAddr)
}

// udpFlow relays one client/backend pair until both directions go idle.
type udpFlow struct {
	client      net.PacketConn
	clientAddr  net.Addr
	backend     *net.UDPConn
	backendAddr net.Addr

	// backendExpect is the only source we accept backend replies from, so an
	// unrelated host cannot inject packets into the flow.
	backendExpect netip.AddrPort

	idleTimeout time.Duration
	lastSeen    atomic.Int64
	closeOnce   sync.Once
}

func (f *udpFlow) run() {
	f.lastSeen.Store(time.Now().UnixNano())

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); f.copy(f.backend, f.backendAddr, f.client, false) }()
	go func() { defer wg.Done(); f.copy(f.client, f.clientAddr, f.backend, true) }()

	go f.watchIdle(done)
	wg.Wait()
	close(done)
}

// copy relays packets from src to dst. Activity is recorded as a timestamp
// rather than by resetting a timer, because resetting per packet contends on
// the runtime timer heap at high packet rates.
func (f *udpFlow) copy(dst net.PacketConn, dstAddr net.Addr, src net.PacketConn, fromBackend bool) {
	defer f.close()

	bufp := udpBufPool.Get().(*[]byte)
	defer udpBufPool.Put(bufp)
	buf := *bufp

	for {
		n, from, err := src.ReadFrom(buf)
		if err != nil {
			return
		}
		if fromBackend && !f.fromExpectedBackend(from) {
			metricUDPSpoofDrop.Add(1)
			continue
		}
		if _, err := dst.WriteTo(buf[:n], dstAddr); err != nil {
			return
		}
		f.lastSeen.Store(time.Now().UnixNano())
	}
}

func (f *udpFlow) fromExpectedBackend(from net.Addr) bool {
	ua, ok := from.(*net.UDPAddr)
	if !ok {
		return false
	}
	ap := ua.AddrPort()
	return ap.Port() == f.backendExpect.Port() && ap.Addr().Unmap() == f.backendExpect.Addr().Unmap()
}

// watchIdle tears the flow down once it has been quiet for the idle timeout.
// One ticker per flow replaces the previous per-packet timer resets.
func (f *udpFlow) watchIdle(done <-chan struct{}) {
	interval := f.idleTimeout / 4
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case now := <-t.C:
			if now.UnixNano()-f.lastSeen.Load() > int64(f.idleTimeout) {
				f.close()
				return
			}
		}
	}
}

// close unblocks both copy goroutines; ReadFrom only returns once its socket is
// closed.
func (f *udpFlow) close() {
	f.closeOnce.Do(func() {
		f.client.Close()
		f.backend.Close()
	})
}
