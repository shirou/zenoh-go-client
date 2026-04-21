package wire

// Resolution byte carried by INIT and JOIN (when S==1):
//
//	 7 6 5 4 3 2 1 0
//	+-+-+-+-+-+-+-+-+
//	|X|X|X|X|RID|FSN|
//	+-+-+-+-+-+-+-+-+
//
// Code → maximum VLE-encoded byte count of the field:
//
//	0b00 → 1 byte (7 data bits, max value 127)
//	0b01 → 2 bytes (14 data bits, max value 16 383)
//	0b10 → 4 bytes (28 data bits, max value 268 435 455)  — default
//	0b11 → 9 bytes (63 data bits, max value 2⁶³−1)
//
// The widths named "8/16/32/64" in the spec table refer to the nominal
// VLE-byte budget, NOT the data bit width of the integer. zenoh-rust's
// `io/zenoh-transport/src/common/seq_num.rs::get_mask` is the canonical
// definition and we match it here; treating code 0b10 as "value <= 2^32 - 1"
// causes the peer to reject frames whose SN happens to exceed 0x0FFFFFFF.

// Field selects which 2-bit width (FSN or RID) to read from a Resolution byte.
type Field uint8

const (
	FieldFrameSN   Field = 0 // bits 1:0
	FieldRequestID Field = 1 // bits 3:2
)

// Resolution is a decoded resolution byte.
type Resolution byte

// DefaultResolution = FSN 4-byte VLE (28 data bits), RID 4-byte VLE.
const DefaultResolution Resolution = 0x0A

// Bits returns the number of data bits carried by the selected field.
func (r Resolution) Bits(f Field) uint8 {
	code := uint8(r>>(2*f)) & 0b11
	switch code {
	case 0b00:
		return 7
	case 0b01:
		return 14
	case 0b10:
		return 28
	default:
		return 63
	}
}

// Mask returns the maximum value of the selected field — useful as a
// seq-number / request-id mask.
func (r Resolution) Mask(f Field) uint64 {
	b := r.Bits(f)
	if b >= 64 {
		return ^uint64(0)
	}
	return (uint64(1) << b) - 1
}

// NewResolution builds a Resolution byte from FSN and RID width codes
// (0=8-bit, 1=16-bit, 2=32-bit, 3=64-bit).
func NewResolution(fsnCode, ridCode uint8) Resolution {
	return Resolution((ridCode&0b11)<<2 | (fsnCode & 0b11))
}
