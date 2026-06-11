package session

import (
	"testing"
)

// One REQUEST delivered to several overlapping queryables must produce
// exactly one "last delivery" — RESPONSE_FINAL is sent once, after the
// final handler completes.
func TestDispatchQuerySharedCompletion(t *testing.T) {
	s := New()
	var deliveries []QueryReceived
	deliver := func(q QueryReceived) { deliveries = append(deliveries, q) }
	s.RegisterQueryable(1, mustKE(t, "a/**"), deliver, false)
	s.RegisterQueryable(2, mustKE(t, "a/b/*"), deliver, false)
	s.RegisterQueryable(3, mustKE(t, "unrelated/**"), deliver, false)

	n := s.dispatchQuery(mustKE(t, "a/b/c"), QueryReceived{RequestID: 7})
	if n != 2 || len(deliveries) != 2 {
		t.Fatalf("dispatched to %d queryables (%d deliveries), want 2", n, len(deliveries))
	}

	if deliveries[0].QueryDone() {
		t.Error("first completion reported as last")
	}
	if !deliveries[1].QueryDone() {
		t.Error("second completion not reported as last")
	}
}

func TestDispatchQueryNoMatchReturnsZero(t *testing.T) {
	s := New()
	s.RegisterQueryable(1, mustKE(t, "a/**"), func(QueryReceived) {}, false)
	if n := s.dispatchQuery(mustKE(t, "b/c"), QueryReceived{RequestID: 8}); n != 0 {
		t.Errorf("dispatchQuery = %d, want 0", n)
	}
}

// A QueryReceived that never went through dispatchQuery (no shared counter)
// must still count as the last delivery.
func TestQueryDoneWithoutCounter(t *testing.T) {
	q := QueryReceived{RequestID: 9}
	if !q.QueryDone() {
		t.Error("counterless QueryReceived should report last")
	}
}
