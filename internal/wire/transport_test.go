package wire

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/shirou/zenoh-go-client/internal/codec"
)

// Build an INIT SYN with a QoS Unit extension (ext 0x1) and ensure it
// roundtrips byte-for-byte.
func TestInitSynWithQoSExtension(t *testing.T) {
	zid := ZenohID{Bytes: []byte{1, 2, 3, 4, 5, 6, 7, 8}}
	qosExt := codec.Extension{
		Header: codec.ExtHeader{ID: 0x1, Encoding: codec.ExtEncUnit},
	}
	orig := &Init{
		Ack:         false,
		Version:     ProtoVersion,
		ZID:         zid,
		WhatAmI:     WhatAmIClient,
		HasSizeInfo: true,
		Resolution:  DefaultResolution,
		BatchSize:   65535,
		Extensions:  []codec.Extension{qosExt},
	}
	w := codec.NewWriter(64)
	if err := orig.EncodeTo(w); err != nil {
		t.Fatal(err)
	}

	r := codec.NewReader(w.Bytes())
	h, err := r.DecodeHeader()
	if err != nil {
		t.Fatal(err)
	}
	if h.ID != IDTransportInit {
		t.Fatalf("header ID = %#x", h.ID)
	}
	got, err := DecodeInit(r, h)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != ProtoVersion {
		t.Errorf("version = %#x", got.Version)
	}
	if !got.ZID.Equal(zid) {
		t.Errorf("ZID mismatch")
	}
	if got.WhatAmI != WhatAmIClient {
		t.Errorf("WhatAmI = %d", got.WhatAmI)
	}
	if got.BatchSize != 65535 {
		t.Errorf("BatchSize = %d", got.BatchSize)
	}
	if got.Resolution != DefaultResolution {
		t.Errorf("Resolution = %#x", got.Resolution)
	}
	if len(got.Extensions) != 1 || got.Extensions[0].Header.ID != 0x1 {
		t.Errorf("extensions = %+v", got.Extensions)
	}
}

func TestInitAckCookie(t *testing.T) {
	orig := &Init{
		Ack:     true,
		Version: ProtoVersion,
		ZID:     ZenohID{Bytes: []byte{0xAA, 0xBB, 0xCC, 0xDD}},
		WhatAmI: WhatAmIRouter,
		Cookie:  []byte{1, 2, 3, 4},
	}
	w := codec.NewWriter(32)
	if err := orig.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	r := codec.NewReader(w.Bytes())
	h, err := r.DecodeHeader()
	if err != nil {
		t.Fatal(err)
	}
	if !h.F1 {
		t.Error("A flag not set on InitAck")
	}
	got, err := DecodeInit(r, h)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Cookie, orig.Cookie) {
		t.Errorf("cookie mismatch: got % x", got.Cookie)
	}
}

func TestOpenRoundtrip(t *testing.T) {
	orig := &Open{
		Ack:       false,
		Lease:     10000, // 10s in ms
		InitialSN: 0xDEADBEEF,
		Cookie:    []byte{0x11, 0x22, 0x33},
	}
	w := codec.NewWriter(32)
	if err := orig.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	r := codec.NewReader(w.Bytes())
	h, err := r.DecodeHeader()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeOpen(r, h)
	if err != nil {
		t.Fatal(err)
	}
	if got.Lease != orig.Lease || got.InitialSN != orig.InitialSN {
		t.Errorf("Open mismatch: got %+v", got)
	}
	if !bytes.Equal(got.Cookie, orig.Cookie) {
		t.Errorf("cookie mismatch")
	}
}

func TestCloseRoundtrip(t *testing.T) {
	orig := &Close{Session: true, Reason: 0x07}
	w := codec.NewWriter(8)
	if err := orig.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	r := codec.NewReader(w.Bytes())
	h, err := r.DecodeHeader()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeClose(r, h)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reason != 0x07 {
		t.Errorf("reason = %#x", got.Reason)
	}
	if !got.Session {
		t.Error("S flag lost")
	}
}

