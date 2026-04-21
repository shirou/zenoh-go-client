package session

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// closedPortAddr returns a "host:port" that is guaranteed to refuse
// connections by opening a listener on an ephemeral port and closing it
// immediately; the kernel holds the port in TIME_WAIT / closed state so
// connect returns ECONNREFUSED without the long TCP connect timeout.
func closedPortAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestDialFirstPicksFirstReachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	eps := []string{
		"tcp/" + closedPortAddr(t), // refuses immediately
		"tcp/" + ln.Addr().String(),
	}
	link, err := DialFirst(ctx, eps)
	if err != nil {
		t.Fatalf("DialFirst: %v", err)
	}
	defer link.Close()
	if link.RemoteLocator().Address != ln.Addr().String() {
		t.Errorf("dialed %q, want %q", link.RemoteLocator().Address, ln.Addr().String())
	}
}

func TestDialFirstAllFail(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := DialFirst(ctx, []string{"tcp/" + closedPortAddr(t), "tcp/" + closedPortAddr(t)})
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(err.Error(), "all endpoints failed") {
		t.Errorf("want aggregated failure msg, got %v", err)
	}
}

func TestDialFirstEmpty(t *testing.T) {
	_, err := DialFirst(context.Background(), nil)
	if err == nil {
		t.Fatal("empty endpoints should fail")
	}
}

func TestDialFirstUnknownScheme(t *testing.T) {
	ctx := context.Background()
	_, err := DialFirst(ctx, []string{"bogus/x:1"})
	if err == nil {
		t.Fatal("expected unknown-scheme failure")
	}
}
