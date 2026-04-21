package codec

// Message header byte layout (spec §primitives "message header"):
//
//	 7 6 5 4 3 2 1 0
//	+-+-+-+-+-+-+-+-+
//	|Z|F2|F1|  ID   |  bits 4:0 = message ID
//	+-+-+-+---------+
//
// Bits:
//
//	0..4  Message ID (0..31)
//	5     Message-specific flag 1 (commonly named A, R, T, N, S, etc.)
//	6     Message-specific flag 2 (commonly named S, M, E, C, etc.)
//	7     Z: extension chain follows when set
//
// We deliberately keep the meaning of F1/F2 untyped here; higher-level
// encoders/decoders interpret them per message.

// Flag bit positions within the header byte.
const (
	HdrFlagF1 byte = 1 << 5 // 0x20
	HdrFlagF2 byte = 1 << 6 // 0x40
	HdrFlagZ  byte = 1 << 7 // 0x80
	HdrIDMask byte = 0x1F   // low 5 bits
)

// Header represents a decoded message header byte.
type Header struct {
	ID byte // bits 4:0
	F1 bool // bit 5
	F2 bool // bit 6
	Z  bool // bit 7: extension chain follows
}

// PackHeader returns the encoded header byte.
func PackHeader(h Header) byte {
	b := h.ID & HdrIDMask
	if h.F1 {
		b |= HdrFlagF1
	}
	if h.F2 {
		b |= HdrFlagF2
	}
	if h.Z {
		b |= HdrFlagZ
	}
	return b
}

// UnpackHeader extracts ID and flags from a header byte.
func UnpackHeader(b byte) Header {
	return Header{
		ID: b & HdrIDMask,
		F1: b&HdrFlagF1 != 0,
		F2: b&HdrFlagF2 != 0,
		Z:  b&HdrFlagZ != 0,
	}
}

// EncodeHeader writes a message header byte.
func (w *Writer) EncodeHeader(h Header) { w.AppendByte(PackHeader(h)) }

// DecodeHeader reads a message header byte.
func (r *Reader) DecodeHeader() (Header, error) {
	b, err := r.ReadByte()
	if err != nil {
		return Header{}, err
	}
	return UnpackHeader(b), nil
}
