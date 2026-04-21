package session

import "testing"

func TestInitialSNDeterministic(t *testing.T) {
	a := []byte{0x01, 0x02, 0x03, 0x04}
	b := []byte{0x10, 0x20, 0x30, 0x40}
	s1 := ComputeInitialSN(a, b, 32)
	s2 := ComputeInitialSN(a, b, 32)
	if s1 != s2 {
		t.Fatalf("deterministic: got %d and %d", s1, s2)
	}
}

func TestInitialSNMaskedByFSNWidth(t *testing.T) {
	a := []byte{0xFF}
	b := []byte{0x00}
	for _, bits := range []uint8{8, 16, 32} {
		sn := ComputeInitialSN(a, b, bits)
		if sn >= (uint64(1) << bits) {
			t.Errorf("fsn=%d bits: value %#x exceeds mask", bits, sn)
		}
	}
}

func TestInitialSNAsymmetric(t *testing.T) {
	// swap order of args → different value (each side derives its own SN).
	a := []byte{0x01, 0x02, 0x03, 0x04}
	b := []byte{0x10, 0x20, 0x30, 0x40}
	if ComputeInitialSN(a, b, 32) == ComputeInitialSN(b, a, 32) {
		t.Error("swapping ZIDs should yield different initial_sn")
	}
}
