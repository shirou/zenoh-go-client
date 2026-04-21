package wire

import (
	"bytes"
	"testing"

	"github.com/shirou/zenoh-go-client/internal/codec"
)

// TestGoldenInitSynSpec encodes an INIT SYN assembled from the spec figure
// byte by byte and verifies our encoder produces the same bytes AND our
// decoder recovers the expected struct. This catches byte-layout mistakes
// that a pure encode→decode self-roundtrip would miss.
//
// Fixture is hand-built from docs/modules/session/pages/open-accept.adoc §INIT.
// Once Phase 2 can capture a real INIT SYN from zenoh-rust via tcpdump, this
// fixture will be replaced with the real capture for true interop assurance.
func TestGoldenInitSynSpec(t *testing.T) {
	// INIT SYN with:
	//   A=0 (Syn), S=1 (resolution+batch), Z=1 (one QoS Unit extension)
	//   version = 0x09
	//   zid_len=4 (encoded 3 in bits 7:4), WAI=Client (0b10)
	//   ZID = 0x11 0x22 0x33 0x44
	//   resolution byte = 0x0A (default: FSN 32-bit, RID 32-bit)
	//   batch_size = 65535 (0xFF 0xFF LE)
	//   QoS Unit extension: ID=0x1, ENC=Unit, M=0, Z=0 → byte 0x01
	want := []byte{
		// header
		// Z=1, S=1, A=0, ID=0x01 → 0b_1_1_0_00001 = 0xC1
		0xC1,
		// version
		0x09,
		// packed byte: zid_len=(4-1)=0011, X=00, WAI=10 → 0b0011_0010 = 0x32
		0x32,
		// ZID bytes
		0x11, 0x22, 0x33, 0x44,
		// resolution
		0x0A,
		// batch_size LE (65535)
		0xFF, 0xFF,
		// QoS Unit extension (no cookie because A=0)
		0x01,
	}

	msg := &Init{
		Ack:         false,
		Version:     ProtoVersion,
		ZID:         ZenohID{Bytes: []byte{0x11, 0x22, 0x33, 0x44}},
		WhatAmI:     WhatAmIClient,
		HasSizeInfo: true,
		Resolution:  DefaultResolution,
		BatchSize:   65535,
		Extensions: []codec.Extension{
			{Header: codec.ExtHeader{ID: 0x1, Encoding: codec.ExtEncUnit}},
		},
	}

	w := codec.NewWriter(32)
	if err := msg.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(w.Bytes(), want) {
		t.Errorf("encoded bytes mismatch:\n got: % X\nwant: % X", w.Bytes(), want)
	}

	// Decode the fixture independently.
	r := codec.NewReader(want)
	h, err := r.DecodeHeader()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeInit(r, h)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != ProtoVersion {
		t.Errorf("version = %#x", got.Version)
	}
	if !got.ZID.Equal(msg.ZID) {
		t.Errorf("ZID mismatch")
	}
	if got.WhatAmI != WhatAmIClient {
		t.Errorf("WhatAmI = %d", got.WhatAmI)
	}
	if !got.HasSizeInfo || got.BatchSize != 65535 {
		t.Errorf("size info = %v/%d", got.HasSizeInfo, got.BatchSize)
	}
	if got.Resolution != DefaultResolution {
		t.Errorf("resolution = %#x", got.Resolution)
	}
	if len(got.Extensions) != 1 || got.Extensions[0].Header.ID != 0x1 {
		t.Errorf("extensions = %+v", got.Extensions)
	}
}

// TestGoldenCloseConnectionToSelf verifies CLOSE with reason=0x07
// encodes to the exact 3-byte sequence expected by the spec.
func TestGoldenCloseConnectionToSelf(t *testing.T) {
	// S=1 (whole session), Z=0, ID=0x03 → 0b_0_0_1_00011 = 0x23
	want := []byte{0x23, 0x07}

	msg := &Close{Session: true, Reason: 0x07}
	w := codec.NewWriter(8)
	if err := msg.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(w.Bytes(), want) {
		t.Errorf("CLOSE bytes:\n got: % X\nwant: % X", w.Bytes(), want)
	}
}

// TestGoldenKeepAliveEmpty — KEEPALIVE with no body must be a single byte.
func TestGoldenKeepAliveEmpty(t *testing.T) {
	want := []byte{0x04} // ID=0x04, all flags 0

	w := codec.NewWriter(4)
	if err := (&KeepAlive{}).EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(w.Bytes(), want) {
		t.Errorf("KEEPALIVE bytes: got % X, want % X", w.Bytes(), want)
	}
}

// TestGoldenInterestFinalByteLength — INTEREST Final is 2 bytes (header + id).
// Verifies that the options byte is NOT emitted for Final mode.
func TestGoldenInterestFinalByteLength(t *testing.T) {
	msg := &Interest{InterestID: 5, Mode: InterestModeFinal}
	w := codec.NewWriter(4)
	if err := msg.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	if w.Len() != 2 {
		t.Errorf("INTEREST Final length = %d, want 2 (header + id)", w.Len())
	}
	// header: Mod=00, Z=0, ID=0x19
	if w.Bytes()[0] != 0x19 {
		t.Errorf("header = %#x, want 0x19", w.Bytes()[0])
	}
	if w.Bytes()[1] != 5 {
		t.Errorf("id = %#x, want 0x05", w.Bytes()[1])
	}
}

// TestGoldenQoSExtBlockingPublish — QoS extension z64 payload for a Block
// congestion control, Data priority PUSH. D=1, prio=5 → low byte 0x0D.
func TestGoldenQoSExtBlockingPublish(t *testing.T) {
	q := QoS{Priority: QoSPriorityData, DontDrop: true}
	if got := q.EncodeZ64(); got != 0x0D {
		t.Errorf("QoS z64 = %#x, want 0x0D", got)
	}
}
