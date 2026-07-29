package main

import (
	"errors"
	"expvar"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"
)

// counter is an expvar.Int that also exposes itself in Prometheus text format.
type counter struct {
	name string
	help string
	v    *expvar.Int
}

func (c *counter) Add(n int64)  { c.v.Add(n) }
func (c *counter) Value() int64 { return c.v.Value() }

// gauge reports a value sampled at scrape time.
type gauge struct {
	name string
	help string
	f    func() int64
}

var (
	registryMu sync.Mutex
	counters   []*counter
	gauges     []*gauge
)

func newCounter(name, help string) *counter {
	c := &counter{name: name, help: help, v: expvar.NewInt(name)}
	registryMu.Lock()
	counters = append(counters, c)
	registryMu.Unlock()
	return c
}

func newGauge(name, help string, f func() int64) *gauge {
	g := &gauge{name: name, help: help, f: f}
	expvar.Publish(name, expvar.Func(func() any { return f() }))
	registryMu.Lock()
	gauges = append(gauges, g)
	registryMu.Unlock()
	return g
}

// Forwarding counters. These distinguish the failure modes that otherwise look
// identical to a client: backend refusals, backend timeouts, and our own
// capacity limits.
var (
	metricDialsTotal      = newCounter("tailnode_tcp_dials_total", "backend TCP dials attempted")
	metricDialsOK         = newCounter("tailnode_tcp_dials_ok", "backend TCP dials that connected")
	metricDialFailTimeout = newCounter("tailnode_tcp_dial_fail_timeout", "backend TCP dials that timed out")
	metricDialFailRefused = newCounter("tailnode_tcp_dial_fail_refused", "backend TCP dials refused by the target")
	metricDialFailOther   = newCounter("tailnode_tcp_dial_fail_other", "backend TCP dials that failed for other reasons")

	metricDialSlotTimeout = newCounter("tailnode_tcp_dial_slot_timeout", "flows reset because no dial slot was free in time")
	metricConnLimited     = newCounter("tailnode_tcp_conn_limit_refused", "flows reset because the proxied connection ceiling was reached")

	metricResetRefused = newCounter("tailnode_tcp_reset_refused",
		"flows reset because the target refused the connection; the client's \"closed\" verdict is accurate")
	metricFlowsDropped = newCounter("tailnode_tcp_flows_dropped",
		"flows dropped without a reply because the backend was unreachable for a reason that does not "+
			"prove the port is closed: a filtered target, or our own dial or connection ceiling")

	metricConnsAccepted = newCounter("tailnode_tcp_conns_accepted", "client flows accepted for proxying")
	metricLazyDialFail  = newCounter("tailnode_tcp_lazy_dial_fail", "accepted flows dropped because the deferred backend dial failed")
	metricHandoffReaped = newCounter("tailnode_tcp_handoff_reaped", "backends closed because the client handshake never completed")

	metricBytesToBackend = newCounter("tailnode_tcp_bytes_to_backend", "bytes copied from client to backend")
	metricBytesToClient  = newCounter("tailnode_tcp_bytes_to_client", "bytes copied from backend to client")

	metricCacheHit      = newCounter("tailnode_reachability_hit", "reachability cache hits")
	metricCacheMiss     = newCounter("tailnode_reachability_miss", "reachability cache misses")
	metricCacheEviction = newCounter("tailnode_reachability_eviction", "reachability cache entries evicted")

	metricUDPFlows     = newCounter("tailnode_udp_flows_total", "UDP flows started")
	metricUDPFlowFail  = newCounter("tailnode_udp_flow_fail", "UDP flows that could not bind a backend socket")
	metricUDPLimited   = newCounter("tailnode_udp_flow_limit_refused", "UDP flows refused by the connection ceiling")
	metricUDPSpoofDrop = newCounter("tailnode_udp_spoofed_drop", "UDP packets dropped for an unexpected source address")
)

// recordDialFailure classifies a backend dial error so operators can tell a
// saturated path (timeouts) from a closed port (refusals).
func recordDialFailure(err error) {
	var netErr net.Error
	switch {
	case errors.As(err, &netErr) && netErr.Timeout():
		metricDialFailTimeout.Add(1)
	case errors.Is(err, syscall.ECONNREFUSED):
		metricDialFailRefused.Add(1)
	default:
		metricDialFailOther.Add(1)
	}
}

// initMetricGauges registers sampled values. Called once limits are known.
func initMetricGauges(cache *reachabilityCache) {
	newGauge("tailnode_tcp_conns_live", "proxied flows currently open", liveConns)
	newGauge("tailnode_tcp_conns_limit", "ceiling on concurrent proxied flows", func() int64 { return maxProxiedConns })
	newGauge("tailnode_reachability_entries", "entries held in the reachability cache", func() int64 {
		return int64(cache.len())
	})
	newGauge("tailnode_tcp_handoff_pending", "backends dialed but not yet claimed by a handler", pendingHandoffs)
}

// publishNetstackVars exposes netstack's own counters, which are the only way
// to see SYNs that gVisor dropped before reaching our handler
// (counter_tcp_forward_max_in_flight_drop) and flows rejected by the per-client
// in-flight cap (counter_tcp_forward_max_in_flight_per_client_drop).
func publishNetstackVars(v expvar.Var) {
	defer func() {
		// expvar panics if the name is already taken.
		_ = recover()
	}()
	expvar.Publish("netstack", v)
}

// serveMetrics starts the metrics listener. /metrics is Prometheus text for our
// counters; /debug/vars is the full expvar tree including netstack.
func serveMetrics(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", writePrometheus)
	mux.Handle("/debug/vars", expvar.Handler())
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "tailnode\n\n/metrics\n/debug/vars\n")
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("metrics server: %v", err)
		}
	}()
	log.Printf("metrics on http://%s/metrics", ln.Addr())
	return nil
}

func writePrometheus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var b strings.Builder

	registryMu.Lock()
	cs := append([]*counter(nil), counters...)
	gs := append([]*gauge(nil), gauges...)
	registryMu.Unlock()

	for _, c := range cs {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", c.name, c.help, c.name, c.name, c.v.Value())
	}
	for _, g := range gs {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", g.name, g.help, g.name, g.name, g.f())
	}
	io.WriteString(w, b.String())
}

// logLimits records the descriptor budget and derived ceilings, so a node that
// is silently capped by RLIMIT_NOFILE is diagnosable from the log alone.
func logLimits(eff effectiveLimits) {
	if eff.fdWarning != "" {
		log.Printf("could not read file descriptor limit: %s", eff.fdWarning)
	}
	log.Printf("limits: fds soft=%d hard=%s, max concurrent dials=%s, max proxied conns=%s, dial slot wait=%s",
		eff.softFDs, limitStr(int64(eff.hardFDs)), limitStr(eff.maxDials), limitStr(eff.maxConns), eff.dialWait)
	if eff.softFDs < 4096 {
		log.Printf("warning: file descriptor limit %d is low for high-volume forwarding; "+
			"each proxied flow uses one descriptor (see deploy/tailnode.service)", eff.softFDs)
	}
}

func limitStr(n int64) string {
	if n <= 0 {
		return "unlimited"
	}
	if uint64(n) >= uint64(1)<<62 {
		return "unlimited"
	}
	return fmt.Sprint(n)
}
