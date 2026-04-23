//go:build interop && stress

package stress

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/zenoh"
)

// TestStartupOrdering is a table-driven sweep over the nine
// entity-startup permutations called out in docs/testing-strategy.md §N.
// Each subtest drives its pattern end-to-end against a real zenohd and
// asserts the delivery contract; where the current library API cannot
// exercise a pattern cleanly, the subtest skips with a cross-reference.
//
// Each subtest re-checks zenohd reachability before running. zenohd 1.0.0
// has a teardown panic we occasionally trip (resource.rs:597 "Invalid
// Key Expr"); when that happens subsequent subtests skip instead of
// cascading spurious failures.
func TestStartupOrdering(t *testing.T) {
	requireZenohd(t)

	patterns := []struct {
		name string
		run  func(*testing.T)
	}{
		{"PublisherBeforeSubscriber", runPublisherBeforeSubscriber},
		{"SubscriberBeforePublisher", runSubscriberBeforePublisher},
		{"QueryableBeforeGet", runQueryableBeforeGet},
		{"GetBeforeQueryable", runGetBeforeQueryable},
		{"RouterBeforePeer", runRouterBeforePeer},
		{"PeerBeforeRouter", runPeerBeforeRouter},
		{"TokenBeforeLivelinessSubHistoryFalse", func(t *testing.T) { runTokenBeforeLivelinessSub(t, false) }},
		{"TokenBeforeLivelinessSubHistoryTrue", func(t *testing.T) { runTokenBeforeLivelinessSub(t, true) }},
		{"TokenBeforeLivelinessGet", runTokenBeforeLivelinessGet},
	}
	for _, p := range patterns {
		t.Run(p.name, func(t *testing.T) {
			requireZenohd(t)
			p.run(t)
		})
	}
}

// runPeerBeforeRouter documents why the "peer up first, router arrives
// later" pattern is not exercised here: Open() currently requires the
// initial dial to succeed, so "peer waiting on an absent router" is not
// a reachable state through the public API. The reconnect suite
// (tests/interop/reconnect_test.go TestReconnectRedeclaresAndResumes)
// covers the equivalent "live session survives a router bounce" path,
// which is the closest observable proxy for this pattern.
func runPeerBeforeRouter(t *testing.T) {
	t.Skip("covered by tests/interop/reconnect_test.go TestReconnectRedeclaresAndResumes; " +
		"the stress variant would duplicate its docker compose manipulation")
}

// runPublisherBeforeSubscriber: a Publisher that emits a sample before
// any Subscriber exists leaves no trace; once the Subscriber is declared
// subsequent samples arrive normally.
func runPublisherBeforeSubscriber(t *testing.T) {
	pubSession := openSession(t)
	defer pubSession.Close()
	subSession := openSession(t)
	defer subSession.Close()

	key := fmt.Sprintf("stressord/pubfirst/%d", time.Now().UnixNano())
	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}

	pub, err := pubSession.DeclarePublisher(ke, nil)
	if err != nil {
		t.Fatalf("DeclarePublisher: %v", err)
	}
	defer pub.Drop()

	if err := pub.Put(zenoh.NewZBytesFromString("before-sub"), nil); err != nil {
		t.Fatalf("pre-sub Put: %v", err)
	}
	// Give the router a moment so the pre-sub sample cannot be in flight
	// by the time DeclareSubscriber completes.
	time.Sleep(subPropagationDelay)

	ch := make(chan zenoh.Sample, 4)
	sub, err := subSession.DeclareSubscriber(ke, zenoh.Closure[zenoh.Sample]{
		Call: func(s zenoh.Sample) { ch <- s },
	})
	if err != nil {
		t.Fatalf("DeclareSubscriber: %v", err)
	}
	defer sub.Drop()

	time.Sleep(subPropagationDelay)

	if err := pub.Put(zenoh.NewZBytesFromString("after-sub"), nil); err != nil {
		t.Fatalf("post-sub Put: %v", err)
	}

	select {
	case s := <-ch:
		got := string(s.Payload().Bytes())
		if got != "after-sub" {
			t.Errorf("first sample=%q, want %q (pre-sub publish leaked through router)", got, "after-sub")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no sample after Subscriber declare")
	}

	// Drain any stray extra samples; none should arrive.
	select {
	case s := <-ch:
		t.Errorf("unexpected second sample %q", string(s.Payload().Bytes()))
	case <-time.After(250 * time.Millisecond):
	}
}

