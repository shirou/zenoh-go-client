package codec

import (
	"bytes"
	"errors"
	"testing"
)

func TestEncodeBytesZ8Roundtrip(t *testing.T) {
	cases := [][]byte{
		nil,
		{},
		{0xDE, 0xAD, 0xBE, 0xEF},
		bytes.Repeat([]byte{0xAB}, 255),
	}
	for _, c := range cases {
		w := NewWriter(8)
		if err := w.EncodeBytesZ8(c); err != nil {
			t.Fatalf("EncodeBytesZ8: %v", err)
		}
		r := NewReader(w.Bytes())
		got, err := r.DecodeBytesZ8()
		if err != nil {
			t.Fatalf("DecodeBytesZ8: %v", err)
		}
		if !bytes.Equal(got, c) && !(len(got) == 0 && len(c) == 0) {
			t.Errorf("roundtrip mismatch: got % x, want % x", got, c)
		}
	}
}

func TestEncodeBytesZ8TooLarge(t *testing.T) {
	w := NewWriter(8)
	if err := w.EncodeBytesZ8(make([]byte, 256)); err == nil {
		t.Error("expected error for 256-byte slice")
	}
}

func TestEncodeStringZ16(t *testing.T) {
	s := "hello 世界"
	w := NewWriter(16)
	if err := w.EncodeStringZ16(s); err != nil {
		t.Fatal(err)
	}
	r := NewReader(w.Bytes())
	got, err := r.DecodeStringZ16()
	if err != nil {
		t.Fatal(err)
	}
	if got != s {
		t.Errorf("got %q, want %q", got, s)
	}
}

func TestEncodeStringInvalidUTF8(t *testing.T) {
	w := NewWriter(4)
	if err := w.EncodeStringZ8(string([]byte{0xFF, 0xFE})); err == nil {
		t.Error("expected UTF-8 validation error")
	}
}

func TestDecodeBytesZ16Truncated(t *testing.T) {
	// Length prefix says 5, only 2 payload bytes → ErrUnexpectedEOF.
	r := NewReader([]byte{0x05, 0x01, 0x02})
	if _, err := r.DecodeBytesZ16(); !errors.Is(err, ErrUnexpectedEOF) {
		t.Errorf("expected ErrUnexpectedEOF, got %v", err)
	}
}
