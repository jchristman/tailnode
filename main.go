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
	"strings"
	"syscall"

	"tailscale.com/ipn"
	"tailscale.com/tsnet"
)

func main() {
	advertiseRoute := flag.String("advertise-route", "", "subnet route to advertise (CIDR), e.g. 192.168.0.0/24")
	preauthKey := flag.String("preauthkey", "", "Tailscale auth key (overrides .env)")
	envFile := flag.String("env-file", ".env", "path to .env with AUTH_KEY")
	hostname := flag.String("hostname", "tailnode", "hostname for this node in the tailnet")
	stateDir := flag.String("state-dir", "", "directory for tsnet state (default: OS user config dir)")
	flag.Parse()

	if *advertiseRoute == "" {
		log.Fatal("--advertise-route is required")
	}

	authKey, err := resolvePreauthKey(*preauthKey, *envFile)
	if err != nil {
		log.Fatalf("preauth key: %v", err)
	}

	routes, err := parseRoutes(*advertiseRoute)
	if err != nil {
		log.Fatalf("invalid --advertise-route: %v", err)
	}

	srv := &tsnet.Server{
		Hostname: *hostname,
		AuthKey:  authKey,
	}
	if *stateDir != "" {
		srv.Dir = *stateDir
	}

	registerSubnetForwarder(srv, routes)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("joining tailnet as %q, advertising %v", *hostname, routes)

	status, err := srv.Up(ctx)
	if err != nil {
		log.Fatalf("failed to join tailnet: %v", err)
	}

	if err := registerSubnetUDPForwarder(srv, routes); err != nil {
		log.Fatalf("udp forwarder: %v", err)
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
