package session

import (
	"sync"
	"testing"

	"github.com/shirou/zenoh-go-client/internal/keyexpr"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// fakeSrc is an arbitrary non-zero ZenohID used by tests that don't
// care which peer "sent" the simulated declaration.
var fakeSrc = wire.ZenohID{Bytes: []byte{0xCA, 0xFE}}

func declare(s *Session, kind MatchingKind, entityID uint32, ke keyexpr.KeyExpr) {
	s.onMatchingDeclare(kind, fakeSrc, entityID, ke)
}

func undeclare(s *Session, kind MatchingKind, entityID uint32, ke keyexpr.KeyExpr) {
	s.onMatchingUndeclare(kind, fakeSrc, entityID, ke)
}

func mustKE(t *testing.T, s string) keyexpr.KeyExpr {
	t.Helper()
	ke, err := keyexpr.New(s)
	if err != nil {
		t.Fatalf("keyexpr %q: %v", s, err)
	}
	return ke
}

// collectListener is a MatchingDeliverFn that records every invocation
// under a mutex so the test can assert ordering deterministically.
type collectListener struct {
	mu   sync.Mutex
	msgs []MatchingStatus
}

func (c *collectListener) deliver(st MatchingStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, st)
}

func (c *collectListener) snapshot() []MatchingStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]MatchingStatus(nil), c.msgs...)
}

func TestMatching_InitialSnapshotAndTransitions(t *testing.T) {
	s := New()
	ke := mustKE(t, "demo/**")
	s.RegisterMatching(42, ke, MatchingSubscribers)

	coll := &collectListener{}
	if _, ok := s.AttachMatchingListener(42, coll.deliver, nil); !ok {
		t.Fatal("attach failed")
	}

	// Current snapshot: two matching subscribers arrive before DeclareFinal.
	declare(s, MatchingSubscribers, 1, mustKE(t, "demo/a"))
	declare(s, MatchingSubscribers, 2, mustKE(t, "demo/b"))
	if got := coll.snapshot(); len(got) != 0 {
		t.Fatalf("pre-snapshot deliveries: %+v", got)
	}

	// DeclareFinal completes the snapshot → first delivery.
	if !s.onMatchingSnapshotDone(42) {
		t.Fatal("SnapshotDone on known id returned false")
	}
	got := coll.snapshot()
	if len(got) != 1 || !got[0].Matching {
		t.Fatalf("after snapshot: got %+v, want [{true}]", got)
	}

	// Subsequent subscriber arrival — no 0↔1 transition, no delivery.
	declare(s, MatchingSubscribers, 3, mustKE(t, "demo/c"))
	if len(coll.snapshot()) != 1 {
		t.Fatalf("no transition but delivered: %+v", coll.snapshot())
	}

	// Undeclare three — count hits 0, triggers Matching=false.
	undeclare(s, MatchingSubscribers, 1, mustKE(t, "demo/a"))
	undeclare(s, MatchingSubscribers, 2, mustKE(t, "demo/b"))
	undeclare(s, MatchingSubscribers, 3, mustKE(t, "demo/c"))
	got = coll.snapshot()
	if len(got) != 2 || got[1].Matching {
		t.Fatalf("after drain: got %+v, want [{true},{false}]", got)
	}

	// One more arrival → 0→1 again.
	declare(s, MatchingSubscribers, 4, mustKE(t, "demo/d"))
	got = coll.snapshot()
	if len(got) != 3 || !got[2].Matching {
		t.Fatalf("after re-match: got %+v, want 3rd {true}", got)
	}
}

func TestMatching_NonIntersectingIgnored(t *testing.T) {
	s := New()
	s.RegisterMatching(1, mustKE(t, "demo/**"), MatchingSubscribers)

	coll := &collectListener{}
	s.AttachMatchingListener(1, coll.deliver, nil)
	s.onMatchingSnapshotDone(1)
	if got := coll.snapshot(); len(got) != 1 || got[0].Matching {
		t.Fatalf("initial after snapshot: %+v", got)
	}

	declare(s, MatchingSubscribers, 1, mustKE(t, "other/x"))
	if got := coll.snapshot(); len(got) != 1 {
		t.Fatalf("non-intersecting declare delivered: %+v", got)
	}
}

