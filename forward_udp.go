package main

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"time"
	"unsafe"

	"tailscale.com/tsnet"
	"tailscale.com/types/nettype"
	"tailscale.com/wgengine/netstack"
)

// registerSubnetUDPForwarder installs a UDP handler for traffic to advertised
// subnets. tsnet drops unmatched UDP flows by default; there is no
// RegisterFallbackUDPHandler yet, so we chain onto netstack's handler after Up.
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

func netstackForServer(srv *tsnet.Server) (*netstack.Impl, error) {
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

	backendRemoteAddr := net.UDPAddrFromAddrPort(dstAddr)
	backendListenAddr := &net.UDPAddr{Port: int(clientAddr.Port())}
	if dstAddr.Addr().Is4() {
		backendListenAddr.IP = net.IPv4zero
	} else {
		backendListenAddr.IP = net.IPv6zero
	}

	backendConn, err := net.ListenUDP("udp", backendListenAddr)
	if err != nil {
		backendListenAddr.Port = 0
		backendConn, err = net.ListenUDP("udp", backendListenAddr)
		if err != nil {
			return
		}
	}
	defer backendConn.Close()

	idleTimeout := 2 * time.Minute
	if dstAddr.Port() == 53 {
		idleTimeout = 30 * time.Second
	}

	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(idleTimeout, func() {
		cancel()
		client.Close()
		backendConn.Close()
	})
	extend := func() { timer.Reset(idleTimeout) }

	startPacketCopy(ctx, cancel, client, net.UDPAddrFromAddrPort(clientAddr), backendConn, extend)
	startPacketCopy(ctx, cancel, backendConn, backendRemoteAddr, client, extend)
	<-ctx.Done()
}

func startPacketCopy(ctx context.Context, cancel context.CancelFunc, dst net.PacketConn, dstAddr net.Addr, src net.PacketConn, extend func()) {
	go func() {
		defer cancel()

		buf := make([]byte, 65535)
		for {
			select {
			case <-ctx.Done():
				return
			default:
				n, _, err := src.ReadFrom(buf)
				if err != nil {
					return
				}
				if _, err := dst.WriteTo(buf[:n], dstAddr); err != nil {
					return
				}
				extend()
			}
		}
	}()
}
