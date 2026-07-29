// tailnode joins a tailnet with tsnet and advertises subnet routes.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"tailscale.com/ipn"
	"tailscale.com/tsnet"
)

func main() {
	advertiseRoute := flag.String("advertise-route", "", "subnet route to advertise (CIDR), e.g. 192.168.0.0/24")
	preauthKey := flag.String("preauthkey", "", "Tailscale auth key (overrides .env)")
	envFile := flag.String("env-file", ".env", "path to .env with AUTH_KEY")
	hostname := flag.String("hostname", "tailnode", "hostname for this node in the tailnet")
	loginServer := flag.String("login-server", "", "control server URL (Headscale or custom; default: Tailscale)")
	stateDir := flag.String("state-dir", "", "directory for tsnet state (default: <user config dir>/"+defaultStateDirName+")")

	maxConcurrentDials := flag.Int64("max-concurrent-dials", autoLimit, "max parallel backend dials (-1 derives from the file descriptor limit, 0 = unlimited)")
	maxConns := flag.Int64("max-proxied-conns", autoLimit, "max concurrent proxied flows (-1 derives from the file descriptor limit, 0 = unlimited)")
	dialSlotWait := flag.Duration("dial-slot-wait", 100*time.Millisecond, "how long a new flow waits for a dial slot before being reset")
	backendDialTimeoutFlag := flag.Duration("backend-dial-timeout", defaultBackendDialTimeout, "timeout for backend TCP dials")
	idleTimeout := flag.Duration("idle-timeout", 0, "close proxied TCP flows idle for this long (0 disables; netstack keepalives still apply)")

	useCache := flag.Bool("reachability-cache", true, "cache backend reachability so repeat flows skip the dial on netstack's accept path")
	cacheTTL := flag.Duration("reachability-ttl", 10*time.Second, "how long a reachable backend stays cached")
	cacheNegTTL := flag.Duration("reachability-negative-ttl", 3*time.Second, "how long an unreachable backend stays cached")
	cacheSize := flag.Int("reachability-cache-size", 65536, "max entries in the reachability cache")

	preserveUDPPort := flag.Bool("udp-preserve-source-port", false, "bind backend UDP sockets to the client's source port (needs privileges below 1024)")
	metricsAddr := flag.String("metrics-addr", "", "serve metrics on this address, e.g. 127.0.0.1:9090 (empty disables)")
	verbose := flag.Bool("verbose", false, "log tsnet and netstack internals")
	flag.Parse()

	if *advertiseRoute == "" {
		log.Fatal("--advertise-route is required")
	}

	backendDialTimeout = *backendDialTimeoutFlag
	proxyIdleTimeout = *idleTimeout
	udpPreserveSourcePort = *preserveUDPPort

	eff := initLimits(*maxConcurrentDials, *maxConns, *dialSlotWait)
	if *useCache {
		reachCache = newReachabilityCache(*cacheTTL, *cacheNegTTL, *cacheSize)
	}
	initMetricGauges(reachCache)

	authKey, err := resolvePreauthKey(*preauthKey, *envFile)
	if err != nil {
		log.Fatalf("preauth key: %v", err)
	}

	routes, err := parseRoutes(*advertiseRoute)
	if err != nil {
		log.Fatalf("invalid --advertise-route: %v", err)
	}

	controlURL, err := resolveControlURL(*loginServer, *envFile)
	if err != nil {
		log.Fatalf("login server: %v", err)
	}

	srv := &tsnet.Server{
		Hostname: *hostname,
		AuthKey:  authKey,
	}
	if controlURL != "" {
		srv.ControlURL = controlURL
	}
	srv.Dir, err = resolveStateDir(*stateDir)
	if err != nil {
		log.Fatalf("state dir: %v", err)
	}
	if !*verbose {
		// tsnet and netstack are chatty per connection; at volume the logging
		// itself becomes the bottleneck.
		srv.Logf = func(string, ...any) {}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logLimits(eff)
	log.Printf("tsnet state: %s", srv.Dir)
	log.Printf("joining tailnet as %q, advertising %v", *hostname, routes)
	if controlURL != "" {
		log.Printf("using control server %q", controlURL)
	}

	status, err := srv.Up(ctx)
	if err != nil {
		log.Fatalf("failed to join tailnet: %v", err)
	}

	if err := registerSubnetForwarders(srv, routes); err != nil {
		log.Fatalf("subnet forwarder: %v", err)
	}

	// netstack's own counters are the only place SYNs dropped before our
	// handler are visible.
	if ns, err := netstackForServer(srv); err == nil {
		publishNetstackVars(ns.ExpVar())
	} else {
		log.Printf("netstack metrics unavailable: %v", err)
	}

	reachCache.startSweeper(ctx, *cacheTTL)
	startHandoffSweeper(ctx, handoffTTL/2)

	if *metricsAddr != "" {
		if err := serveMetrics(*metricsAddr); err != nil {
			log.Fatalf("metrics listener: %v", err)
		}
	}

	lc, err := srv.LocalClient()
	if err != nil {
		log.Fatalf("local client: %v", err)
	}

	if _, err := lc.EditPrefs(ctx, &ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			AdvertiseRoutes: routes,
		},
		AdvertiseRoutesSet: true,
	}); err != nil {
		log.Fatalf("advertise routes: %v", err)
	}

	log.Printf("connected; tailscale IPs: %v", status.TailscaleIPs)
	log.Printf("advertising routes: %v", routes)
	log.Printf("approve routes in the admin console if required, then use this node as a subnet router")
	log.Printf("on Linux, enable IP forwarding for physical subnet access: sysctl -w net.ipv4.ip_forward=1")
	log.Printf("running; press Ctrl+C to exit")

	<-ctx.Done()
	log.Printf("shutting down")
	srv.Close()
}

// defaultStateDirName is the fixed directory name holding the node's identity.
// It matches the name tsnet itself picks for a binary called "tailnode", so an
// existing deployment keeps its node key when it upgrades to this build.
const defaultStateDirName = "tsnet-tailnode"

// resolveStateDir returns the directory tsnet keeps the node key in.
//
// tsnet's own default is derived from filepath.Base(os.Args[0]), so running the
// same build under any other name — tailnode.new, tailnode-linux-amd64, a test
// copy — points it at an empty directory. tsnet then registers a brand new node
// with the auth key, which lands on a new tailnet IP and whose subnet routes
// are unapproved, so the route has to be approved again in the admin console.
// Pinning the path keeps the node's identity independent of how it is invoked.
func resolveStateDir(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	confDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir (set --state-dir): %w", err)
	}
	return filepath.Join(confDir, defaultStateDirName), nil
}

func parseRoutes(s string) ([]netip.Prefix, error) {
	var routes []netip.Prefix
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", part, err)
		}
		routes = append(routes, p)
	}
	if len(routes) == 0 {
		return nil, fmt.Errorf("no routes provided")
	}
	return routes, nil
}
