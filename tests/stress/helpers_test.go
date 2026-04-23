//go:build interop && stress

// Package stress holds opt-in scale / throughput / startup-ordering tests.
// Build with `-tags "interop stress"` (see Makefile `interop-stress` target).
// Tests skip rather than fail when zenohd is not reachable on the host.
package stress

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/zenoh"
)

const (
	hostEndpoint = "tcp/127.0.0.1:7447"
	// subPropagationDelay gives the router a window to propagate a freshly
	// declared entity back to the declaring session. 200 ms is comfortable
	// over localhost Docker; shorter values cause early-send drops. Name
	// matches the constant in tests/interop so a grep across both packages
	// lands on the same semantics.
	subPropagationDelay = 200 * time.Millisecond
	openTimeout         = 5 * time.Second
)

// requireZenohd skips the test unless tcp/127.0.0.1:7447 accepts a TCP dial.
func requireZenohd(t *testing.T) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", "127.0.0.1:7447", 2*time.Second)
	if err != nil {
		t.Skipf("zenohd not reachable at 127.0.0.1:7447 (run `make interop-up` first): %v", err)
	}
	_ = conn.Close()
}

// openSession connects to the host-exposed zenohd and applies any tweaks.
func openSession(t *testing.T, tweaks ...func(*zenoh.Config)) *zenoh.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), openTimeout)
	defer cancel()
	cfg := zenoh.NewConfig().WithEndpoint(hostEndpoint)
	for _, tweak := range tweaks {
		tweak(&cfg)
	}
	s, err := zenoh.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("zenoh.Open: %v", err)
	}
	return s
}
