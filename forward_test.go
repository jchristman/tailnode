package main

import (
	"net/netip"
	"strings"
	"testing"

	"tailscale.com/tsnet"
)

func TestContainsAddr(t *testing.T) {
	routes := []netip.Prefix{
		netip.MustParsePrefix("192.168.0.0/24"),
		netip.MustParsePrefix("10.0.0.0/8"),
	}

	tests := []struct {
		addr netip.Addr
		want bool
	}{
		{netip.MustParseAddr("192.168.0.1"), true},
		{netip.MustParseAddr("192.168.0.255"), true},
		{netip.MustParseAddr("10.1.2.3"), true},
		{netip.MustParseAddr("172.16.0.1"), false},
		{netip.MustParseAddr("192.169.0.1"), false},
	}

	for _, tt := range tests {
		if got := containsAddr(routes, tt.addr); got != tt.want {
			t.Errorf("containsAddr(%v) = %v, want %v", tt.addr, got, tt.want)
		}
	}
}

func TestRegisterSubnetTCPForwarderRequiresUp(t *testing.T) {
	srv := &tsnet.Server{Hostname: "tailnode-test"}
	routes := []netip.Prefix{netip.MustParsePrefix("192.168.0.0/24")}

	err := registerSubnetTCPForwarder(srv, routes)
	if err == nil {
		t.Fatal("registerSubnetTCPForwarder before Up() should fail")
	}
	if !strings.Contains(err.Error(), "call after srv.Up()") {
		t.Fatalf("unexpected error: %v", err)
	}
}
