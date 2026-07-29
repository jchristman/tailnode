package main

import (
	"testing"
	"time"
)

func resetLimits(t *testing.T) {
	t.Helper()
	savedSem, savedWait, savedMax := dialSem, dialAcquireTimeout, maxProxiedConns
	savedLive := proxiedConns.Load()
	t.Cleanup(func() {
		dialSem, dialAcquireTimeout, maxProxiedConns = savedSem, savedWait, savedMax
		proxiedConns.Store(savedLive)
	})
}

func TestDeriveLimit(t *testing.T) {
	tests := []struct {
		budget  uint64
		divisor int64
		max     int64
		want    int64
	}{
		{65535, 4, 512, 512}, // clamped to the ceiling
		{1024, 4, 512, 256},  // derived from the budget
		{1024, 2, 4096, 512}, // derived from the budget
		{2, 4, 512, 1},       // never returns zero
		{1 << 20, 2, 4096, 4096},
	}
	for _, tt := range tests {
		if got := deriveLimit(tt.budget, tt.divisor, tt.max); got != tt.want {
			t.Errorf("deriveLimit(%d, %d, %d) = %d, want %d", tt.budget, tt.divisor, tt.max, got, tt.want)
		}
	}
}

func TestInitLimitsAutoDerives(t *testing.T) {
	resetLimits(t)

	eff := initLimits(autoLimit, autoLimit, 50*time.Millisecond)
	if eff.maxDials < 1 || eff.maxDials > autoMaxDials {
		t.Errorf("maxDials = %d, want between 1 and %d", eff.maxDials, autoMaxDials)
	}
	if eff.maxConns < 1 || eff.maxConns > autoMaxConns {
		t.Errorf("maxConns = %d, want between 1 and %d", eff.maxConns, autoMaxConns)
	}
	if eff.softFDs == 0 {
		t.Error("expected a non-zero descriptor limit")
	}
}

func TestInitLimitsExplicitAndUnlimited(t *testing.T) {
	resetLimits(t)

	eff := initLimits(8, 16, time.Millisecond)
	if eff.maxDials != 8 || eff.maxConns != 16 {
		t.Fatalf("got dials=%d conns=%d, want 8/16", eff.maxDials, eff.maxConns)
	}

	eff = initLimits(0, 0, time.Millisecond)
	if dialSem != nil {
		t.Error("a dial limit of 0 should disable the semaphore")
	}
	if eff.maxConns != 0 {
		t.Errorf("maxConns = %d, want 0 (unlimited)", eff.maxConns)
	}
}

// TestAcquireDialSlotBounded is the guard on the netstack accept path: stalling
// there holds a gVisor forwarder slot, and a full forwarder table makes netstack
// drop later SYNs with no RST.
func TestAcquireDialSlotBounded(t *testing.T) {
	resetLimits(t)
	initLimits(1, 0, 30*time.Millisecond)

	if !acquireDialSlot() {
		t.Fatal("first acquire should succeed")
	}

	start := time.Now()
	if acquireDialSlot() {
		t.Fatal("second acquire should fail while the only slot is held")
	}
	elapsed := time.Since(start)
	if elapsed > time.Second {
		t.Fatalf("acquire blocked for %s, want it bounded near the 30ms wait", elapsed)
	}

	releaseDialSlot()
	if !acquireDialSlot() {
		t.Fatal("acquire should succeed once the slot is released")
	}
	releaseDialSlot()
}

func TestAcquireConnSlotCeiling(t *testing.T) {
	resetLimits(t)
	initLimits(0, 2, time.Millisecond)
	proxiedConns.Store(0)

	if !acquireConnSlot() || !acquireConnSlot() {
		t.Fatal("should admit up to the ceiling")
	}
	if acquireConnSlot() {
		t.Fatal("should refuse past the ceiling")
	}
	if got := liveConns(); got != 2 {
		t.Fatalf("live conns = %d, want 2; a refused slot must not stay counted", got)
	}

	releaseConnSlot()
	if !acquireConnSlot() {
		t.Fatal("should admit again after a release")
	}
}

func TestAcquireConnSlotUnlimited(t *testing.T) {
	resetLimits(t)
	initLimits(0, 0, time.Millisecond)
	proxiedConns.Store(0)

	for i := range 1000 {
		if !acquireConnSlot() {
			t.Fatalf("unlimited ceiling refused slot %d", i)
		}
	}
}
