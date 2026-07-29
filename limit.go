package main

import (
	"context"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"
)

// autoLimit selects a limit derived from the file descriptor budget.
const autoLimit = -1

// fdBudgetCap bounds the descriptor budget used for auto-derived limits so an
// unlimited RLIMIT_NOFILE does not produce nonsensical ceilings.
const fdBudgetCap = 1 << 20

// Ceilings for auto-derived limits. Beyond these, netstack's own in-flight
// caps (8192 global, ~5461 per client on Linux) bind first.
const (
	autoMaxDials = 512
	autoMaxConns = 4096
)

var (
	dialSem            *semaphore.Weighted
	dialAcquireTimeout time.Duration

	proxiedConns    atomic.Int64
	maxProxiedConns int64
)

// effectiveLimits reports the limits in force, for startup logging.
type effectiveLimits struct {
	softFDs   uint64
	hardFDs   uint64
	maxDials  int64
	maxConns  int64
	dialWait  time.Duration
	fdWarning string
}

// initLimits raises the descriptor limit and installs the dial and connection
// ceilings. A limit of autoLimit derives from the descriptor budget; 0 means
// unlimited.
func initLimits(maxDials, maxConns int64, dialWait time.Duration) effectiveLimits {
	soft, hard, err := raiseFileLimit()
	eff := effectiveLimits{softFDs: soft, hardFDs: hard}
	if err != nil {
		eff.fdWarning = err.Error()
	}

	budget := soft
	if budget > fdBudgetCap {
		budget = fdBudgetCap
	}

	if maxDials == autoLimit {
		maxDials = deriveLimit(budget, 4, autoMaxDials)
	}
	if maxConns == autoLimit {
		maxConns = deriveLimit(budget, 2, autoMaxConns)
	}

	if maxDials > 0 {
		dialSem = semaphore.NewWeighted(maxDials)
	} else {
		dialSem = nil
	}
	maxProxiedConns = maxConns
	dialAcquireTimeout = dialWait

	eff.maxDials = maxDials
	eff.maxConns = maxConns
	eff.dialWait = dialWait
	return eff
}

// deriveLimit returns budget/divisor clamped to [1, max].
func deriveLimit(budget uint64, divisor, max int64) int64 {
	n := int64(budget / uint64(divisor))
	if n > max {
		return max
	}
	if n < 1 {
		return 1
	}
	return n
}

// acquireDialSlot reserves a backend dial slot, waiting at most
// dialAcquireTimeout. It reports whether a slot was obtained.
//
// Callers on netstack's GetTCPHandlerForFlow path hold a gVisor forwarder slot
// and a per-client in-flight slot until they return, so the wait is bounded:
// stalling there exhausts netstack's forwarder table, which drops later SYNs
// silently instead of resetting them.
func acquireDialSlot() bool {
	if dialSem == nil {
		return true
	}
	// Fast path: avoid allocating a timer when a slot is free.
	if dialSem.TryAcquire(1) {
		return true
	}
	if dialAcquireTimeout <= 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), dialAcquireTimeout)
	defer cancel()
	return dialSem.Acquire(ctx, 1) == nil
}

// acquireDialSlotBlocking reserves a dial slot without a deadline. Only safe
// off netstack's critical path, i.e. once the connection handler is running.
func acquireDialSlotBlocking() {
	if dialSem != nil {
		_ = dialSem.Acquire(context.Background(), 1)
	}
}

func releaseDialSlot() {
	if dialSem != nil {
		dialSem.Release(1)
	}
}

// acquireConnSlot reserves capacity for one proxied flow. Each flow holds a
// host descriptor for its lifetime, so this is the ceiling that keeps a busy
// node from running out of descriptors.
func acquireConnSlot() bool {
	n := proxiedConns.Add(1)
	if maxProxiedConns > 0 && n > maxProxiedConns {
		proxiedConns.Add(-1)
		return false
	}
	return true
}

func releaseConnSlot() {
	proxiedConns.Add(-1)
}

func liveConns() int64 { return proxiedConns.Load() }
