package main

import (
	"context"
	"net/netip"
	"sync"
	"time"
)

// reachShards is the shard count for the reachability cache. Lookups happen on
// netstack's connection-accept path, so the map is split to keep lock hold
// times short under concurrent flows.
const reachShards = 64

type reachEntry struct {
	open    bool
	expires time.Time
}

type reachShard struct {
	mu      sync.RWMutex
	entries map[netip.AddrPort]reachEntry
}

// reachabilityCache remembers whether a backend address recently accepted a
// connection, so the netstack callback can answer without dialing.
//
// A nil *reachabilityCache is valid and behaves as a permanent miss, which is
// how the cache is disabled.
type reachabilityCache struct {
	shards [reachShards]reachShard

	openTTL   time.Duration
	closedTTL time.Duration
	perShard  int

	now func() time.Time
}

func newReachabilityCache(openTTL, closedTTL time.Duration, maxEntries int) *reachabilityCache {
	perShard := maxEntries / reachShards
	if perShard < 1 {
		perShard = 1
	}
	c := &reachabilityCache{
		openTTL:   openTTL,
		closedTTL: closedTTL,
		perShard:  perShard,
		now:       time.Now,
	}
	for i := range c.shards {
		c.shards[i].entries = make(map[netip.AddrPort]reachEntry)
	}
	return c
}

func (c *reachabilityCache) shard(dst netip.AddrPort) *reachShard {
	a := dst.Addr().As16()
	h := uint32(2166136261)
	for _, b := range a[8:] {
		h = (h ^ uint32(b)) * 16777619
	}
	h = (h ^ uint32(dst.Port())) * 16777619
	return &c.shards[h%reachShards]
}

// get reports the cached state of dst. ok is false on a miss or expired entry.
func (c *reachabilityCache) get(dst netip.AddrPort) (open, ok bool) {
	if c == nil {
		return false, false
	}
	s := c.shard(dst)
	s.mu.RLock()
	e, found := s.entries[dst]
	s.mu.RUnlock()
	if !found || c.now().After(e.expires) {
		metricCacheMiss.Add(1)
		return false, false
	}
	metricCacheHit.Add(1)
	return e.open, true
}

// markOpen and markClosed are no-ops on a nil cache, which is how the cache is
// disabled. The nil check has to happen before the TTL field is read.
func (c *reachabilityCache) markOpen(dst netip.AddrPort) {
	if c == nil {
		return
	}
	c.put(dst, true, c.openTTL)
}

func (c *reachabilityCache) markClosed(dst netip.AddrPort) {
	if c == nil {
		return
	}
	c.put(dst, false, c.closedTTL)
}

func (c *reachabilityCache) put(dst netip.AddrPort, open bool, ttl time.Duration) {
	if c == nil || ttl <= 0 {
		return
	}
	now := c.now()
	s := c.shard(dst)

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.entries[dst]; !exists && len(s.entries) >= c.perShard {
		c.evictLocked(s, now)
	}
	s.entries[dst] = reachEntry{open: open, expires: now.Add(ttl)}
}

// evictLocked makes room in a full shard, dropping expired entries first and
// falling back to arbitrary ones. A wide port sweep inserts a unique entry per
// probe, so the cache has to shed entries rather than grow without bound.
func (c *reachabilityCache) evictLocked(s *reachShard, now time.Time) {
	for k, e := range s.entries {
		if now.After(e.expires) {
			delete(s.entries, k)
			metricCacheEviction.Add(1)
		}
	}
	// Map iteration order is randomized, so this sheds an arbitrary tenth of
	// the shard rather than repeatedly evicting the same keys.
	if len(s.entries) < c.perShard {
		return
	}
	drop := c.perShard/10 + 1
	for k := range s.entries {
		if drop == 0 {
			break
		}
		delete(s.entries, k)
		metricCacheEviction.Add(1)
		drop--
	}
}

// sweep drops expired entries across all shards.
func (c *reachabilityCache) sweep() {
	if c == nil {
		return
	}
	now := c.now()
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.Lock()
		for k, e := range s.entries {
			if now.After(e.expires) {
				delete(s.entries, k)
			}
		}
		s.mu.Unlock()
	}
}

func (c *reachabilityCache) len() int {
	if c == nil {
		return 0
	}
	n := 0
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.RLock()
		n += len(s.entries)
		s.mu.RUnlock()
	}
	return n
}

// startSweeper reclaims expired entries until ctx is done, so an idle node does
// not hold memory for flows that stopped long ago.
func (c *reachabilityCache) startSweeper(ctx context.Context, interval time.Duration) {
	if c == nil || interval <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.sweep()
			}
		}
	}()
}
