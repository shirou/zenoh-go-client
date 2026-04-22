//go:build interop_multicast

package interop

import (
	"net"
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/zenoh"
)

// requireMulticastZenohd skips the test unless 127.0.0.1:7447 accepts TCP —
// the signal that our host-networked zenohd-multicast container is up.
func requireMulticastZenohd(t *testing.T) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", "127.0.0.1:7447", 2*time.Second)
	if err != nil {
		t.Skipf("multicast zenohd not reachable at 127.0.0.1:7447 (run `make interop-multicast-up` first): %v", err)
	}
	_ = conn.Close()
}

// TestScoutFindsZenohd broadcasts SCOUT on the default IPv4 group and expects
// at least one HELLO back from the host-networked zenohd container.
func TestScoutFindsZenohd(t *testing.T) {
	requireMulticastZenohd(t)

	cfg := zenoh.NewConfig()
	cfg.Scouting.MulticastMode = zenoh.MulticastAuto

	ch, err := zenoh.Scout(cfg, zenoh.NewFifoChannel[zenoh.Hello](8), &zenoh.ScoutOptions{
		TimeoutMs: 3000,
		What:      zenoh.WhatRouter,
	})
	if err != nil {
		t.Fatalf("Scout: %v", err)
	}

	var got []zenoh.Hello
	for h := range ch {
		got = append(got, h)
	}
	if len(got) == 0 {
		t.Fatal("no HELLO received from zenohd within timeout")
	}
	// Expect at least one router.
	seenRouter := false
	for _, h := range got {
		if h.WhatAmI() == zenoh.WhatAmIRouter && !h.ZId().IsZero() {
			seenRouter = true
		}
	}
	if !seenRouter {
		t.Fatalf("no Router HELLO with non-empty ZID in %v", got)
	}
}
