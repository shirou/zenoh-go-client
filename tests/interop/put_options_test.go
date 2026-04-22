//go:build interop

package interop

import (
	"fmt"
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/zenoh"
)

// TestGoPriorityMetadata: the priority Go sets lands on the Python
// subscriber's sample.priority field.
//
// Excludes Control (0) because it is reserved for control-plane messages
// and rejecting it at send time is the library's prerogative. Including
// Data (5, the default) is a by-design omission on the wire: no ext is
// emitted, and Python resolves sample.priority to the same Data default,
// which round-trips correctly.
func TestGoPriorityMetadata(t *testing.T) {
	requireZenohd(t)

	priorities := []zenoh.Priority{
		zenoh.PriorityRealTime,
		zenoh.PriorityInteractiveHigh,
		zenoh.PriorityInteractiveLow,
		zenoh.PriorityDataHigh,
		zenoh.PriorityData,
		zenoh.PriorityDataLow,
		zenoh.PriorityBackground,
	}

	const key = "interop/priority"
	sub := startPython(t, "python_sub.py",
		"--key", key, "--count", fmt.Sprint(len(priorities)))
	defer sub.close(t)
	sub.waitFor(t, readyMarker, readyTimeout)

	session := openGoSession(t)
	defer session.Close()

	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}
	pub, err := session.DeclarePublisher(ke, nil)
	if err != nil {
		t.Fatalf("DeclarePublisher: %v", err)
	}
	defer pub.Drop()
	time.Sleep(subPropagationDelay)

	for _, p := range priorities {
		err := pub.Put(zenoh.NewZBytesFromString(p.String()), &zenoh.PutOptions{
			Priority: p, HasPriority: true,
		})
		if err != nil {
			t.Fatalf("Put(priority=%s): %v", p, err)
		}
	}

	type pair struct {
		payload  string
		priority int
	}
	got := make(map[pair]bool, len(priorities))
	for i := 0; i < len(priorities); i++ {
		line := sub.readLine(t, ioTimeout)
		if line == doneMarker {
			t.Fatalf("DONE before %d samples (got %d)", len(priorities), i)
		}
		rec, payload := decodeRecord(t, line)
		if rec.Priority == nil {
			t.Fatalf("%s: priority missing from JSON", rec.Key)
		}
		got[pair{string(payload), *rec.Priority}] = true
	}
	sub.waitFor(t, doneMarker, ioTimeout)

	for _, p := range priorities {
		want := pair{p.String(), int(p)}
		if !got[want] {
			t.Errorf("missing %v; got %v", want, got)
		}
	}
}

// TestGoCongestionExpressMetadata: CongestionControl and Express metadata
// round-trip to the Python subscriber. Actual drop / batch-bypass behaviour
// is covered by the QoS lane tests once fault injection is wired up.
func TestGoCongestionExpressMetadata(t *testing.T) {
	requireZenohd(t)

	type emission struct {
		label    string
		cc       zenoh.CongestionControl
		express  bool
		wantCong int
	}
	emissions := []emission{
		{label: "drop_noexpress", cc: zenoh.CongestionControlDrop, express: false, wantCong: 0},
		{label: "drop_express", cc: zenoh.CongestionControlDrop, express: true, wantCong: 0},
		{label: "block_noexpress", cc: zenoh.CongestionControlBlock, express: false, wantCong: 1},
		{label: "block_express", cc: zenoh.CongestionControlBlock, express: true, wantCong: 1},
	}

	const key = "interop/congestion"
	sub := startPython(t, "python_sub.py",
		"--key", key, "--count", fmt.Sprint(len(emissions)))
	defer sub.close(t)
	sub.waitFor(t, readyMarker, readyTimeout)

	session := openGoSession(t)
	defer session.Close()

	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}
	pub, err := session.DeclarePublisher(ke, nil)
	if err != nil {
		t.Fatalf("DeclarePublisher: %v", err)
	}
	defer pub.Drop()
	time.Sleep(subPropagationDelay)

	for _, e := range emissions {
		err := pub.Put(zenoh.NewZBytesFromString(e.label), &zenoh.PutOptions{
			CongestionControl: e.cc, HasCongestion: true,
			IsExpress: e.express,
		})
		if err != nil {
			t.Fatalf("Put(%s): %v", e.label, err)
		}
	}

	type obs struct {
		cong    int
		express bool
	}
	got := map[string]obs{}
	for i := 0; i < len(emissions); i++ {
		line := sub.readLine(t, ioTimeout)
		if line == doneMarker {
			t.Fatalf("DONE before %d samples (got %d)", len(emissions), i)
		}
		rec, payload := decodeRecord(t, line)
		if rec.Congestion == nil || rec.Express == nil {
			t.Fatalf("%s: missing congestion/express in JSON: %+v", string(payload), rec)
		}
		got[string(payload)] = obs{*rec.Congestion, *rec.Express}
	}
	sub.waitFor(t, doneMarker, ioTimeout)

	for _, e := range emissions {
		g, ok := got[e.label]
		if !ok {
			t.Errorf("missing emission %q", e.label)
			continue
		}
		if g.cong != e.wantCong {
			t.Errorf("%s: congestion=%d, want %d", e.label, g.cong, e.wantCong)
		}
		if g.express != e.express {
			t.Errorf("%s: express=%v, want %v", e.label, g.express, e.express)
		}
	}
}
