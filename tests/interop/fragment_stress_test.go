//go:build interop && stress

package interop

import "testing"

// TestLargePayloadRoundTripStress runs the 100 MiB case isolated behind
// the `stress` build tag — the default CI runner shouldn't pay the memory
// + wall-time cost on every commit.
func TestLargePayloadRoundTripStress(t *testing.T) {
	requireZenohd(t)
	runLargePayloadRoundTrip(t, 100<<20)
}
