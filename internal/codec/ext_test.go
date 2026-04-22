package codec

import (
	"bytes"
	"testing"
)

func TestExtHeaderPackUnpack(t *testing.T) {
	tests := []struct {
		h     ExtHeader
		wantB byte
	}{
		{ExtHeader{ID: 0x1, Encoding: ExtEncUnit}, 0x01},
		{ExtHeader{ID: 0x3, Encoding: ExtEncZBuf, Mandatory: true}, 0x53}, // M=1, ENC=0b10
		{ExtHeader{ID: 0x1, Encoding: ExtEncZ64, More: true}, 0xA1},       // Z=1, ENC=0b01
		{ExtHeader{ID: 0xF, Encoding: ExtEncZBuf, Mandatory: true, More: true}, 0xDF},
	}
	for _, tt := range tests {
		b := PackExtHeader(tt.h)
		if b != tt.wantB {
			t.Errorf("PackExtHeader(%+v) = %#x, want %#x", tt.h, b, tt.wantB)
		}
		if got := UnpackExtHeader(b); got != tt.h {
			t.Errorf("UnpackExtHeader(%#x) = %+v, want %+v", b, got, tt.h)
		}
	}
}

func TestEncodeDecodeExtensionUnit(t *testing.T) {
	w := NewWriter(4)
	ext := Extension{Header: ExtHeader{ID: 0x1, Encoding: ExtEncUnit}}
	if err := w.EncodeExtension(ext); err != nil {
		t.Fatal(err)
	}
	r := NewReader(w.Bytes())
	got, err := r.DecodeExtension()
	if err != nil {
		t.Fatal(err)
	}
	if got.Header.ID != 0x1 || got.Header.Encoding != ExtEncUnit {
		t.Errorf("unexpected ext: %+v", got)
	}
	if r.Len() != 0 {
		t.Errorf("%d bytes left unread", r.Len())
	}
}

func TestEncodeDecodeExtensionZ64(t *testing.T) {
	w := NewWriter(10)
	ext := Extension{
		Header: ExtHeader{ID: 0x7, Encoding: ExtEncZ64},
		Z64:    1,
	}
	if err := w.EncodeExtension(ext); err != nil {
		t.Fatal(err)
	}
	r := NewReader(w.Bytes())
	got, err := r.DecodeExtension()
	if err != nil {
		t.Fatal(err)
	}
	if got.Z64 != 1 || got.Header.ID != 0x7 {
		t.Errorf("got %+v", got)
	}
}

func TestEncodeDecodeExtensionZBuf(t *testing.T) {
	payload := []byte("hello")
	w := NewWriter(16)
	ext := Extension{
		Header: ExtHeader{ID: 0x3, Encoding: ExtEncZBuf, Mandatory: true},
		ZBuf:   payload,
	}
	if err := w.EncodeExtension(ext); err != nil {
		t.Fatal(err)
	}
	r := NewReader(w.Bytes())
	got, err := r.DecodeExtension()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.ZBuf, payload) {
		t.Errorf("ZBuf mismatch: got % x, want % x", got.ZBuf, payload)
	}
	if !got.Header.Mandatory {
		t.Error("mandatory flag lost")
	}
}

func TestExtensionChain(t *testing.T) {
	w := NewWriter(32)
	chain := []Extension{
		{Header: ExtHeader{ID: 0x1, Encoding: ExtEncUnit}},
		{Header: ExtHeader{ID: 0x2, Encoding: ExtEncZ64}, Z64: 42},
		{Header: ExtHeader{ID: 0x3, Encoding: ExtEncZBuf}, ZBuf: []byte("xyz")},
	}
	if err := w.EncodeExtensions(chain); err != nil {
		t.Fatal(err)
	}
	r := NewReader(w.Bytes())
	got, err := r.DecodeExtensions()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d extensions, want 3", len(got))
	}
	// More flag recomputed: first two set, last cleared.
	if !got[0].Header.More || !got[1].Header.More || got[2].Header.More {
		t.Errorf("More flags wrong: %v", []bool{got[0].Header.More, got[1].Header.More, got[2].Header.More})
	}
	if got[1].Z64 != 42 {
		t.Errorf("Z64 = %d", got[1].Z64)
	}
	if !bytes.Equal(got[2].ZBuf, []byte("xyz")) {
		t.Errorf("ZBuf mismatch")
	}
}