func TestMatching_KindFiltered(t *testing.T) {
	s := New()
	s.RegisterMatching(1, mustKE(t, "demo/**"), MatchingSubscribers)

	coll := &collectListener{}
	s.AttachMatchingListener(1, coll.deliver, nil)
	s.onMatchingSnapshotDone(1)

	// Same key, wrong kind — must not count.
	declare(s, MatchingQueryables, 1, mustKE(t, "demo/x"))
	got := coll.snapshot()
	if len(got) != 1 || got[0].Matching {
		t.Fatalf("queryable leaked into subscribers state: %+v", got)
	}
}

func TestMatching_AttachAfterSnapshotDelivers(t *testing.T) {
	s := New()
	s.RegisterMatching(1, mustKE(t, "demo/**"), MatchingSubscribers)

	declare(s, MatchingSubscribers, 1, mustKE(t, "demo/a"))
	s.onMatchingSnapshotDone(1)

	coll := &collectListener{}
	s.AttachMatchingListener(1, coll.deliver, nil)
	got := coll.snapshot()
	if len(got) != 1 || !got[0].Matching {
		t.Fatalf("late-attach should see current state: %+v", got)
	}
}

func TestMatching_MultipleListenersAllNotified(t *testing.T) {
	s := New()
	s.RegisterMatching(1, mustKE(t, "demo/**"), MatchingSubscribers)

	a := &collectListener{}
	b := &collectListener{}
	s.AttachMatchingListener(1, a.deliver, nil)
	s.AttachMatchingListener(1, b.deliver, nil)

	s.onMatchingSnapshotDone(1)
	declare(s, MatchingSubscribers, 1, mustKE(t, "demo/a"))

	if got := a.snapshot(); len(got) != 2 || got[0].Matching || !got[1].Matching {
		t.Fatalf("listener A: %+v", got)
	}
	if got := b.snapshot(); len(got) != 2 || got[0].Matching || !got[1].Matching {
		t.Fatalf("listener B: %+v", got)
	}
}

func TestMatching_DetachStopsDelivery(t *testing.T) {
	s := New()
	s.RegisterMatching(1, mustKE(t, "demo/**"), MatchingSubscribers)
	coll := &collectListener{}
	listenerID, _ := s.AttachMatchingListener(1, coll.deliver, nil)
	s.onMatchingSnapshotDone(1)

	s.DetachMatchingListener(1, listenerID)
	declare(s, MatchingSubscribers, 1, mustKE(t, "demo/a"))

	got := coll.snapshot()
	if len(got) != 1 {
		t.Fatalf("after detach: %+v", got)
	}
}

func TestMatching_ResetClearsState(t *testing.T) {
	s := New()
	s.RegisterMatching(1, mustKE(t, "demo/**"), MatchingSubscribers)
	coll := &collectListener{}
	s.AttachMatchingListener(1, coll.deliver, nil)

	declare(s, MatchingSubscribers, 1, mustKE(t, "demo/a"))
	s.onMatchingSnapshotDone(1)
	if got := coll.snapshot(); len(got) != 1 || !got[0].Matching {
		t.Fatalf("pre-reset: %+v", got)
	}

	s.ResetMatching()
	// After reset the entry is in snapshot=false, count=0 state.
	// A new snapshot-done with count=0 should deliver {false}.
	s.onMatchingSnapshotDone(1)
	got := coll.snapshot()
	if len(got) != 2 || got[1].Matching {
		t.Fatalf("post-reset: %+v (want [{true},{false}])", got)
	}
}

func TestMatching_UnregisterFiresCleanupHook(t *testing.T) {
	s := New()
	s.RegisterMatching(1, mustKE(t, "demo/**"), MatchingSubscribers)

	cleaned := 0
	var hookMu sync.Mutex
	hook := func() {
		hookMu.Lock()
		cleaned++
		hookMu.Unlock()
	}
	s.AttachMatchingListener(1, func(MatchingStatus) {}, hook)
	s.AttachMatchingListener(1, func(MatchingStatus) {}, hook)

	s.UnregisterMatching(1)

	hookMu.Lock()
	defer hookMu.Unlock()
	if cleaned != 2 {
		t.Fatalf("cleanup hooks fired = %d, want 2", cleaned)
	}
}

