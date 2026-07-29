package main

import (
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"
)

func TestReachabilityCacheHitAndMiss(t *testing.T) {
	c := newReachabilityCache(time.Minute, time.Minute, 1024)
	dst := netip.MustParseAddrPort("192.168.0.111:22")

	if _, ok := c.get(dst); ok {
		t.Fatal("empty cache should miss")
	}

	c.markOpen(dst)
	open, ok := c.get(dst)
	if !ok || !open {
		t.Fatalf("markOpen: got open=%v ok=%v, want true/true", open, ok)
	}

	c.markClosed(dst)
	open, ok = c.get(dst)
	if !ok || open {
		t.Fatalf("markClosed: got open=%v ok=%v, want false/true", open, ok)
	}
}

func TestReachabilityCacheExpiry(t *testing.T) {
	c := newReachabilityCache(10*time.Second, 2*time.Second, 1024)

	now := time.Now()
	c.now = func() time.Time { return now }

	open := netip.MustParseAddrPort("192.168.0.111:22")
	closed := netip.MustParseAddrPort("192.168.0.111:80")
	c.markOpen(open)
	c.markClosed(closed)

	// The negative TTL is shorter so a service that comes up is not blackholed.
	now = now.Add(3 * time.Second)
	if _, ok := c.get(closed); ok {
		t.Error("closed entry should have expired after its negative TTL")
	}
	if _, ok := c.get(open); !ok {
		t.Error("open entry should still be live")
	}

	now = now.Add(8 * time.Second)
	if _, ok := c.get(open); ok {
		t.Error("open entry should have expired")
	}
}

func TestReachabilityCacheNilIsDisabled(t *testing.T) {
	var c *reachabilityCache
	dst := netip.MustParseAddrPort("192.168.0.111:22")

	c.markOpen(dst)
	if _, ok := c.get(dst); ok {
		t.Fatal("nil cache should always miss")
	}
	if n := c.len(); n != 0 {
		t.Fatalf("nil cache len = %d, want 0", n)
	}
	c.sweep()
}

func TestReachabilityCacheBounded(t *testing.T) {
	// A wide port sweep inserts a unique entry per probe, so the cache has to
	// shed entries instead of growing without bound.
	const maxEntries = reachShards * 4
	c := newReachabilityCache(time.Hour, time.Hour, maxEntries)

	for port := 1; port <= 20000; port++ {
		c.markClosed(netip.MustParseAddrPort(fmt.Sprintf("192.168.0.111:%d", port)))
	}

	if got := c.len(); got > maxEntries {
		t.Fatalf("cache holds %d entries, want at most %d", got, maxEntries)
	}
}

func TestReachabilityCacheSweep(t *testing.T) {
	c := newReachabilityCache(time.Second, time.Second, 1024)
	now := time.Now()
	c.now = func() time.Time { return now }

	for port := 1; port <= 100; port++ {
		c.markOpen(netip.MustParseAddrPort(fmt.Sprintf("192.168.0.111:%d", port)))
	}
	if c.len() != 100 {
		t.Fatalf("len = %d, want 100", c.len())
	}

	now = now.Add(2 * time.Second)
	c.sweep()
	if c.len() != 0 {
		t.Fatalf("after sweep len = %d, want 0", c.len())
	}
}

func TestReachabilityCacheConcurrent(t *testing.T) {
	c := newReachabilityCache(time.Minute, time.Minute, 4096)

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for j := range 500 {
				dst := netip.MustParseAddrPort(fmt.Sprintf("192.168.0.111:%d", base*500+j+1))
				c.markOpen(dst)
				c.get(dst)
				c.sweep()
			}
		}(i)
	}
	wg.Wait()
}
