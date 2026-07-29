package main

import (
	"io"
	"net"
	"testing"
	"time"
)

// TestProxyTCPHalfClose checks that end-of-stream propagates through the proxy.
// A backend that reads to EOF before replying deadlocks if the client's FIN is
// swallowed, so this fails by timing out if half-close regresses.
func TestProxyTCPHalfClose(t *testing.T) {
	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backendLn.Close()

	go func() {
		c, err := backendLn.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		req, err := io.ReadAll(c) // returns only once the client's FIN arrives
		if err != nil {
			return
		}
		c.Write(append([]byte("got:"), req...))
	}()

	clientLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer clientLn.Close()

	userSide, err := net.Dial("tcp", clientLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer userSide.Close()

	proxySide, err := clientLn.Accept()
	if err != nil {
		t.Fatal(err)
	}

	backend, err := net.Dial("tcp", backendLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	go proxyTCP(proxySide, backend)

	if _, err := userSide.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := userSide.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}

	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		b, err := io.ReadAll(userSide)
		done <- result{b, err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("read reply: %v", got.err)
		}
		if string(got.data) != "got:hello" {
			t.Fatalf("reply = %q, want %q", got.data, "got:hello")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for reply; client FIN was not propagated to the backend")
	}
}

// TestProxyTCPBidirectional covers a plain echo exchange with no half-close.
func TestProxyTCPBidirectional(t *testing.T) {
	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backendLn.Close()

	go func() {
		c, err := backendLn.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()

	clientLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer clientLn.Close()

	userSide, err := net.Dial("tcp", clientLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer userSide.Close()

	proxySide, err := clientLn.Accept()
	if err != nil {
		t.Fatal(err)
	}
	backend, err := net.Dial("tcp", backendLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	go proxyTCP(proxySide, backend)

	payload := make([]byte, 256<<10) // larger than one pooled copy buffer
	for i := range payload {
		payload[i] = byte(i)
	}

	go func() {
		userSide.Write(payload)
		userSide.(*net.TCPConn).CloseWrite()
	}()

	userSide.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadAll(userSide)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if len(got) != len(payload) {
		t.Fatalf("echoed %d bytes, want %d", len(got), len(payload))
	}
	for i := range got {
		if got[i] != payload[i] {
			t.Fatalf("byte %d = %d, want %d", i, got[i], payload[i])
		}
	}
}

func TestHandoffClaim(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	// Counts are compared as deltas: the pending set is process-wide, so other
	// tests may legitimately have handoffs in flight.
	before := pendingHandoffs()

	h := newBackendHandoff(a)
	if got := pendingHandoffs(); got != before+1 {
		t.Fatalf("pending handoffs = %d, want %d; the handoff was not registered", got, before+1)
	}

	if got := h.claim(); got != a {
		t.Fatalf("claim returned %v, want the registered conn", got)
	}
	if got := h.claim(); got != nil {
		t.Fatal("second claim should return nil")
	}

	// Leaving the pending set is what stops the reaper from closing a backend
	// the handler is already proxying.
	if got := pendingHandoffs(); got != before {
		t.Fatalf("pending handoffs = %d, want %d; a claimed handoff must not stay reapable", got, before)
	}
}

// TestHandoffReap covers the case netstack skips the handler because the client
// handshake failed, which a half-open SYN scan triggers on every probe.
func TestHandoffReap(t *testing.T) {
	saved := maxProxiedConns
	maxProxiedConns = 0
	defer func() { maxProxiedConns = saved }()

	acquireConnSlot()
	before := liveConns()

	a, b := net.Pipe()
	defer b.Close()

	h := newBackendHandoff(a)
	h.expires = time.Now().Add(-time.Second)

	if got := sweepHandoffs(time.Now()); got != 1 {
		t.Fatalf("swept %d handoffs, want 1", got)
	}
	if got := h.claim(); got != nil {
		t.Fatal("claim after reap should return nil")
	}
	if got := liveConns(); got != before-1 {
		t.Fatalf("live conns = %d, want %d; the reaper must release the slot", got, before-1)
	}
	// The reaped backend is closed.
	if _, err := a.Write([]byte("x")); err == nil {
		t.Fatal("reaped backend should be closed")
	}
}
