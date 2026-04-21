package codec

import (
	"fmt"
)

// Extension TLV header byte (spec §primitives "extension header"):
//
//	 7 6 5 4 3 2 1 0
//	+-+-+-+-+-+-+-+-+
//	|Z|ENC|M| ExtID |
//	+-+-+-+-+-+-+-+-+
//
// Bits:
//
//	0..3  ExtID (1..15)
//	4     M: mandatory flag (if unknown and M==1 receiver MUST close)
//	5..6  ENC: body encoding selector (Unit/Z64/ZBuf/reserved)
//	7     Z: another extension follows
//
// The body encoding dictates how the following bytes are consumed:
//
//	Unit  (ENC=0b00) → no body
//	Z64   (ENC=0b01) → one VLE-encoded u64
//	ZBuf  (ENC=0b10) → <u8;z32> byte array
//	0b11  reserved

// ExtEncoding is the 2-bit ENC selector.
type ExtEncoding uint8

const (
	ExtEncUnit     ExtEncoding = 0b00
	ExtEncZ64      ExtEncoding = 0b01
	ExtEncZBuf     ExtEncoding = 0b10
	ExtEncReserved ExtEncoding = 0b11
)

func (e ExtEncoding) String() string {
	switch e {
	case ExtEncUnit:
		return "Unit"
	case ExtEncZ64:
		return "Z64"
	case ExtEncZBuf:
		return "ZBuf"
	default:
		return "Reserved"
	}
}

// Extension header flag bits.
const (
	extFlagM byte = 1 << 4 // 0x10
	extFlagZ byte = 1 << 7 // 0x80
	extEncShift     = 5
	extEncMask      = 0b11 << extEncShift
	extIDMask  byte = 0x0F
)

// ExtHeader is the decoded extension header byte.
type ExtHeader struct {
	ID        uint8 // bits 3:0
	Mandatory bool  // bit 4
	Encoding  ExtEncoding
	More      bool // bit 7: another extension follows
}

// PackExtHeader returns the encoded extension header byte.
func PackExtHeader(h ExtHeader) byte {
	b := h.ID & extIDMask
	if h.Mandatory {
		b |= extFlagM
	}
	b |= byte(h.Encoding&0b11) << extEncShift
	if h.More {
		b |= extFlagZ
	}
	return b
}

// UnpackExtHeader decodes an extension header byte.
func UnpackExtHeader(b byte) ExtHeader {
	return ExtHeader{
		ID:        b & extIDMask,
		Mandatory: b&extFlagM != 0,
		Encoding:  ExtEncoding((b & extEncMask) >> extEncShift),
		More:      b&extFlagZ != 0,
	}
}

// Extension is a decoded extension TLV: header + body.
//
// Body holds:
//
//	Unit → nil
//	Z64  → raw VLE bytes (typically re-decoded by the caller with DecodeZ64)
//	ZBuf → the <u8;z32> payload (without the length prefix)
type Extension struct {
	Header ExtHeader
	Z64    uint64 // valid when Header.Encoding == ExtEncZ64
	ZBuf   []byte // valid when Header.Encoding == ExtEncZBuf
}

// NewZ64Ext builds a Z64-encoded extension with the given ID, M flag, and
// body value. Keeps call sites out of the encoding/flag bookkeeping.
func NewZ64Ext(id byte, mandatory bool, v uint64) Extension {
	return Extension{
		Header: ExtHeader{ID: id, Encoding: ExtEncZ64, Mandatory: mandatory},
		Z64:    v,
	}
}

// FindExt scans an extension chain for the first element whose ID matches
// and returns a pointer into the slice, or nil. Callers that care about the
// body encoding should check Header.Encoding before reading Z64 / ZBuf.
func FindExt(exts []Extension, id byte) *Extension {
	for i := range exts {
		if exts[i].Header.ID == id {
			return &exts[i]
		}
	}
	return nil
}

// EncodeExtension writes an extension header + body. The caller sets
// Extension.Header.More to indicate whether another extension follows.
func (w *Writer) EncodeExtension(ext Extension) error {
	w.AppendByte(PackExtHeader(ext.Header))
	switch ext.Header.Encoding {
	case ExtEncUnit:
		// no body
	case ExtEncZ64:
		w.EncodeZ64(ext.Z64)
	case ExtEncZBuf:
		if err := w.EncodeBytesZ32(ext.ZBuf); err != nil {
			return err
		}
	case ExtEncReserved:
		return fmt.Errorf("codec: cannot encode Reserved extension encoding")
	}
	return nil
}

// DecodeExtension reads one extension TLV. Call repeatedly while the
// previous header's More flag was set.
func (r *Reader) DecodeExtension() (Extension, error) {
	hb, err := r.ReadByte()
	if err != nil {
		return Extension{}, err
	}
	h := UnpackExtHeader(hb)
	ext := Extension{Header: h}
	switch h.Encoding {
	case ExtEncUnit:
		// no body
	case ExtEncZ64:
		v, err := r.DecodeZ64()
		if err != nil {
			return Extension{}, fmt.Errorf("extension id=%d Z64 body: %w", h.ID, err)
		}
		ext.Z64 = v
	case ExtEncZBuf:
		b, err := r.DecodeBytesZ32()
		if err != nil {
			return Extension{}, fmt.Errorf("extension id=%d ZBuf body: %w", h.ID, err)
		}
		ext.ZBuf = b
	case ExtEncReserved:
		return Extension{}, fmt.Errorf("codec: reserved extension encoding (id=%d)", h.ID)
	}
	return ext, nil
}

// DecodeExtensions reads an extension chain starting after a header whose Z bit is set.
// It stops after consuming an extension with More=false and returns the collected
// extensions in order.
//
// Chains are typically short (QoS + Timestamp is the common pair), so we
// pre-size to 2 to avoid the first one or two regrow-and-copy cycles on the
// per-packet hot path.
func (r *Reader) DecodeExtensions() ([]Extension, error) {
	exts := make([]Extension, 0, 2)
	for {
		ext, err := r.DecodeExtension()
		if err != nil {
			return nil, err
		}
		exts = append(exts, ext)
		if !ext.Header.More {
			return exts, nil
		}
	}
}

// EncodeExtensions writes a chain of extensions. The More flag on each
// element is recomputed so the caller can leave it zero. Returns without
// writing anything when the chain is empty.
func (w *Writer) EncodeExtensions(exts []Extension) error {
	for i := range exts {
		exts[i].Header.More = i < len(exts)-1
		if err := w.EncodeExtension(exts[i]); err != nil {
			return err
		}
	}
	return nil
}
