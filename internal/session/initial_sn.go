package session

import (
	"crypto/sha3"
	"encoding/binary"
)

// ComputeInitialSN derives a deterministic initial sequence number from two
// ZenohIDs, matching zenoh-rust's
// io/zenoh-transport/src/unicast/establishment/mod.rs:104.
//
// Both sides compute the same value independently by calling with their own
// ZID first and the peer's ZID second. The returned value is masked to the
// negotiated FSN bit width (fsnBits must be 8, 16, 32, or 64).
func ComputeInitialSN(myZid, peerZid []byte, fsnBits uint8) uint64 {
	h := sha3.NewSHAKE128()
	h.Write(myZid)
	h.Write(peerZid)
	var buf [8]byte
	_, _ = h.Read(buf[:])
	sn := binary.LittleEndian.Uint64(buf[:])
	if fsnBits >= 64 {
		return sn
	}
	return sn & ((uint64(1) << fsnBits) - 1)
}
