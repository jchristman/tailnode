package main

import (
	"io"
	"net"
	"net/netip"
	"time"

	"tailscale.com/tsnet"
)

const backendDialTimeout = 2 * time.Second

// registerSubnetForwarder installs TCP and UDP handlers for traffic to advertised
// subnets. Call registerSubnetUDPForwarder after srv.Up().
//
// tsnet terminates unmatched TCP flows with RST by default; we dial the real LAN
// target instead (see tailscale/tailscale#8897). Unmatched UDP flows are dropped
// unless registerSubnetUDPForwarder hooks netstack after startup.
//
// For TCP, the backend is dialed before tsnet completes the client handshake.
// Otherwise scanners (e.g. nmap -sS) see SYN-ACK on every port because we used
// to accept first and only dial afterward.
func registerSubnetForwarder(srv *tsnet.Server, routes []netip.Prefix) {
	srv.RegisterFallbackTCPHandler(func(src, dst netip.AddrPort) (func(net.Conn), bool) {
		if !containsAddr(routes, dst.Addr()) {
			return nil, false
		}
		backend, err := net.DialTimeout("tcp", dst.String(), backendDialTimeout)
		if err != nil {
			return nil, true
		}
		return func(client net.Conn) {
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
