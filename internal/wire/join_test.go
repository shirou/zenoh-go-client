package wire

import (
	"reflect"
	"testing"

	"github.com/shirou/zenoh-go-client/internal/codec"
)

// TestJoinEncodeDecodeBasic exercises the round-trip without lane SNs.
func TestJoinEncodeDecodeBasic(t *testing.T) {
	src := &Join{
		Version:     ProtoVersion,
		ZID:         ZenohID{Bytes: []byte{0xAA, 0xBB, 0xCC}},
		WhatAmI:     WhatAmIPeer,
		HasSizeInfo: true,
		Resolution:  DefaultResolution,
		BatchSize:   8192,
		Lease:       10000,
		NextSNRel:   42,
		NextSNBE:    43,
	}
	w := codec.NewWriter(64)
	if err := src.EncodeTo(w); err != nil {
		t.Fatalf("encode: %v", err)
	}
	r := codec.NewReader(w.Bytes())
	h, err := r.DecodeHeader()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeJoin(r, h)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(src, got) {
		t.Errorf("roundtrip mismatch\n got: %#v\nwant: %#v", got, src)
	}
}

// TestJoinLaneSNsRoundtrip attaches a populated LaneSNs to a JOIN and
// recovers it on decode.
func TestJoinLaneSNsRoundtrip(t *testing.T) {
	sns := LaneSNs{
		Reliable:   [8]uint64{1, 2, 3, 4, 5, 6, 7, 8},
		BestEffort: [8]uint64{100, 200, 300, 400, 500, 600, 700, 800},
	}
	src := &Join{
		Version: ProtoVersion,
		ZID:     ZenohID{Bytes: []byte{0x42}},
		WhatAmI: WhatAmIPeer,
		Lease:   10000,
	}
	if err := src.AttachLaneSNs(sns); err != nil {
		t.Fatalf("AttachLaneSNs: %v", err)
	}

	w := codec.NewWriter(128)
	if err := src.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	r := codec.NewReader(w.Bytes())
	h, err := r.DecodeHeader()
	if err != nil {
		t.Fatal(err)
	}
	dec, err := DecodeJoin(r, h)
	if err != nil {
		t.Fatal(err)
	}
	got, present, err := dec.DecodeLaneSNs()
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Fatal("LaneSNs ext not present after roundtrip")
	}
	if !reflect.DeepEqual(*got, sns) {
		t.Errorf("LaneSNs roundtrip mismatch\n got: %#v\nwant: %#v", got, sns)
	}
}

// TestJoinDecodeLaneSNsAbsent: no QoS ZBuf ext → (nil, false, nil).
func TestJoinDecodeLaneSNsAbsent(t *testing.T) {
	j := &Join{Version: ProtoVersion, ZID: ZenohID{Bytes: []byte{0x01}}, Lease: 1000}
	got, present, err := j.DecodeLaneSNs()
	if err != nil {
		t.Fatalf("DecodeLaneSNs: %v", err)
	}
	if present || got != nil {
		t.Errorf("absent ext should return (nil,false,nil); got %v %v", got, present)
	}
}

// TestJoinDecodeLaneSNsCorrupt: a QoS ZBuf with too few bytes errors.
func TestJoinDecodeLaneSNsCorrupt(t *testing.T) {
	j := &Join{
		Extensions: []codec.Extension{
			{
				Header: codec.ExtHeader{ID: ExtIDQoS, Encoding: codec.ExtEncZBuf, Mandatory: true},
				ZBuf:   []byte{0x01, 0x02}, // way too short
			},
		},
	}
	_, _, err := j.DecodeLaneSNs()
	if err == nil {
		t.Error("expected error from truncated lane-SN ext")
	}
}
