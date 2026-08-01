package main

import (
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// flowAction is the netstack hook shape returned by tcpHandlerForFlow.
type flowAction int

const (
	flowActionProxy flowAction = iota
	flowActionReset
	flowActionDrop
)

func classifyHook(h func(net.Conn), intercept, sendReset bool) flowAction {
	if !intercept {
		panic("tcpHandlerForFlow always intercepts routed decisions")
	}
	if h != nil {
		return flowActionProxy
	}
	if sendReset {
		return flowActionReset
	}
	return flowActionDrop
}

func testRoutes(t *testing.T) []netip.Prefix {
	t.Helper()
	return []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
}

// echoBackend starts a listener that echoes and returns its address.
func echoBackend(t *testing.T) netip.AddrPort {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()
	return netip.MustParseAddrPort(ln.Addr().String())
}

// closedPort returns an address with nothing listening.
func closedPort(t *testing.T) netip.AddrPort {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := netip.MustParseAddrPort(ln.Addr().String())
	ln.Close()
	return addr
}

func setupFlowTest(t *testing.T) {
	t.Helper()
	resetLimits(t)
	initLimits(0, 0, 100*time.Millisecond)
	proxiedConns.Store(0)

	savedCache := reachCache
	reachCache = newReachabilityCache(10*time.Second, 3*time.Second, 1024)
	t.Cleanup(func() { reachCache = savedCache })

	// classifyFlow parks a dialed backend in the global handoff registry for the
	// handler to claim. A test that inspects the verdict without running the
	// handler would otherwise leak the socket and its connection slot into
	// whichever test runs next.
	t.Cleanup(func() { sweepHandoffs(time.Now().Add(2 * handoffTTL)) })
}

func TestFlowOutsideRouteIsReset(t *testing.T) {
	setupFlowTest(t)

	h, intercept, sendReset := tcpHandlerForFlow(testRoutes(t), netip.MustParseAddrPort("10.0.0.1:22"))
	if got := classifyHook(h, intercept, sendReset); got != flowActionReset {
		t.Fatalf("off-route flow: got handler=%v action=%v, want nil/reset", h != nil, got)
	}
}

// TestFlowClosedPortResetsAndCaches is the scan-accuracy guarantee: a closed
// port must be reset rather than handshaked, and the result is remembered so
// the next probe skips the dial entirely.
func TestFlowClosedPortResetsAndCaches(t *testing.T) {
	setupFlowTest(t)
	dst := closedPort(t)

	h, intercept, sendReset := tcpHandlerForFlow(testRoutes(t), dst)
	if classifyHook(h, intercept, sendReset) != flowActionReset {
		t.Fatalf("closed port: got action=%v handler=%v, want nil/reset", classifyHook(h, intercept, sendReset), h != nil)
	}

	open, cached := reachCache.get(dst)
	if !cached || open {
		t.Fatalf("closed port should be cached as closed; got open=%v cached=%v", open, cached)
	}

	// The cached answer must not leave a connection slot held.
	if got := liveConns(); got != 0 {
		t.Fatalf("live conns = %d after a refused dial, want 0", got)
	}

	before := metricDialsTotal.Value()
	if h, intercept, sendReset := tcpHandlerForFlow(testRoutes(t), dst); classifyHook(h, intercept, sendReset) != flowActionReset {
		t.Fatal("cached closed port should still reset")
	}
	if got := metricDialsTotal.Value(); got != before {
		t.Error("a cached closed port must not dial the backend again")
	}
}

func TestFlowOpenPortProxies(t *testing.T) {
	setupFlowTest(t)
	dst := echoBackend(t)

	h, intercept, sendReset := tcpHandlerForFlow(testRoutes(t), dst)
	if classifyHook(h, intercept, sendReset) != flowActionProxy {
		t.Fatalf("open port: got action=%v handler=%v, want handler/handle", classifyHook(h, intercept, sendReset), h != nil)
	}
	if open, cached := reachCache.get(dst); !cached || !open {
		t.Fatalf("open port should be cached as open; got open=%v cached=%v", open, cached)
	}

	assertEchoThroughHandler(t, h)
}

// TestFlowCachedOpenSkipsCallbackDial covers the throughput fix: once a backend
// is known reachable the accept callback must not dial, because it runs while
// netstack holds a forwarder slot.
func TestFlowCachedOpenSkipsCallbackDial(t *testing.T) {
	setupFlowTest(t)
	dst := echoBackend(t)
	reachCache.markOpen(dst)

	before := metricDialsTotal.Value()
	h, intercept, sendReset := tcpHandlerForFlow(testRoutes(t), dst)
	if classifyHook(h, intercept, sendReset) != flowActionProxy {
		t.Fatal("cached-open flow should be accepted")
	}
	if got := metricDialsTotal.Value(); got != before {
		t.Fatalf("callback dialed %d times on a cache hit, want 0", got-before)
	}
	if got := liveConns(); got != 0 {
		t.Fatalf("live conns = %d before the handler ran, want 0", got)
	}

	// The deferred dial happens when the handler runs.
	assertEchoThroughHandler(t, h)
	if metricDialsTotal.Value() <= before {
		t.Error("handler should have dialed the backend")
	}
}

// TestFlowConnCeilingDrops covers a capacity limit of our own. Resetting would
// tell the client the port is closed, turning a busy moment on this node into a
// wrong answer about the target.
func TestFlowConnCeilingDrops(t *testing.T) {
	setupFlowTest(t)
	initLimits(0, 1, 10*time.Millisecond)
	proxiedConns.Store(0)
	dst := echoBackend(t)

	// Occupy the only slot.
	if !acquireConnSlot() {
		t.Fatal("expected the first slot to be free")
	}
	defer releaseConnSlot()

	h, intercept, sendReset := tcpHandlerForFlow(testRoutes(t), dst)
	if classifyHook(h, intercept, sendReset) != flowActionDrop {
		t.Fatalf("at the ceiling: got action=%v handler=%v, want nil/drop", classifyHook(h, intercept, sendReset), h != nil)
	}
	if metricConnLimited.Value() == 0 {
		t.Error("expected the connection-limit counter to record the refusal")
	}
}

func TestFlowDialSlotExhaustionDrops(t *testing.T) {
	setupFlowTest(t)
	initLimits(1, 0, 10*time.Millisecond)
	proxiedConns.Store(0)
	dst := echoBackend(t)

	// Hold the only dial slot so the callback cannot get one.
	if !acquireDialSlot() {
		t.Fatal("expected the first dial slot to be free")
	}
	defer releaseDialSlot()

	before := metricDialSlotTimeout.Value()
	start := time.Now()
	h, intercept, sendReset := tcpHandlerForFlow(testRoutes(t), dst)
	elapsed := time.Since(start)

	if classifyHook(h, intercept, sendReset) != flowActionDrop {
		t.Fatalf("dial slots exhausted: got action=%v handler=%v, want nil/drop", classifyHook(h, intercept, sendReset), h != nil)
	}
	if elapsed > time.Second {
		t.Fatalf("callback blocked for %s; it must stay bounded to avoid filling netstack's forwarder table", elapsed)
	}
	if metricDialSlotTimeout.Value() == before {
		t.Error("expected the dial-slot timeout counter to increment")
	}
	if got := liveConns(); got != 0 {
		t.Fatalf("live conns = %d, want 0; the connection slot must be returned", got)
	}
}

type fakeTimeoutError struct{}

func (fakeTimeoutError) Error() string   { return "i/o timeout" }
func (fakeTimeoutError) Timeout() bool   { return true }
func (fakeTimeoutError) Temporary() bool { return true }

func TestIsDefinitiveRefusal(t *testing.T) {
	refused := &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	if !isDefinitiveRefusal(refused) {
		t.Error("ECONNREFUSED should be treated as definitive")
	}

	timedOut := &net.OpError{Op: "dial", Err: fakeTimeoutError{}}
	if isDefinitiveRefusal(timedOut) {
		t.Error("a timeout is ambiguous and must not be treated as definitive")
	}

	// Measured against a filtering host: EHOSTUNREACH landed on ~2.7% of dials
	// to ports that were only filtered, so it cannot be cached as closed.
	for _, errno := range []syscall.Errno{syscall.EHOSTUNREACH, syscall.ENETUNREACH} {
		unreach := &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", errno)}
		if isDefinitiveRefusal(unreach) {
			t.Errorf("%v is raised transiently under churn and must not be definitive", errno)
		}
	}
}

// TestFlowTimeoutIsNotCached protects scan accuracy: a saturated target that
// times out must be re-dialed on the next flow rather than remembered as
// closed, otherwise one stall turns into wrong answers for the whole TTL.
func TestFlowTimeoutIsNotCached(t *testing.T) {
	setupFlowTest(t)

	savedDial := dialBackend
	dialBackend = func(netip.AddrPort) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Err: fakeTimeoutError{}}
	}
	t.Cleanup(func() { dialBackend = savedDial })

	dst := netip.MustParseAddrPort("127.0.0.1:9")
	routes := testRoutes(t)

	h, intercept, sendReset := tcpHandlerForFlow(routes, dst)
	if classifyHook(h, intercept, sendReset) != flowActionDrop {
		t.Fatalf("timed-out dial: got action=%v handler=%v, want nil/drop", classifyHook(h, intercept, sendReset), h != nil)
	}
	if _, cached := reachCache.get(dst); cached {
		t.Fatal("an ambiguous timeout must not be cached")
	}

	// The next flow re-dials rather than answering from cache.
	before := metricDialsTotal.Value()
	tcpHandlerForFlow(routes, dst)
	if metricDialsTotal.Value() != before+1 {
		t.Error("expected the next flow to re-dial after an ambiguous failure")
	}
	if got := liveConns(); got != 0 {
		t.Fatalf("live conns = %d, want 0", got)
	}
}

