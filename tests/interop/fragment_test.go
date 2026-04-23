//go:build interop

package interop

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/zenoh"
)

// TestLargePayloadRoundTrip exercises FRAGMENT split + reassembly through
// zenohd for sizes that span well over the 64 KB stream-batch cap. Each
// case publishes once, receives once, and asserts byte-for-byte equality.
//
// 100 MB and larger are gated behind the `stress` build tag in
// fragment_stress_test.go to keep the default CI runner memory bounded.
//
// Subtests run in parallel — each opens its own sessions and uses a
// size-derived key so cross-talk is impossible.
func TestLargePayloadRoundTrip(t *testing.T) {
	requireZenohd(t)

	cases := []struct {
		name string
		size int
	}{
		{name: "1KiB", size: 1 << 10},
		{name: "64KiB", size: 64 << 10},
		{name: "1MiB", size: 1 << 20},
		{name: "16MiB", size: 16 << 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runLargePayloadRoundTrip(t, tc.size)
		})
	}
}

// runLargePayloadRoundTrip publishes a single payload of the given size
// from a Go session and verifies a separate Go subscriber receives the
// exact bytes back. Shared by the default and stress suites.
func runLargePayloadRoundTrip(t *testing.T, size int) {
	t.Helper()

	subSession := openGoSession(t)
	defer subSession.Close()
	pubSession := openGoSession(t)
	defer pubSession.Close()

	keyStr := fmt.Sprintf("interop/large/%d", size)
	ke, err := zenoh.NewKeyExpr(keyStr)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}

	// Buffer of 2 so a regression that delivers more than one sample
	// surfaces as a test failure rather than a silent drop.
	ch := make(chan zenoh.Sample, 2)
	sub, err := subSession.DeclareSubscriber(ke, zenoh.Closure[zenoh.Sample]{
		Call: func(s zenoh.Sample) { ch <- s },
	})
	if err != nil {
		t.Fatalf("DeclareSubscriber: %v", err)
	}
	defer sub.Drop()

	time.Sleep(subPropagationDelay)

	payload := make([]byte, size)
	// Pseudo-random fill so a buggy reassembly that emits zeros or
	// duplicates a chunk can't pass.
	for i := range payload {
		payload[i] = byte(i*1103515245 + 12345)
	}

	if err := pubSession.Put(ke, zenoh.NewZBytes(payload), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}

	deadline := largePayloadTimeout(size)
	select {
	case s := <-ch:
		got := s.Payload().Bytes()
		if len(got) != len(payload) {
			t.Fatalf("len = %d, want %d", len(got), len(payload))
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("payload mismatch (lengths match)")
		}
	case <-time.After(deadline):
		t.Fatalf("no sample within %v", deadline)
	}
	select {
	case extra := <-ch:
		t.Errorf("unexpected second sample (%d bytes)", len(extra.Payload().Bytes()))
	default:
	}
}

// largePayloadTimeout scales the receive deadline with payload size so
// the 16 MiB and 100 MiB cases don't trip the default ioTimeout. The
// integer division floors so tiny payloads keep ioTimeout.
//
// Empirically loopback over zenohd handles roughly 10 MiB/s for tiny
// FRAGMENT-batched writes; budget at 5 MiB/s plus the constant base.
func largePayloadTimeout(size int) time.Duration {
	return ioTimeout + time.Duration(size/(5<<20))*time.Second
}
