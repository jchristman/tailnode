package main

import (
	"io"
	"net"
	"net/netip"
	"time"

	"tailscale.com/tsnet"
)

// registerSubnetForwarder installs TCP and UDP handlers for traffic to advertised
// subnets. Call registerSubnetUDPForwarder after srv.Up().
//
// tsnet terminates unmatched TCP flows with RST by default; we dial the real LAN
// target instead (see tailscale/tailscale#8897). Unmatched UDP flows are dropped
// unless registerSubnetUDPForwarder hooks netstack after startup.
func registerSubnetForwarder(srv *tsnet.Server, routes []netip.Prefix) {
	srv.RegisterFallbackTCPHandler(func(src, dst netip.AddrPort) (func(net.Conn), bool) {
		if !containsAddr(routes, dst.Addr()) {
			return nil, false
		}
		return func(client net.Conn) {
			backend, err := net.DialTimeout("tcp", dst.String(), 30*time.Second)
			if err != nil {
				client.Close()
				return
			}
			proxyTCP(client, backend)
		}, true
	})
}

func containsAddr(routes []netip.Prefix, addr netip.Addr) bool {
	for _, p := range routes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

func proxyTCP(client, backend net.Conn) {
	defer client.Close()
	defer backend.Close()

	errc := make(chan error, 2)
	go func() { _, err := io.Copy(backend, client); errc <- err }()
	go func() { _, err := io.Copy(client, backend); errc <- err }()
	<-errc
	<-errc
}