// runSubscriberBeforePublisher: the canonical ordering — Subscriber
// declare → Publisher declare → Put → Subscriber receives.
func runSubscriberBeforePublisher(t *testing.T) {
	pubSession := openSession(t)
	defer pubSession.Close()
	subSession := openSession(t)
	defer subSession.Close()

	key := fmt.Sprintf("stressord/subfirst/%d", time.Now().UnixNano())
	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}

	ch := make(chan zenoh.Sample, 4)
	sub, err := subSession.DeclareSubscriber(ke, zenoh.Closure[zenoh.Sample]{
		Call: func(s zenoh.Sample) { ch <- s },
	})
	if err != nil {
		t.Fatalf("DeclareSubscriber: %v", err)
	}
	defer sub.Drop()

	time.Sleep(subPropagationDelay)

	pub, err := pubSession.DeclarePublisher(ke, nil)
	if err != nil {
		t.Fatalf("DeclarePublisher: %v", err)
	}
	defer pub.Drop()

	time.Sleep(subPropagationDelay)

	if err := pub.Put(zenoh.NewZBytesFromString("sample"), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}

	select {
	case s := <-ch:
		if got := string(s.Payload().Bytes()); got != "sample" {
			t.Errorf("payload=%q, want %q", got, "sample")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no sample within 3s")
	}
}

// runQueryableBeforeGet: Queryable declared, then Get reaches it.
func runQueryableBeforeGet(t *testing.T) {
	qblSession := openSession(t)
	defer qblSession.Close()
	getSession := openSession(t)
	defer getSession.Close()

	key := fmt.Sprintf("stressord/qblfirst/%d", time.Now().UnixNano())
	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}

	qbl, err := qblSession.DeclareQueryable(ke, func(q *zenoh.Query) {
		_ = q.Reply(ke, zenoh.NewZBytesFromString("reply"), nil)
	}, nil)
	if err != nil {
		t.Fatalf("DeclareQueryable: %v", err)
	}
	defer qbl.Drop()

	time.Sleep(subPropagationDelay)

	replies, err := getSession.Get(ke, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	payloads := collectPayloads(t, replies, 3*time.Second)
	if !slices.Contains(payloads, "reply") {
		t.Errorf("replies=%v, want one %q", payloads, "reply")
	}
}

// runGetBeforeQueryable: a Get with no matching Queryable terminates with
// zero replies — the router sends RESPONSE_FINAL once it has no matches
// to forward, or the client-side Timeout fires.
//
// The "Queryable declared afterwards does not satisfy the completed Get"
// half of the pattern is intentionally implicit: since we observed the
// reply channel close first, any later Queryable cannot re-open it. We
// avoid a post-Get DeclareQueryable because zenohd 1.0.0 occasionally
// panics on a DeclareQueryable / Undeclare pair that races with
// session close (Invalid Key Expr internal, router crash), and that
// panic masks this test's actual signal.
func runGetBeforeQueryable(t *testing.T) {
	getSession := openSession(t)
	defer getSession.Close()

	key := fmt.Sprintf("stressord/getfirst/%d", time.Now().UnixNano())
	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}

	replies, err := getSession.Get(ke, &zenoh.GetOptions{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	payloads := collectPayloads(t, replies, 4*time.Second)
	if len(payloads) != 0 {
		t.Errorf("received %d replies before Queryable existed: %v", len(payloads), payloads)
	}
}

// runRouterBeforePeer: the default case — zenohd is already up when we
// Open. Exercises the trivial positive path.
func runRouterBeforePeer(t *testing.T) {
	session := openSession(t)
	defer session.Close()

	key := fmt.Sprintf("stressord/routerfirst/%d", time.Now().UnixNano())
	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}
	if err := session.Put(ke, zenoh.NewZBytesFromString("hello"), nil); err != nil {
		t.Errorf("Put failed on router-up session: %v", err)
	}
}

