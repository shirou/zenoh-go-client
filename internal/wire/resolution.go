package wire

// Resolution byte carried by INIT and JOIN (when S==1):
//
//	 7 6 5 4 3 2 1 0
//	+-+-+-+-+-+-+-+-+
//	|X|X|X|X|RID|FSN|
//	+-+-+-+-+-+-+-+-+
//
// Codes for each 2-bit sub-field: 0b00=8-bit, 0b01=16-bit, 0b10=32-bit,
// 0b11=64-bit. Default value = 0x0A → both FSN and RID at 32-bit.

// Field selects which 2-bit width (FSN or RID) to read from a Resolution byte.
type Field uint8

const (
	FieldFrameSN   Field = 0 // bits 1:0
	FieldRequestID Field = 1 // bits 3:2
)

// Resolution is a decoded resolution byte.
type Resolution byte

// DefaultResolution = FSN 32-bit, RID 32-bit.
const DefaultResolution Resolution = 0x0A

// Bits returns the bit width selected for the given field.
func (r Resolution) Bits(f Field) uint8 {
	code := uint8(r>>(2*f)) & 0b11
	return 8 << code // 0b00→8, 0b01→16, 0b10→32, 0b11→64
}

// Mask returns (1<<bits)-1 for the given field — useful as a seq-number mask.
// Returns 0xFFFF_FFFF_FFFF_FFFF (all ones) for a 64-bit field.
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
