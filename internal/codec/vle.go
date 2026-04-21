package codec

import "math/bits"

// Variable-length encoding used throughout the Zenoh wire format.
//
// Encoding: little-endian base-128 (LEB128). Each byte carries 7 payload
// bits; bit 7 is the continuation flag (1 = more bytes follow). The decoder
// bounds the maximum number of bytes per width so that malformed overlong
// encodings are rejected.
//
// Spec: repo/zenoh-spec/docs/modules/wire/pages/primitives.adoc §VLE.
//
// Maximum byte counts per width:
//
//	z8  → 1–2 bytes (covers 0..255)
//	z16 → 1–3 bytes (covers 0..65535)
//	z32 → 1–5 bytes (covers 0..2^32-1)
//	z64 → 1–10 bytes (covers 0..2^64-1)
const (
	maxZ8Bytes  = 2
	maxZ16Bytes = 3
	maxZ32Bytes = 5
	maxZ64Bytes = 10
)

// EncodeZ64 writes v as a VLE integer. Accepts any unsigned width up to 64 bit.
func (w *Writer) EncodeZ64(v uint64) {
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v == 0 {
			w.buf = append(w.buf, b)
			return
		}
		w.buf = append(w.buf, b|0x80)
	}
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
// divide by 7 (rounded up) and clamp to at least one byte.
func SizeZ64(v uint64) int {
	b := bits.Len64(v)
	if b == 0 {
		return 1
	}
	return (b + 6) / 7
}
