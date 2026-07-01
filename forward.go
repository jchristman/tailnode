package main

import (
	"io"
	"net"
	"net/netip"
	"time"

	"tailscale.com/tsnet"
)

const defaultBackendDialTimeout = 500 * time.Millisecond

var backendDialTimeout = defaultBackendDialTimeout

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
		if !containsAddr(routes, dst.Addr()) {
			return nil, true
		}
		acquireDialSlot()
		backend, err := net.DialTimeout("tcp", dst.String(), backendDialTimeout)
		releaseDialSlot()
		if err != nil {
			return nil, true
		}
		return func(client net.Conn) {
			proxyTCP(client, backend)
		}, true
	}
	return nil
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

func proxyTCP(client, backend net.Conn) {
	defer client.Close()
	defer backend.Close()

	errc := make(chan error, 2)
	go func() { _, err := io.Copy(backend, client); errc <- err }()
	go func() { _, err := io.Copy(client, backend); errc <- err }()
	<-errc
	<-errc
}
