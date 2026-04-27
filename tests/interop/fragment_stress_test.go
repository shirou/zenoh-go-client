//go:build interop && stress && !race

package interop

import "testing"

// TestLargePayloadRoundTripStress runs the 100 MiB case isolated behind
// the `stress` build tag — the default CI runner shouldn't pay the memory
// + wall-time cost on every commit.
//
// Excluded under `-race`: a 100 MiB FRAGMENT round-trip that finishes in
// ~1.3 s without the race detector did not complete in 10 minutes with
// `-race` enabled (likely lock contention on the per-link reassembly
// path). FRAGMENT split + reassembly is still exercised under `-race` by
// the 1 KiB–16 MiB cases in TestLargePayloadRoundTrip; the slowdown for
// very large payloads is tracked separately.
func TestLargePayloadRoundTripStress(t *testing.T) {
	requireZenohd(t)
	runLargePayloadRoundTrip(t, 100<<20)
}