// TestFlowRefusedIsCached is the counterpart: a refusal is definitive, so it is
// remembered and the next flow is reset without dialing.
func TestFlowRefusedIsCached(t *testing.T) {
	setupFlowTest(t)

	savedDial := dialBackend
	dialBackend = func(netip.AddrPort) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	}
	t.Cleanup(func() { dialBackend = savedDial })

	dst := netip.MustParseAddrPort("127.0.0.1:9")
	routes := testRoutes(t)

	tcpHandlerForFlow(routes, dst)
	open, cached := reachCache.get(dst)
	if !cached || open {
		t.Fatalf("a refusal should be cached as closed; got open=%v cached=%v", open, cached)
	}

	before := metricDialsTotal.Value()
	tcpHandlerForFlow(routes, dst)
	if metricDialsTotal.Value() != before {
		t.Error("a cached refusal must not dial again")
	}
}

// TestClassifyFlowVerdicts pins which failure modes may reset. A reset tells
// the client the port is closed, so only a refusal from the target earns one;
// filtering, unreachable errors, and our own capacity ceilings are all
// ambiguous and must not be reported as closed.
func TestClassifyFlowVerdicts(t *testing.T) {
	routes := testRoutes(t)
	unreachable := &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)}
	refused := &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}

	tests := []struct {
		name  string
		setup func(t *testing.T) netip.AddrPort
		want  flowVerdict
	}{
		{
			name: "off route resets",
			setup: func(*testing.T) netip.AddrPort {
				return netip.MustParseAddrPort("10.0.0.1:22")
			},
			want: verdictReset,
		},
		{
			name:  "refused resets",
			setup: func(t *testing.T) netip.AddrPort { return stubDial(t, refused) },
			want:  verdictReset,
		},
		{
			name:  "timeout drops",
			setup: func(t *testing.T) netip.AddrPort { return stubDial(t, fakeTimeoutError{}) },
			want:  verdictDrop,
		},
		{
			name:  "unreachable drops",
			setup: func(t *testing.T) netip.AddrPort { return stubDial(t, unreachable) },
			want:  verdictDrop,
		},
		{
			name: "conn ceiling drops",
			setup: func(t *testing.T) netip.AddrPort {
				initLimits(0, 1, 10*time.Millisecond)
				proxiedConns.Store(0)
				if !acquireConnSlot() {
					t.Fatal("expected the only connection slot to be free")
				}
				t.Cleanup(releaseConnSlot)
				return echoBackend(t)
			},
			want: verdictDrop,
		},
		{
			name: "dial slot exhaustion drops",
			setup: func(t *testing.T) netip.AddrPort {
				initLimits(1, 0, 10*time.Millisecond)
				proxiedConns.Store(0)
				if !acquireDialSlot() {
					t.Fatal("expected the only dial slot to be free")
				}
				t.Cleanup(releaseDialSlot)
				return echoBackend(t)
			},
			want: verdictDrop,
		},
		{
			name:  "reachable proxies",
			setup: func(t *testing.T) netip.AddrPort { return echoBackend(t) },
			want:  verdictProxy,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupFlowTest(t)
			dst := tt.setup(t)

			handler, got := classifyFlow(routes, dst)
			if got != tt.want {
				t.Errorf("verdict = %v, want %v", got, tt.want)
			}
			if (handler != nil) != (tt.want == verdictProxy) {
				t.Errorf("handler present = %v, want %v", handler != nil, tt.want == verdictProxy)
			}
		})
	}
}