// runTokenBeforeLivelinessSub: a token declared before a Liveliness
// Subscriber declares. With History=false the subscriber sees only
// future events (none here). With History=true the router replays the
// alive token as a Put Sample.
func runTokenBeforeLivelinessSub(t *testing.T, history bool) {
	tokSession := openSession(t)
	defer tokSession.Close()
	subSession := openSession(t)
	defer subSession.Close()

	key := fmt.Sprintf("stressord/tokfirst/%v/%d", history, time.Now().UnixNano())
	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}

	tok, err := tokSession.Liveliness().DeclareToken(ke, nil)
	if err != nil {
		t.Fatalf("DeclareToken: %v", err)
	}
	defer tok.Drop()

	// Let the router register the token before the subscriber declares.
	time.Sleep(subPropagationDelay)

	// Capacity 4 so any mistaken multi-event replay surfaces as a test
	// failure rather than silently dropping on a full channel.
	events := make(chan zenoh.Sample, 4)
	sub, err := subSession.Liveliness().DeclareSubscriber(ke, zenoh.Closure[zenoh.Sample]{
		Call: func(s zenoh.Sample) { events <- s },
	}, &zenoh.LivelinessSubscriberOptions{History: history})
	if err != nil {
		t.Fatalf("DeclareSubscriber: %v", err)
	}
	defer sub.Drop()

	if history {
		select {
		case got := <-events:
			if got.Kind() != zenoh.SampleKindPut {
				t.Errorf("History=true: event kind=%v, want Put", got.Kind())
			}
			if got.KeyExpr().String() != key {
				t.Errorf("History=true: event key=%q, want %q", got.KeyExpr(), key)
			}
		case <-time.After(1 * time.Second):
			t.Errorf("History=true: no replayed event within 1s, want 1")
		}
		return
	}

	// History=false: confirm silence in a bounded window. 250 ms is
	// comfortable for the router to decide no replay is owed — the
	// Future-only INTEREST creates no replay state to drain.
	select {
	case got := <-events:
		t.Errorf("History=false: unexpected event for pre-existing token (key=%q)",
			got.KeyExpr().String())
	case <-time.After(250 * time.Millisecond):
	}
}

// runTokenBeforeLivelinessGet: an alive token must be returned by
// Liveliness.Get even though the token was declared before the Get
// caller started.
func runTokenBeforeLivelinessGet(t *testing.T) {
	tokSession := openSession(t)
	defer tokSession.Close()
	getSession := openSession(t)
	defer getSession.Close()

	key := fmt.Sprintf("stressord/tokget/%d", time.Now().UnixNano())
	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}

	tok, err := tokSession.Liveliness().DeclareToken(ke, nil)
	if err != nil {
		t.Fatalf("DeclareToken: %v", err)
	}
	defer tok.Drop()

	time.Sleep(subPropagationDelay)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	replies, err := getSession.Liveliness().GetWithContext(ctx, ke, nil)
	if err != nil {
		t.Fatalf("Liveliness.GetWithContext: %v", err)
	}

	var keys []string
	deadline := time.After(4 * time.Second)
loop:
	for {
		select {
		case r, ok := <-replies:
			if !ok {
				break loop
			}
			s, hasSample := r.Sample()
			if !hasSample {
				_, _, _ = r.Err()
				continue
			}
			keys = append(keys, s.KeyExpr().String())
		case <-deadline:
			t.Fatal("Liveliness.Get reply channel did not close within 4s")
		}
	}
	if !slices.Contains(keys, key) {
		t.Errorf("Liveliness.Get keys=%v, want one containing %q", keys, key)
	}
}

// collectPayloads drains a Get reply channel and returns payload bytes in
// arrival order. Errors on reply are logged but not fatal; a missing
// RESPONSE_FINAL within timeout is fatal.
func collectPayloads(t *testing.T, ch <-chan zenoh.Reply, timeout time.Duration) []string {
	t.Helper()
	deadline := time.After(timeout)
	var out []string
	for {
		select {
		case r, ok := <-ch:
			if !ok {
				return out
			}
			s, hasSample := r.Sample()
			if !hasSample {
				if _, _, isErr := r.Err(); isErr {
					t.Logf("err reply ignored")
				}
				continue
			}
			out = append(out, string(s.Payload().Bytes()))
		case <-deadline:
			t.Fatalf("reply channel did not close within %v; got %v", timeout, out)
			return out
		}
	}
}
