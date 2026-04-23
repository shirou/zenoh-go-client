//go:build interop

package interop

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/zenoh"
)

// TestGetConsolidationMode exercises the three ConsolidationMode values
// end-to-end via zenohd. A Go queryable replies to each query with three
// out-of-order timestamps (100, 50, 150) on a single key. The expected
// visible behaviour per mode:
//
//	None      — all three delivered in the order sent.
//	Monotonic — the reply with ts=50 is dropped (≤ HWM=100).
//	Latest    — a single reply with ts=150 (the newest).
//	Auto      — GetOptions omitted; behaves as Latest.
//
// Client-side translator correctness has its own unit coverage against a
// mock router in zenoh/pubsub_test.go. What this test adds is the real
// zenohd wire path: REQUEST carries the Consolidation extension, the
// router forwards replies, and the client delivers the end-to-end result.
func TestGetConsolidationMode(t *testing.T) {
	requireZenohd(t)

	const key = "interop/consol/k"

	replies := []struct {
		ntp     uint64
		payload string
	}{
		{ntp: 100, payload: "first"},
		{ntp: 50, payload: "second"},
		{ntp: 150, payload: "third"},
	}

	qblSession := openGoSession(t)
	defer qblSession.Close()
	getSession := openGoSession(t)
	defer getSession.Close()

	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}
	qblZID := qblSession.ZId()
	qbl, err := qblSession.DeclareQueryable(ke, func(q *zenoh.Query) {
		for _, r := range replies {
			ts := zenoh.NewTimeStamp(r.ntp, qblZID)
			if err := q.Reply(ke, zenoh.NewZBytesFromString(r.payload),
				&zenoh.QueryReplyOptions{TimeStamp: ts, HasTimeStamp: true}); err != nil {
				t.Errorf("Reply(%s): %v", r.payload, err)
			}
		}
	}, nil)
	if err != nil {
		t.Fatalf("DeclareQueryable: %v", err)
	}
	defer qbl.Drop()

	// Let the router propagate D_QUERYABLE.
	time.Sleep(subPropagationDelay)

	cases := []struct {
		name string
		opts *zenoh.GetOptions
		want []string
	}{
		{
			name: "None",
			opts: &zenoh.GetOptions{Consolidation: zenoh.ConsolidationNone, HasConsolidation: true},
			want: []string{"first", "second", "third"},
		},
		{
			name: "Monotonic",
			opts: &zenoh.GetOptions{Consolidation: zenoh.ConsolidationMonotonic, HasConsolidation: true},
			want: []string{"first", "third"},
		},
		{
			name: "Latest",
			opts: &zenoh.GetOptions{Consolidation: zenoh.ConsolidationLatest, HasConsolidation: true},
			want: []string{"third"},
		},
		{
			name: "AutoDefault",
			opts: nil, // extension omitted → Auto (= Latest)
			want: []string{"third"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch, err := getSession.Get(ke, tc.opts)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			got := collectGetPayloads(t, ch, ioTimeout)
			if !slices.Equal(got, tc.want) {
				t.Errorf("payloads = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGetQueryTarget exercises the three QueryTarget values. Each
// queryable sits on its own session because the router collapses
// multiple queryables-on-same-session/same-key into one endpoint, which
// would defeat QueryTarget=All. Consolidation is forced to None so the
// reply set isn't trimmed client-side.
func TestGetQueryTarget(t *testing.T) {
	requireZenohd(t)

	const key = "interop/target/k"

	queryables := []struct {
		payload  string
		complete bool
	}{
		{payload: "q1", complete: false},
		{payload: "q2", complete: true},
		{payload: "q3", complete: true},
	}

	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}

	for _, q := range queryables {
		sess := openGoSession(t)
		defer sess.Close()
		qbl, err := sess.DeclareQueryable(ke, func(qq *zenoh.Query) {
			if err := qq.Reply(ke, zenoh.NewZBytesFromString(q.payload), nil); err != nil {
				t.Errorf("Reply(%s): %v", q.payload, err)
			}
		}, &zenoh.QueryableOptions{Complete: q.complete})
		if err != nil {
			t.Fatalf("DeclareQueryable(%s): %v", q.payload, err)
		}
		defer qbl.Drop()
	}

	getSession := openGoSession(t)
	defer getSession.Close()

	time.Sleep(subPropagationDelay)

	cases := []struct {
		name      string
		target    zenoh.QueryTarget
		wantCount int
		wantSet   []string // sorted; empty → count-only (e.g. BestMatching picks any)
	}{
		{name: "BestMatching", target: zenoh.QueryTargetBestMatching, wantCount: 1},
		{name: "All", target: zenoh.QueryTargetAll, wantCount: 3, wantSet: []string{"q1", "q2", "q3"}},
		{name: "AllComplete", target: zenoh.QueryTargetAllComplete, wantCount: 2, wantSet: []string{"q2", "q3"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch, err := getSession.Get(ke, &zenoh.GetOptions{
				Target:           tc.target,
				HasTarget:        true,
				Consolidation:    zenoh.ConsolidationNone,
				HasConsolidation: true,
			})
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			got := collectGetPayloads(t, ch, ioTimeout)
			if len(got) != tc.wantCount {
				t.Errorf("count = %d, want %d (got %v)", len(got), tc.wantCount, got)
				return
			}
			if len(tc.wantSet) == 0 {
				return
			}
			sorted := slices.Clone(got)
			slices.Sort(sorted)
			if !slices.Equal(sorted, tc.wantSet) {
				t.Errorf("set = %v, want %v", sorted, tc.wantSet)
			}
		})
	}
}

// TestGetBudget exercises the REQUEST Budget extension end-to-end.
// Three queryables (each on its own session, same pattern as
// TestGetQueryTarget) reply with distinct payloads under Target=All +
// Consolidation=None. zenohd 1.0.0 does not enforce the advisory cap for
// local queryables — all three replies reach the client — so the Go
// client enforces the cap locally. Budget=3 additionally confirms the
// cap is inclusive.
func TestGetBudget(t *testing.T) {
	requireZenohd(t)

	const key = "interop/budget/k"
	const queryableCount = 3

	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}

	for i := 0; i < queryableCount; i++ {
		payload := fmt.Sprintf("qbl-%d", i)
		sess := openGoSession(t)
		defer sess.Close()
		qbl, err := sess.DeclareQueryable(ke, func(qq *zenoh.Query) {
			if err := qq.Reply(ke, zenoh.NewZBytesFromString(payload), nil); err != nil {
				t.Errorf("Reply(%s): %v", payload, err)
			}
		}, nil)
		if err != nil {
			t.Fatalf("DeclareQueryable(%s): %v", payload, err)
		}
		defer qbl.Drop()
	}

	getSession := openGoSession(t)
	defer getSession.Close()

	time.Sleep(subPropagationDelay)

	for _, budget := range []uint32{1, 2, 3} {
		t.Run(fmt.Sprintf("Budget=%d", budget), func(t *testing.T) {
			ch, err := getSession.Get(ke, &zenoh.GetOptions{
				Target:           zenoh.QueryTargetAll,
				HasTarget:        true,
				Consolidation:    zenoh.ConsolidationNone,
				HasConsolidation: true,
				Budget:           budget,
			})
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			got := collectGetPayloads(t, ch, ioTimeout)
			wantCount := int(budget)
			if wantCount > queryableCount {
				wantCount = queryableCount
			}
			if len(got) != wantCount {
				t.Errorf("count = %d (%v), want %d", len(got), got, wantCount)
			}
		})
	}
}

// TestGetTimeout exercises GetOptions.Timeout end-to-end. A Go queryable
// emits one reply, then blocks before its handler can return — so
// RESPONSE_FINAL is never sent. The Get must close the reply channel on
// its local Timeout deadline, and any reply emitted before the block must
// still surface.
//
// This covers the client-side timer ("RESPONSE_FINAL not sent" path); the
// wire-side Timeout extension itself is observed in
// TestSessionGetEmitsRequestExtensions against the mock router.
func TestGetTimeout(t *testing.T) {
	requireZenohd(t)

	const key = "interop/timeout/k"
	const timeout = 250 * time.Millisecond

	qblSession := openGoSession(t)
	defer qblSession.Close()

	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}

	// release unblocks the queryable handler so qbl.Drop can return.
	// Deferred AFTER qblSession.Close so LIFO ordering runs the unblock
	// (and Drop) first, before the session shuts down.
	release := make(chan struct{})
	qbl, err := qblSession.DeclareQueryable(ke, func(q *zenoh.Query) {
		_ = q.Reply(ke, zenoh.NewZBytesFromString("first"), nil)
		<-release
	}, nil)
	if err != nil {
		t.Fatalf("DeclareQueryable: %v", err)
	}
	defer func() {
		close(release)
		qbl.Drop()
	}()

	getSession := openGoSession(t)
	defer getSession.Close()

	time.Sleep(subPropagationDelay)

	start := time.Now()
	ch, err := getSession.Get(ke, &zenoh.GetOptions{
		Consolidation:    zenoh.ConsolidationNone,
		HasConsolidation: true,
		Timeout:          timeout,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	got := collectGetPayloads(t, ch, ioTimeout)
	elapsed := time.Since(start)

	if !slices.Equal(got, []string{"first"}) {
		t.Errorf("payloads = %v, want [first]", got)
	}
	if elapsed < timeout {
		t.Errorf("Get returned in %v, want >= timeout (%v)", elapsed, timeout)
	}
	if max := timeout + 2*time.Second; elapsed > max {
		t.Errorf("Get took %v, want <= %v", elapsed, max)
	}
}

// TestGetParameters covers the Parameters string round-trip. The Get
// caller's parameters reach the Queryable's Query.Parameters() verbatim
// for empty / simple / complex / unicode strings.
func TestGetParameters(t *testing.T) {
	requireZenohd(t)

	const key = "interop/params/k"
	cases := []string{
		"",
		"k1=v1",
		"k1=v1;k2=v2",
		"complex=a/b/c?d=e&f=g",
		"unicode=ä日本語",
	}

	qblSession := openGoSession(t)
	defer qblSession.Close()

	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}

	received := make(chan string, len(cases))
	qbl, err := qblSession.DeclareQueryable(ke, func(q *zenoh.Query) {
		received <- q.Parameters()
		_ = q.Reply(ke, zenoh.NewZBytesFromString("ack"), nil)
	}, nil)
	if err != nil {
		t.Fatalf("DeclareQueryable: %v", err)
	}
	defer qbl.Drop()

	getSession := openGoSession(t)
	defer getSession.Close()

	time.Sleep(subPropagationDelay)

	for _, params := range cases {
		t.Run(fmt.Sprintf("params=%q", params), func(t *testing.T) {
			ch, err := getSession.Get(ke, &zenoh.GetOptions{
				Parameters:       params,
				Consolidation:    zenoh.ConsolidationNone,
				HasConsolidation: true,
			})
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			_ = collectGetPayloads(t, ch, ioTimeout)
			select {
			case got := <-received:
				if got != params {
					t.Errorf("Queryable.Parameters = %q, want %q", got, params)
				}
			case <-time.After(ioTimeout):
				t.Fatal("Queryable handler did not run")
			}
		})
	}
}

// TestReplyKindDistinction verifies the Get caller distinguishes Put,
// Delete, and Err replies returned by a single Queryable invocation.
// Consolidation=None preserves arrival order across all three response
// sub-message kinds (PUT / DEL / ERR).
func TestReplyKindDistinction(t *testing.T) {
	requireZenohd(t)

	const (
		key        = "interop/replykind/k"
		putPayload = "put-payload"
		errPayload = "boom"
	)

	qblSession := openGoSession(t)
	defer qblSession.Close()

	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}

	qbl, err := qblSession.DeclareQueryable(ke, func(q *zenoh.Query) {
		if err := q.Reply(ke, zenoh.NewZBytesFromString(putPayload), nil); err != nil {
			t.Errorf("Reply: %v", err)
		}
		if err := q.ReplyDel(ke, nil); err != nil {
			t.Errorf("ReplyDel: %v", err)
		}
		if err := q.ReplyErr(zenoh.NewZBytesFromString(errPayload), nil); err != nil {
			t.Errorf("ReplyErr: %v", err)
		}
	}, nil)
	if err != nil {
		t.Fatalf("DeclareQueryable: %v", err)
	}
	defer qbl.Drop()

	getSession := openGoSession(t)
	defer getSession.Close()

	time.Sleep(subPropagationDelay)

	ch, err := getSession.Get(ke, &zenoh.GetOptions{
		Consolidation:    zenoh.ConsolidationNone,
		HasConsolidation: true,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	var sawPut, sawDel, sawErr bool
	deadline := time.After(ioTimeout)
	for {
		select {
		case r, ok := <-ch:
			if !ok {
				if !sawPut || !sawDel || !sawErr {
					t.Errorf("missing reply kind: put=%v del=%v err=%v", sawPut, sawDel, sawErr)
				}
				return
			}
			if payload, _, isErr := r.Err(); isErr {
				if sawErr {
					t.Errorf("duplicate Err reply")
				}
				sawErr = true
				if got := payload.String(); got != errPayload {
					t.Errorf("Err payload = %q, want %q", got, errPayload)
				}
				continue
			}
			s, hasSample := r.Sample()
			if !hasSample {
				t.Errorf("reply is neither sample nor err")
				continue
			}
			switch s.Kind() {
			case zenoh.SampleKindPut:
				if sawPut {
					t.Errorf("duplicate Put reply")
				}
				sawPut = true
				if got := s.Payload().String(); got != putPayload {
					t.Errorf("Put payload = %q, want %q", got, putPayload)
				}
			case zenoh.SampleKindDelete:
				if sawDel {
					t.Errorf("duplicate Delete reply")
				}
				sawDel = true
			default:
				t.Errorf("unexpected sample kind = %v", s.Kind())
			}
		case <-deadline:
			t.Fatal("timeout waiting for replies")
		}
	}
}

// TestDeclareQuerier covers the Querier façade end-to-end. A Querier is
// declared with Target=All + Consolidation=None defaults; a default Get
// inherits both and reaches every queryable; per-Get overrides reduce the
// reply set; Get after Drop returns ErrAlreadyDropped. Wire-level
// observation of the INTEREST is covered by TestDeclareQuerierEmitsInterest
// in the package's mock-router unit suite.
func TestDeclareQuerier(t *testing.T) {
	requireZenohd(t)

	const (
		key            = "interop/querier/k"
		queryableCount = 3
	)

	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}

	for i := 0; i < queryableCount; i++ {
		payload := fmt.Sprintf("q%d", i)
		sess := openGoSession(t)
		defer sess.Close()
		qbl, err := sess.DeclareQueryable(ke, func(qq *zenoh.Query) {
			if err := qq.Reply(ke, zenoh.NewZBytesFromString(payload), nil); err != nil {
				t.Errorf("Reply(%s): %v", payload, err)
			}
		}, nil)
		if err != nil {
			t.Fatalf("DeclareQueryable(%s): %v", payload, err)
		}
		defer qbl.Drop()
	}

	qrSession := openGoSession(t)
	defer qrSession.Close()

	qr, err := qrSession.DeclareQuerier(ke, &zenoh.QuerierOptions{
		Target:           zenoh.QueryTargetAll,
		HasTarget:        true,
		Consolidation:    zenoh.ConsolidationNone,
		HasConsolidation: true,
	})
	if err != nil {
		t.Fatalf("DeclareQuerier: %v", err)
	}

	time.Sleep(subPropagationDelay)

	t.Run("Defaults", func(t *testing.T) {
		ch, err := qr.Get(nil)
		if err != nil {
			t.Fatalf("Querier.Get: %v", err)
		}
		got := collectGetPayloads(t, ch, ioTimeout)
		if len(got) != queryableCount {
			t.Errorf("count = %d (%v), want %d", len(got), got, queryableCount)
		}
	})

	t.Run("OverrideBudget", func(t *testing.T) {
		ch, err := qr.Get(&zenoh.QuerierGetOptions{Budget: 2})
		if err != nil {
			t.Fatalf("Querier.Get: %v", err)
		}
		got := collectGetPayloads(t, ch, ioTimeout)
		if len(got) != 2 {
			t.Errorf("count = %d (%v), want 2", len(got), got)
		}
	})

	t.Run("OverrideConsolidation", func(t *testing.T) {
		// Default = ConsolidationNone (3 replies); override Latest →
		// the client-side translator dedupes per-key, leaving 1 reply.
		// Tests the override path independently of router-side routing
		// decisions (zenohd 1.0.0's BestMatching is non-deterministic
		// for equidistant queryables and is exercised in TestGetQueryTarget).
		ch, err := qr.Get(&zenoh.QuerierGetOptions{
			Consolidation:    zenoh.ConsolidationLatest,
			HasConsolidation: true,
		})
		if err != nil {
			t.Fatalf("Querier.Get: %v", err)
		}
		got := collectGetPayloads(t, ch, ioTimeout)
		if len(got) != 1 {
			t.Errorf("count = %d (%v), want 1", len(got), got)
		}
	})

	t.Run("UseAfterDrop", func(t *testing.T) {
		q2, err := qrSession.DeclareQuerier(ke, nil)
		if err != nil {
			t.Fatalf("DeclareQuerier: %v", err)
		}
		q2.Drop()
		if _, err := q2.Get(nil); !errors.Is(err, zenoh.ErrAlreadyDropped) {
			t.Errorf("Get after Drop = %v, want ErrAlreadyDropped", err)
		}
	})

	qr.Drop()
}
