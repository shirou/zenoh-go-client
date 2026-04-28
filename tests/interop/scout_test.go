//go:build interop_multicast

package interop

import (
	"testing"

	"github.com/shirou/zenoh-go-client/zenoh"
)

// requireMulticastZenohd skips the test unless a SCOUT round-trip to
// the multicast group succeeds. A plain TCP probe to 127.0.0.1:7447
// can't tell zenohd-multicast (host networking, multicast reachable)
// apart from the regular interop-up zenohd (bridge networking, no
// multicast traversal) — both bind 7447 on localhost. Send a SCOUT and
// require ≥1 HELLO; on miss, point at the right Make target.
func requireMulticastZenohd(t *testing.T) {
	t.Helper()
	cfg := zenoh.NewConfig()
	cfg.Scouting.MulticastMode = zenoh.MulticastAuto
	ch, err := zenoh.Scout(cfg, zenoh.NewFifoChannel[zenoh.Hello](2), &zenoh.ScoutOptions{
		TimeoutMs: 1500,
		What:      zenoh.WhatRouter,
	})
	if err != nil {
		t.Skipf("multicast scout setup failed (run `make interop-multicast-up`): %v", err)
	}
	got := 0
	for range ch {
		got++
	}
	if got == 0 {
		t.Skip("no multicast HELLO from zenohd-multicast within 1.5s — run `make interop-multicast-up` (NOT `interop-up`); the regular zenohd is bridge-networked and can't traverse the host's multicast group")
	}
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