// stubDial makes the backend dial fail with err and returns a routed address.
func stubDial(t *testing.T, err error) netip.AddrPort {
	t.Helper()
	saved := dialBackend
	dialBackend = func(netip.AddrPort) (net.Conn, error) { return nil, err }
	t.Cleanup(func() { dialBackend = saved })
	return netip.MustParseAddrPort("127.0.0.1:9")
}

// TestDroppedFlowIsCounted covers the counter an operator watches to see how
// much traffic could not be answered definitively.
func TestDroppedFlowIsCounted(t *testing.T) {
	setupFlowTest(t)
	dst := stubDial(t, fakeTimeoutError{})

	before := metricFlowsDropped.Value()
	h, intercept, sendReset := tcpHandlerForFlow(testRoutes(t), dst)
	if classifyHook(h, intercept, sendReset) != flowActionDrop {
		t.Fatalf("got action=%v handler=%v, want nil/drop", classifyHook(h, intercept, sendReset), h != nil)
	}
	if got := metricFlowsDropped.Value(); got != before+1 {
		t.Errorf("dropped flows = %d, want %d", got, before+1)
	}
}

// TestTCPHandlerForFlowDropShape is the contract our Tailscale fork relies on:
// a drop must be (nil, intercept=true, sendReset=false) so acceptTCP calls
// Complete(false) instead of sending RST.
func TestTCPHandlerForFlowDropShape(t *testing.T) {
	setupFlowTest(t)
	dst := stubDial(t, fakeTimeoutError{})

	h, intercept, sendReset := tcpHandlerForFlow(testRoutes(t), dst)
	if h != nil || !intercept || sendReset {
		t.Fatalf("drop shape = (%v, %v, %v), want (nil, true, false)", h != nil, intercept, sendReset)
	}
}

