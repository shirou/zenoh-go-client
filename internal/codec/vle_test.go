package codec

import (
	"bytes"
	"errors"
	"testing"
)

// Golden vectors from repo/zenoh-spec/docs/modules/wire/pages/primitives.adoc.
var vleGolden = []struct {
	v        uint64
	expected []byte
}{
	{0, []byte{0x00}},
	{1, []byte{0x01}},
	{127, []byte{0x7F}},
	{128, []byte{0x80, 0x01}},
	{300, []byte{0xAC, 0x02}},
	{16383, []byte{0xFF, 0x7F}},
	{16384, []byte{0x80, 0x80, 0x01}},
	{0xFFFFFFFF, []byte{0xFF, 0xFF, 0xFF, 0xFF, 0x0F}},
	{0xFFFFFFFFFFFFFFFF, []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x01}},
}

func TestEncodeZ64Golden(t *testing.T) {
	for _, g := range vleGolden {
		w := NewWriter(10)
		w.EncodeZ64(g.v)
		if !bytes.Equal(w.Bytes(), g.expected) {
			t.Errorf("EncodeZ64(%d) = % x, want % x", g.v, w.Bytes(), g.expected)
		}
	}
}

func TestDecodeZ64Golden(t *testing.T) {
	for _, g := range vleGolden {
		r := NewReader(g.expected)
		got, err := r.DecodeZ64()
		if err != nil {
			t.Fatalf("DecodeZ64(% x): %v", g.expected, err)
		}
		if got != g.v {
			t.Errorf("DecodeZ64(% x) = %d, want %d", g.expected, got, g.v)
		}
		if r.Len() != 0 {
			t.Errorf("DecodeZ64 left %d bytes unread", r.Len())
		}
	}
}

func TestSizeZ64(t *testing.T) {
	for _, g := range vleGolden {
		if got := SizeZ64(g.v); got != len(g.expected) {
			t.Errorf("SizeZ64(%d) = %d, want %d", g.v, got, len(g.expected))
		}
	}
}

func TestDecodeZ32Overflow(t *testing.T) {
	// Encoding of 2^32 requires 5 bytes with the 5th byte still having bit 7 set
	// or value > 32 bits. Use raw bytes to force overflow.
	// 2^32 = 0x1_0000_0000 → VLE: 0x80 0x80 0x80 0x80 0x10
	r := NewReader([]byte{0x80, 0x80, 0x80, 0x80, 0x10})
	if _, err := r.DecodeZ32(); !errors.Is(err, ErrOverflow) {
		t.Errorf("expected ErrOverflow, got %v", err)
	}
}

func TestDecodeZ16Overflow(t *testing.T) {
	// 65536 = 0x10000 → VLE: 0x80 0x80 0x04
	r := NewReader([]byte{0x80, 0x80, 0x04})
	if _, err := r.DecodeZ16(); !errors.Is(err, ErrOverflow) {
		t.Errorf("expected ErrOverflow, got %v", err)
	}
}

func TestDecodeZ64OverlongRejected(t *testing.T) {
	// 11 bytes all with continuation bit set (the 11th would be too many for z64).
	r := NewReader([]byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01})
	if _, err := r.DecodeZ64(); !errors.Is(err, ErrOverflow) {
		t.Errorf("expected ErrOverflow on overlong z64, got %v", err)
	}
}

func TestDecodeZ64Truncated(t *testing.T) {
	r := NewReader([]byte{0x80}) // continuation bit set but no follow-up
	if _, err := r.DecodeZ64(); !errors.Is(err, ErrUnexpectedEOF) {
		t.Errorf("expected ErrUnexpectedEOF, got %v", err)
	}
}

func TestRoundtripZ32(t *testing.T) {
	values := []uint32{0, 1, 127, 128, 16383, 16384, 0x12345, 0xFFFFFFFF}
	for _, v := range values {
		w := NewWriter(5)
		w.EncodeZ32(v)
		r := NewReader(w.Bytes())
		got, err := r.DecodeZ32()
		if err != nil {
			t.Fatalf("DecodeZ32: %v", err)
		}
		if got != v {
			t.Errorf("roundtrip %d: got %d", v, got)
		}
	}
}

func FuzzDecodeZ64(f *testing.F) {
	for _, g := range vleGolden {
		f.Add(g.expected)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		r := NewReader(input)
		_, _ = r.DecodeZ64() // must not panic
	})
}
