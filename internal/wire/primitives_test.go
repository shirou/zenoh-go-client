package wire

import (
	"bytes"
	"testing"

	"github.com/shirou/zenoh-go-client/internal/codec"
)

func TestZIDRoundtrip(t *testing.T) {
	sizes := []int{1, 4, 8, 16}
	for _, n := range sizes {
		orig := make([]byte, n)
		for i := range orig {
			orig[i] = byte(0xC0 + i)
		}
		w := codec.NewWriter(32)
		if err := EncodePackedZIDByte(w, n, WhatAmIClient); err != nil {
			t.Fatal(err)
		}
		if err := EncodeZIDBytes(w, ZenohID{Bytes: orig}, n); err != nil {
			t.Fatal(err)
		}
		r := codec.NewReader(w.Bytes())
		gotLen, gotWAI, err := DecodePackedZIDByte(r)
		if err != nil {
			t.Fatal(err)
		}
		if gotLen != n {
			t.Errorf("zid_len: got %d, want %d", gotLen, n)
		}
		if gotWAI != WhatAmIClient {
			t.Errorf("WAI: got %d, want Client", gotWAI)
		}
		gotZID, err := DecodeZIDBytes(r, gotLen)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gotZID.Bytes, orig) {
			t.Errorf("ZID bytes mismatch: got % x, want % x", gotZID.Bytes, orig)
		}
	}
}

func TestZIDInvalidLen(t *testing.T) {
	w := codec.NewWriter(4)
	if err := EncodePackedZIDByte(w, 0, WhatAmIClient); err == nil {
		t.Error("expected error for zid_len=0")
	}
	if err := EncodePackedZIDByte(w, 17, WhatAmIClient); err == nil {
		t.Error("expected error for zid_len=17")
	}
}

func TestResolutionDefault(t *testing.T) {
	if byte(DefaultResolution) != 0x0A {
		t.Errorf("default resolution byte = %#x, want 0x0A", byte(DefaultResolution))
	}
	if DefaultResolution.Bits(FieldFrameSN) != 32 {
		t.Errorf("default FSN bits = %d, want 32", DefaultResolution.Bits(FieldFrameSN))
	}
	if DefaultResolution.Bits(FieldRequestID) != 32 {
		t.Errorf("default RID bits = %d, want 32", DefaultResolution.Bits(FieldRequestID))
	}
}

func TestResolutionCustom(t *testing.T) {
	// FSN=16-bit (code 0b01), RID=8-bit (code 0b00)
	res := NewResolution(0b01, 0b00)
	if res.Bits(FieldFrameSN) != 16 {
		t.Errorf("FSN bits = %d, want 16", res.Bits(FieldFrameSN))
	}
	if res.Bits(FieldRequestID) != 8 {
		t.Errorf("RID bits = %d, want 8", res.Bits(FieldRequestID))
	}
	if m := res.Mask(FieldFrameSN); m != 0xFFFF {
		t.Errorf("FSN mask = %#x, want 0xFFFF", m)
	}
}

func TestResolution64Bit(t *testing.T) {
	// All ones means 64-bit; Mask must not overflow.
	res := NewResolution(0b11, 0b11)
	if res.Bits(FieldFrameSN) != 64 {
		t.Errorf("FSN bits = %d", res.Bits(FieldFrameSN))
	}
	if res.Mask(FieldFrameSN) != ^uint64(0) {
		t.Errorf("FSN mask = %#x", res.Mask(FieldFrameSN))
	}
}

func TestWireExprRoundtrip(t *testing.T) {
	cases := []WireExpr{
		{Scope: 0},
		{Scope: 42, Suffix: "demo/example/*"},
		{Scope: 0, Suffix: "full/path"},
	}
	for _, c := range cases {
		w := codec.NewWriter(32)
		if err := c.EncodeScope(w); err != nil {
			t.Fatal(err)
		}
		r := codec.NewReader(w.Bytes())
		got, err := DecodeWireExpr(r, c.Named(), false)
		if err != nil {
			t.Fatal(err)
		}
		if got.Scope != c.Scope || got.Suffix != c.Suffix {
			t.Errorf("roundtrip: got %+v, want %+v", got, c)
		}
	}
}

func TestTimestampRoundtrip(t *testing.T) {
	ts := Timestamp{
		NTP64: 0x1234567890ABCDEF,
		ZID:   ZenohID{Bytes: []byte{1, 2, 3, 4, 5, 6, 7, 8}},
	}
	w := codec.NewWriter(32)
	if err := ts.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	r := codec.NewReader(w.Bytes())
	got, err := DecodeTimestamp(r)
	if err != nil {
		t.Fatal(err)
	}
	if got.NTP64 != ts.NTP64 {
		t.Errorf("NTP64 mismatch: got %x", got.NTP64)
	}
	if !got.ZID.Equal(ts.ZID) {
		t.Errorf("ZID mismatch: got % x", got.ZID.Bytes)
	}
}

func TestEncodingRoundtrip(t *testing.T) {
	cases := []Encoding{
		{ID: 0, Schema: ""},
		{ID: 42, Schema: ""},
		{ID: 42, Schema: "application/json"},
		{ID: MaxEncodingID, Schema: "max"},
	}
	for _, c := range cases {
		w := codec.NewWriter(32)
		if err := c.EncodeTo(w); err != nil {
			t.Fatal(err)
		}
		r := codec.NewReader(w.Bytes())
		got, err := DecodeEncoding(r)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != c.ID || got.Schema != c.Schema {
			t.Errorf("roundtrip: got %+v, want %+v", got, c)
		}
	}
}

// TestEncodingIDRangeRejected verifies that an ID > MaxEncodingID is
// rejected rather than silently truncated by the 31-bit shift.
func TestEncodingIDRangeRejected(t *testing.T) {
	w := codec.NewWriter(8)
	if err := (Encoding{ID: MaxEncodingID + 1}).EncodeTo(w); err == nil {
		t.Error("expected error for ID > MaxEncodingID")
	}
	if err := (Encoding{ID: 0xFFFFFFFF}).EncodeTo(w); err == nil {
		t.Error("expected error for ID = 0xFFFFFFFF")
	}
}
