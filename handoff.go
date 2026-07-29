package main

import (
	"context"
	"net"
	"sync"
	"time"
)

// handoffTTL bounds how long a dialed backend waits to be claimed by its
// connection handler.
const handoffTTL = 10 * time.Second

// backendHandoff carries a backend connection from the netstack callback to the
// connection handler that will proxy it.
//
// netstack only invokes the handler once the client handshake completes; if
// CreateEndpoint fails it returns without calling us. A half-open SYN scan
// (nmap -sS) answers every SYN-ACK with a RST, so that path is common and an
// unclaimed backend would leak a socket per probe.
type backendHandoff struct {
	conn    net.Conn // guarded by handoffMu; nil once claimed or reaped
	expires time.Time
}

var (
	handoffMu sync.Mutex
	pending   = make(map[*backendHandoff]struct{})
)

func newBackendHandoff(conn net.Conn) *backendHandoff {
	h := &backendHandoff{conn: conn, expires: time.Now().Add(handoffTTL)}
	handoffMu.Lock()
	pending[h] = struct{}{}
	handoffMu.Unlock()
	return h
}

// claim transfers ownership of the backend to the caller. It returns nil if the
// handoff was already reaped, in which case the caller owns nothing.
func (h *backendHandoff) claim() net.Conn {
	handoffMu.Lock()
	defer handoffMu.Unlock()
	conn := h.conn
	h.conn = nil
	delete(pending, h)
	return conn
}

// sweepHandoffs closes backends whose handler never ran and releases the
// connection slot each was holding. It returns the number reaped.
func sweepHandoffs(now time.Time) int {
	handoffMu.Lock()
	var stale []net.Conn
	for h := range pending {
		if now.After(h.expires) {
			if h.conn != nil {
				stale = append(stale, h.conn)
				h.conn = nil
			}
			delete(pending, h)
		}
	}
	handoffMu.Unlock()

	for _, c := range stale {
		c.Close()
		releaseConnSlot()
		metricHandoffReaped.Add(1)
	}
	return len(stale)
}

func pendingHandoffs() int64 {
	handoffMu.Lock()
	defer handoffMu.Unlock()
	return int64(len(pending))
}

func startHandoffSweeper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
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
				sweepHandoffs(time.Now())
			}
		}
	}()
}
