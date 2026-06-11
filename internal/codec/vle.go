package codec

import "math/bits"

// Variable-length encoding used throughout the Zenoh wire format.
//
// Encoding: little-endian base-128 (LEB128) capped at 9 bytes. Each byte
// carries 7 payload bits; bit 7 is the continuation flag (1 = more bytes
// follow) — except the 9th byte, which carries the remaining 8 bits raw
// with no continuation semantics. This is NOT plain LEB128: a full uint64
// occupies 9 bytes, never 10, and bit 7 of the 9th byte is payload (bit 63
// of the value). The reference codec is zenoh-codec/src/core/zint.rs.
// The decoder bounds the maximum number of bytes per width so that
// malformed overlong encodings are rejected.
//
// Maximum byte counts per width:
//
//	z8  → 1–2 bytes (covers 0..255)
//	z16 → 1–3 bytes (covers 0..65535)
//	z32 → 1–5 bytes (covers 0..2^32-1)
//	z64 → 1–9 bytes (covers 0..2^64-1)
const (
	maxZ8Bytes  = 2
	maxZ16Bytes = 3
	maxZ32Bytes = 5
	maxZ64Bytes = 9
)

// EncodeZ64 writes v as a VLE integer. Accepts any unsigned width up to 64 bit.
func (w *Writer) EncodeZ64(v uint64) {
	for range maxZ64Bytes - 1 {
		if v < 0x80 {
			w.buf = append(w.buf, byte(v))
			return
		}
		w.buf = append(w.buf, byte(v)|0x80)
		v >>= 7
	}
	// 9th byte: the remaining 8 bits go out raw. For values >= 2^63 bit 7
	// is set but it is payload, not a continuation flag.
	w.buf = append(w.buf, byte(v))
}

// EncodeZ32 writes v as a VLE integer at most 5 bytes long.
func (w *Writer) EncodeZ32(v uint32) { w.EncodeZ64(uint64(v)) }

// EncodeZ16 writes v as a VLE integer at most 3 bytes long.
func (w *Writer) EncodeZ16(v uint16) { w.EncodeZ64(uint64(v)) }

// EncodeZ8 writes v as a VLE integer at most 2 bytes long.
func (w *Writer) EncodeZ8(v uint8) { w.EncodeZ64(uint64(v)) }

// decodeVLE reads a VLE integer bounded to maxBytes. Returns (value, bytesConsumed, error).
func decodeVLE(r *Reader, maxBytes int) (uint64, error) {
	var value uint64
	var shift uint
	for i := range maxBytes {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		// At full uint64 width, the 9th byte carries all 8 remaining bits
		// raw; bit 7 is payload (bit 63), not a continuation flag.
		if i == maxZ64Bytes-1 {
			value |= uint64(b) << shift
			return value, nil
		}
		// The last allowed byte must not have the continuation bit set.
		// This rejects encodings that would overflow the target width.
		if i == maxBytes-1 && b&0x80 != 0 {
			return 0, ErrOverflow
		}
		value |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return value, nil
		}
		shift += 7
	}
	// Should not be reached — maxBytes is small enough that the loop
	// returns inside.
	return 0, ErrOverflow
}

// DecodeZ64 reads a VLE uint64.
func (r *Reader) DecodeZ64() (uint64, error) { return decodeVLE(r, maxZ64Bytes) }

// DecodeZ32 reads a VLE uint32.
func (r *Reader) DecodeZ32() (uint32, error) {
	v, err := decodeVLE(r, maxZ32Bytes)
	if err != nil {
		return 0, err
	}
	if v > 0xFFFFFFFF {
		return 0, ErrOverflow
	}
	return uint32(v), nil
}

// DecodeZ16 reads a VLE uint16.
func (r *Reader) DecodeZ16() (uint16, error) {
	v, err := decodeVLE(r, maxZ16Bytes)
	if err != nil {
		return 0, err
	}
	if v > 0xFFFF {
		return 0, ErrOverflow
	}
	return uint16(v), nil
}

// DecodeZ8 reads a VLE uint8.
func (r *Reader) DecodeZ8() (uint8, error) {
	v, err := decodeVLE(r, maxZ8Bytes)
	if err != nil {
		return 0, err
	}
	if v > 0xFF {
		return 0, ErrOverflow
	}
	return uint8(v), nil
}

// SizeZ64 returns the encoded byte length of v as z64. Useful when reserving
// space ahead of time, e.g. for outer length prefixes.
//
// Implementation: one bit-scan (bits.Len64) determines the MSB position;
// divide by 7 (rounded up), clamp to at least one byte and to the 9-byte
// cap (the 9th byte carries 8 raw bits, so 64-bit values never need 10).
func SizeZ64(v uint64) int {
	b := bits.Len64(v)
	if b == 0 {
		return 1
	}
	n := (b + 6) / 7
	if n > maxZ64Bytes {
		n = maxZ64Bytes
	}
	return n
}