func TestKeepAliveRoundtrip(t *testing.T) {
	w := codec.NewWriter(4)
	if err := (&KeepAlive{}).EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	if w.Len() != 1 {
		t.Errorf("KEEPALIVE empty body length = %d", w.Len())
	}
	r := codec.NewReader(w.Bytes())
	h, _ := r.DecodeHeader()
	if _, err := DecodeKeepAlive(r, h); err != nil {
		t.Fatal(err)
	}
}

func TestFrameRoundtrip(t *testing.T) {
	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	orig := &Frame{
		Reliable: true,
		SeqNum:   12345,
		Body:     payload,
	}
	w := codec.NewWriter(16)
	if err := orig.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	r := codec.NewReader(w.Bytes())
	h, err := r.DecodeHeader()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeFrame(r, h)
	if err != nil {
		t.Fatal(err)
	}
	if got.SeqNum != 12345 || !got.Reliable {
		t.Errorf("Frame mismatch: %+v", got)
	}
	if !bytes.Equal(got.Body, payload) {
		t.Errorf("body mismatch")
	}
}

func TestFragmentRoundtrip(t *testing.T) {
	orig := &Fragment{
		Reliable: true,
		More:     true,
		SeqNum:   999,
		Body:     []byte("partial-message-chunk"),
	}
	w := codec.NewWriter(32)
	if err := orig.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	r := codec.NewReader(w.Bytes())
	h, _ := r.DecodeHeader()
	got, err := DecodeFragment(r, h)
	if err != nil {
		t.Fatal(err)
	}
	if !got.More || got.SeqNum != 999 {
		t.Errorf("Fragment flags/seq wrong: %+v", got)
	}
	if !bytes.Equal(got.Body, orig.Body) {
		t.Errorf("body mismatch")
	}
}

func TestJoinRoundtrip(t *testing.T) {
	orig := &Join{
		LeaseSeconds: false,
		HasSizeInfo:  true,
		Version:      ProtoVersion,
		ZID:          ZenohID{Bytes: []byte{0xA, 0xB, 0xC, 0xD, 0xE, 0xF, 0x1, 0x2}},
		WhatAmI:      WhatAmIPeer,
		Resolution:   DefaultResolution,
		BatchSize:    32768,
		Lease:        10000,
		NextSNRel:    1,
		NextSNBE:     2,
	}
	w := codec.NewWriter(64)
	if err := orig.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	r := codec.NewReader(w.Bytes())
	h, _ := r.DecodeHeader()
	got, err := DecodeJoin(r, h)
	if err != nil {
		t.Fatal(err)
	}
	if got.WhatAmI != WhatAmIPeer || got.BatchSize != 32768 || got.Lease != 10000 {
		t.Errorf("Join mismatch: %+v", got)
	}
}

func TestQoSExtEncodeZ64(t *testing.T) {
	// prio=Data (5), D=1 (Block), E=0, F=0 → low byte 0b00001_101 = 0x0D
	q := QoS{Priority: QoSPriorityData, DontDrop: true}
	if got := q.EncodeZ64(); got != 0x0D {
		t.Errorf("EncodeZ64 = %#x, want 0x0D", got)
	}
	// Roundtrip
	q2 := DecodeQoSZ64(q.EncodeZ64())
	if q2 != q {
		t.Errorf("roundtrip: got %+v, want %+v", q2, q)
	}
}

func TestQoSExtExpress(t *testing.T) {
	q := QoS{Priority: QoSPriorityInteractiveHigh, Express: true}
	// prio=2, E(bit 4)=1 → 0b00010010 = 0x12
	if got := q.EncodeZ64(); got != 0x12 {
		t.Errorf("got %#x, want 0x12", got)
	}
}

