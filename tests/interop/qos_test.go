//go:build interop

package interop

import (
	"fmt"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/zenoh"
)

// TestGoLaneMetadata covers every (Priority × CongestionControl) combination
// the library exposes, one Put per combination, and asserts the Python
// subscriber observes the metadata that was set.
//
// Excludes PriorityControl (wire value 0) because it is reserved for the
// library's control plane — see the existing TestGoPriorityMetadata for the
// same carve-out.
func TestGoLaneMetadata(t *testing.T) {
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
	congestions := []zenoh.CongestionControl{
		zenoh.CongestionControlDrop,
		zenoh.CongestionControlBlock,
	}

	type combo struct {
		priority zenoh.Priority
		cc       zenoh.CongestionControl
	}
	emissions := make([]combo, 0, len(priorities)*len(congestions))
	for _, p := range priorities {
		for _, c := range congestions {
			emissions = append(emissions, combo{p, c})
		}
	}

	const key = "interop/lane_matrix"
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

	payloadFor := func(c combo) string {
		return fmt.Sprintf("p=%d,c=%d", int(c.priority), int(c.cc))
	}

	for _, c := range emissions {
		opts := &zenoh.PutOptions{
			Priority: c.priority, HasPriority: true,
			CongestionControl: c.cc, HasCongestion: true,
		}
		if err := pub.Put(zenoh.NewZBytesFromString(payloadFor(c)), opts); err != nil {
			t.Fatalf("Put(%s): %v", payloadFor(c), err)
		}
	}

	type obs struct {
		priority int
		cong     int
	}
	got := make(map[string]obs, len(emissions))
	for i := 0; i < len(emissions); i++ {
		line := sub.readLine(t, ioTimeout)
		if line == doneMarker {
			t.Fatalf("DONE before %d samples (got %d)", len(emissions), i)
		}
		rec, payload := decodeRecord(t, line)
		if rec.Priority == nil || rec.Congestion == nil {
			t.Fatalf("%s: missing priority/congestion in JSON: %+v", string(payload), rec)
		}
		got[string(payload)] = obs{*rec.Priority, *rec.Congestion}
	}
	sub.waitFor(t, doneMarker, ioTimeout)

	for _, c := range emissions {
		want := payloadFor(c)
		g, ok := got[want]
		if !ok {
			t.Errorf("missing emission %q", want)
			continue
		}
		if g.priority != int(c.priority) {
			t.Errorf("%s: priority=%d, want %d", want, g.priority, int(c.priority))
		}
		if g.cong != int(c.cc) {
			t.Errorf("%s: congestion=%d, want %d", want, g.cong, int(c.cc))
		}
	}
}

// TestGoPriorityOrderingTendency fires a burst of interleaved high- and
// low-priority Puts and checks that the subscriber observes the high-priority
// samples at earlier arrival positions on average.
//
// The batcher flushes lanes in priority order (lane 0 first, lane 7 last)
// each time the writer drains a batch from the outbound queue, so a tight
// alternating send burst lets the higher-priority lane overtake the lower
// one whenever more than one item is pending. The assertion is intentionally
// weak (median arrival index strictly lower) because a zero-congestion
// localhost run can still occasionally tie.
//
// Not a strict contract: if zenohd's forwarder re-orders differently this
// would need revisiting, but within the Go client's own send path the
// behaviour is deterministic given enough coalescing.
func TestGoPriorityOrderingTendency(t *testing.T) {
	requireZenohd(t)

	const (
		key = "interop/priority_order"
		// Per-priority sample count. With a default outQ buffer of 256 the
		// sender fills the queue before the writer drains, letting the
		// batcher coalesce enough items for inter-lane priority ordering to
		// show up statistically.
		perPriority = 1000
		total       = perPriority * 2
	)

	// Use two sessions: zenohd does not loop a Put back to subscribers on
	// the same session.
	subSession := openGoSession(t)
	defer subSession.Close()
	pubSession := openGoSession(t)
	defer pubSession.Close()

	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}

	const (
		hiTag = "H"
		loTag = "L"
	)
	var (
		mu       sync.Mutex
		arrivals = make([]string, 0, total)
		done     = make(chan struct{})
	)
	sub, err := subSession.DeclareSubscriber(ke, zenoh.Closure[zenoh.Sample]{
		Call: func(s zenoh.Sample) {
			mu.Lock()
			defer mu.Unlock()
			arrivals = append(arrivals, string(s.Payload().Bytes()))
			if len(arrivals) == total {
				close(done)
			}
		},
	})
	if err != nil {
		t.Fatalf("DeclareSubscriber: %v", err)
	}
	defer sub.Drop()

	time.Sleep(subPropagationDelay)

	// Interleaved bursts — alternating high/low so both lanes stay filled
	// while the writer drains. The payload tags which lane the sample came
	// from because the public Sample type does not currently surface the
	// QoS extension's priority.
	hi := &zenoh.PutOptions{Priority: zenoh.PriorityRealTime, HasPriority: true}
	lo := &zenoh.PutOptions{Priority: zenoh.PriorityBackground, HasPriority: true}
	hiPayload := zenoh.NewZBytesFromString(hiTag)
	loPayload := zenoh.NewZBytesFromString(loTag)
	for i := 0; i < perPriority; i++ {
		if err := pubSession.Put(ke, hiPayload, hi); err != nil {
			t.Fatalf("Put hi[%d]: %v", i, err)
		}
		if err := pubSession.Put(ke, loPayload, lo); err != nil {
			t.Fatalf("Put lo[%d]: %v", i, err)
		}
	}

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		mu.Lock()
		got := len(arrivals)
		mu.Unlock()
		t.Fatalf("received %d/%d samples", got, total)
	}

	mu.Lock()
	final := slices.Clone(arrivals)
	mu.Unlock()

	hiIdx := make([]int, 0, perPriority)
	loIdx := make([]int, 0, perPriority)
	for i, tag := range final {
		switch tag {
		case hiTag:
			hiIdx = append(hiIdx, i)
		case loTag:
			loIdx = append(loIdx, i)
		}
	}
	if len(hiIdx) != perPriority || len(loIdx) != perPriority {
		t.Fatalf("per-priority count mismatch: hi=%d lo=%d, want %d each",
			len(hiIdx), len(loIdx), perPriority)
	}
	medHi := medianInt(hiIdx)
	medLo := medianInt(loIdx)
	t.Logf("median arrival index: RealTime=%d, Background=%d", medHi, medLo)
	if medHi >= medLo {
		t.Errorf("priority lanes not honoured: median(RealTime)=%d ≥ median(Background)=%d",
			medHi, medLo)
	}
}

// medianInt returns the median of xs. xs must be non-empty; the sorted
// copy keeps the caller's slice untouched.
func medianInt(xs []int) int {
	sorted := slices.Clone(xs)
	sort.Ints(sorted)
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}