// TestMatching_DuplicateDeclareIsNoop pins the per-(peer, entityID)
// dedup. A peer that re-floods the same D_SUBSCRIBER (which is exactly
// what happens on every multicast OnPeerJoin from the publisher's
// side) must not bump matchCount past 1.
func TestMatching_DuplicateDeclareIsNoop(t *testing.T) {
	s := New()
	s.RegisterMatching(1, mustKE(t, "demo/**"), MatchingSubscribers)
	coll := &collectListener{}
	s.AttachMatchingListener(1, coll.deliver, nil)
	s.onMatchingSnapshotDone(1)

	src := wire.ZenohID{Bytes: []byte{0xAA}}
	for range 5 {
		s.onMatchingDeclare(MatchingSubscribers, src, 42, mustKE(t, "demo/a"))
	}
	// Expect exactly two deliveries: snapshot ({false}) and the single
	// 0→1 transition the dedup'd declares produced. Five repeats must
	// not produce five "true" notifications.
	got := coll.snapshot()
	if len(got) != 2 || got[0].Matching || !got[1].Matching {
		t.Fatalf("dup declare fired wrong number of transitions: %+v", got)
	}

	// One U_SUBSCRIBER from the same peer drops the count to zero.
	s.onMatchingUndeclare(MatchingSubscribers, src, 42, mustKE(t, "demo/a"))
	got = coll.snapshot()
	if len(got) != 3 || got[2].Matching {
		t.Fatalf("after undeclare: %+v", got)
	}
}

// TestMatching_PerPeerEntityIDNamespace pins that the dedup key
// includes the source peer ZID. Two peers may legitimately each pick
// entityID=1 for their own subscribers; they must count separately.
func TestMatching_PerPeerEntityIDNamespace(t *testing.T) {
	s := New()
	s.RegisterMatching(1, mustKE(t, "demo/**"), MatchingSubscribers)
	coll := &collectListener{}
	s.AttachMatchingListener(1, coll.deliver, nil)
	s.onMatchingSnapshotDone(1)

	a := wire.ZenohID{Bytes: []byte{0xAA}}
	b := wire.ZenohID{Bytes: []byte{0xBB}}
	s.onMatchingDeclare(MatchingSubscribers, a, 1, mustKE(t, "demo/a"))
	s.onMatchingDeclare(MatchingSubscribers, b, 1, mustKE(t, "demo/b"))

	// snapshot ({false}) + the 0→1 transition from a's declare. b
	// arrives while count>0 so produces no transition.
	if got := coll.snapshot(); len(got) != 2 || got[0].Matching || !got[1].Matching {
		t.Fatalf("after two distinct peers: %+v", got)
	}

	// Undeclare from peer A only — peer B still matches, no transition.
	s.onMatchingUndeclare(MatchingSubscribers, a, 1, mustKE(t, "demo/a"))
	if got := coll.snapshot(); len(got) != 2 {
		t.Fatalf("partial undeclare leaked a transition: %+v", got)
	}

	// Undeclare from peer B drops to zero.
	s.onMatchingUndeclare(MatchingSubscribers, b, 1, mustKE(t, "demo/b"))
	got := coll.snapshot()
	if len(got) != 3 || got[2].Matching {
		t.Fatalf("after both undeclare: %+v", got)
	}
}

func TestMatching_GetMatchingStatus(t *testing.T) {
	s := New()
	s.RegisterMatching(1, mustKE(t, "demo/**"), MatchingSubscribers)

	st, ok := s.GetMatchingStatus(1)
	if !ok || st.Matching {
		t.Fatalf("initial: %+v ok=%v", st, ok)
	}

	declare(s, MatchingSubscribers, 1, mustKE(t, "demo/a"))
	st, _ = s.GetMatchingStatus(1)
	if !st.Matching {
		t.Fatal("GetMatchingStatus ignores pre-snapshot counts")
	}

	_, ok = s.GetMatchingStatus(999)
	if ok {
		t.Fatal("unknown interestID should report ok=false")
	}
}