// TestFrameFlagVariants exercises every combination of the FRAME header
// flags (Reliable, Z) across the full range of QoSPriority values and of
// the QoS-extension D/E/F bits, making sure every bit that affects lane
// routing survives encode→decode.
//
// FRAME's own header only carries R (Reliable, F1) and Z (extensions);
// the D (DontDrop / CongestionControl) and E (Express) bits live on the
// QoS Z64 extension, so the combined FRAME + QoS-ext roundtrip is what
// the publisher/subscriber contract actually depends on.
func TestFrameFlagVariants(t *testing.T) {
	priorities := []QoSPriority{
		QoSPriorityControl,
		QoSPriorityRealTime,
		QoSPriorityInteractiveHigh,
		QoSPriorityInteractiveLow,
		QoSPriorityDataHigh,
		QoSPriorityData,
		QoSPriorityDataLow,
		QoSPriorityBackground,
	}
	bools := []bool{false, true}

	for _, reliable := range bools {
		for _, prio := range priorities {
			for _, dontDrop := range bools {
				for _, express := range bools {
					for _, dontDropFirst := range bools {
						name := fmt.Sprintf("R=%v/prio=%d/D=%v/E=%v/F=%v",
							reliable, prio, dontDrop, express, dontDropFirst)
						t.Run(name, func(t *testing.T) {
							qos := QoS{
								Priority:      prio,
								DontDrop:      dontDrop,
								Express:       express,
								DontDropFirst: dontDropFirst,
							}
							orig := &Frame{
								Reliable: reliable,
								SeqNum:   0x1234,
								Extensions: []codec.Extension{
									codec.NewZ64Ext(ExtIDQoS, true, qos.EncodeZ64()),
								},
								Body: []byte{0xAA, 0xBB},
							}
							w := codec.NewWriter(16)
							if err := orig.EncodeTo(w); err != nil {
								t.Fatalf("encode: %v", err)
							}
							// Header byte: Z must always be set here because
							// we attach one extension, F1 mirrors Reliable,
							// F2 is reserved (must be 0).
							headerByte := w.Bytes()[0]
							if got := codec.UnpackHeader(headerByte); got.F1 != reliable || got.F2 || !got.Z {
								t.Errorf("header flags = %+v, want F1=%v F2=false Z=true",
									got, reliable)
							}

							r := codec.NewReader(w.Bytes())
							h, err := r.DecodeHeader()
							if err != nil {
								t.Fatal(err)
							}
							got, err := DecodeFrame(r, h)
							if err != nil {
								t.Fatalf("decode: %v", err)
							}
							if got.Reliable != reliable {
								t.Errorf("Reliable = %v, want %v", got.Reliable, reliable)
							}
							if got.SeqNum != orig.SeqNum {
								t.Errorf("SeqNum = %d, want %d", got.SeqNum, orig.SeqNum)
							}
							if !bytes.Equal(got.Body, orig.Body) {
								t.Errorf("body = % x, want % x", got.Body, orig.Body)
							}
							if len(got.Extensions) != 1 {
								t.Fatalf("extensions = %d, want 1", len(got.Extensions))
							}
							if got.Extensions[0].Header.ID != ExtIDQoS {
								t.Errorf("ext ID = %#x, want %#x",
									got.Extensions[0].Header.ID, ExtIDQoS)
							}
							roundtrip := DecodeQoSZ64(got.Extensions[0].Z64)
							if roundtrip != qos {
								t.Errorf("QoS roundtrip = %+v, want %+v", roundtrip, qos)
							}
						})
					}
				}
			}
		}
	}
}

// TestFrameNoExtensions covers the Z=0 branch: no QoS extension attached,
// so the header must decode with Z=false and the extension slice must be
// empty after roundtrip.
func TestFrameNoExtensions(t *testing.T) {
	for _, reliable := range []bool{false, true} {
		orig := &Frame{
			Reliable: reliable,
			SeqNum:   42,
			Body:     []byte{0x00, 0x01, 0x02},
		}
		w := codec.NewWriter(8)
		if err := orig.EncodeTo(w); err != nil {
			t.Fatal(err)
		}
		if got := codec.UnpackHeader(w.Bytes()[0]); got.Z {
			t.Errorf("R=%v: Z should be 0 when no extensions", reliable)
		}
		r := codec.NewReader(w.Bytes())
		h, err := r.DecodeHeader()
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeFrame(r, h)
		if err != nil {
			t.Fatalf("R=%v: decode: %v", reliable, err)
		}
		if got.Reliable != reliable || len(got.Extensions) != 0 {
			t.Errorf("R=%v: got Reliable=%v extensions=%d, want %v/0",
				reliable, got.Reliable, len(got.Extensions), reliable)
		}
		if !bytes.Equal(got.Body, orig.Body) {
			t.Errorf("R=%v: body mismatch", reliable)
		}
	}
}