// TestTCPHandlerForFlowResetShape is the counterpart: a refusal must still RST.
func TestTCPHandlerForFlowResetShape(t *testing.T) {
	setupFlowTest(t)
	dst := stubDial(t, &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)})

	h, intercept, sendReset := tcpHandlerForFlow(testRoutes(t), dst)
	if h != nil || !intercept || !sendReset {
		t.Fatalf("reset shape = (%v, %v, %v), want (nil, true, true)", h != nil, intercept, sendReset)
	}
}

// TestFlowCachedPathIsConcurrent is the regression guard for the original
// bottleneck. The accept callback holds a gVisor forwarder slot, so a burst of
// flows to a known backend must be decided without any of them blocking on a
// dial or on each other.
func TestFlowCachedPathIsConcurrent(t *testing.T) {
	setupFlowTest(t)
	// One dial slot, and it is held for the whole test. If the cached path
	// touched the dial limiter, every flow below would stall for the full wait.
	initLimits(1, 0, 5*time.Second)
	proxiedConns.Store(0)
	if !acquireDialSlot() {
		t.Fatal("expected the dial slot to be free")
	}
	defer releaseDialSlot()

	dst := echoBackend(t)
	reachCache.markOpen(dst)

	const flows = 500
	routes := testRoutes(t)

	var wg sync.WaitGroup
	var accepted atomic.Int64
	start := time.Now()
	for range flows {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if h, intercept, sendReset := tcpHandlerForFlow(routes, dst); classifyHook(h, intercept, sendReset) == flowActionProxy {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	if got := accepted.Load(); got != flows {
		t.Fatalf("accepted %d of %d flows, want all", got, flows)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("%d cached decisions took %s; the callback is serializing", flows, elapsed)
	}
	t.Logf("%d cached accept decisions in %s", flows, elapsed)
}

func BenchmarkTCPHandlerForFlowCached(b *testing.B) {
	saved := reachCache
	reachCache = newReachabilityCache(time.Hour, time.Hour, 4096)
	defer func() { reachCache = saved }()

	routes := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	dst := netip.MustParseAddrPort("127.0.0.1:9")
	reachCache.markOpen(dst)

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if h, _, _ := tcpHandlerForFlow(routes, dst); h == nil {
				b.Fatal("expected a handler")
			}
		}
	})
}

// assertEchoThroughHandler runs the handler against a real client socket and
// verifies bytes make the round trip.
func assertEchoThroughHandler(t *testing.T, h func(net.Conn)) {
	t.Helper()

	clientLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer clientLn.Close()

	userSide, err := net.Dial("tcp", clientLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer userSide.Close()

	proxySide, err := clientLn.Accept()
	if err != nil {
		t.Fatal(err)
	}

	go h(proxySide)

	if _, err := userSide.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	userSide.(*net.TCPConn).CloseWrite()

	userSide.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(userSide)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(got) != "ping" {
		t.Fatalf("echo = %q, want %q", got, "ping")
	}

	// The handler releases its slot once the flow ends.
	deadline := time.Now().Add(2 * time.Second)
	for liveConns() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := liveConns(); got != 0 {
		t.Fatalf("live conns = %d after the flow closed, want 0", got)
	}
}
