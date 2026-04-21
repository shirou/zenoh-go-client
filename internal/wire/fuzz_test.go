package wire

import (
	"testing"

	"github.com/shirou/zenoh-go-client/internal/codec"
)

// Each FuzzDecode* target decodes arbitrary bytes and asserts the decoder
// never panics. Seeds are the bytes produced by our own encoders to give the
// engine a valid starting point; the fuzzer mutates from there.
//
// The common pattern is:
//   1. read a header byte
//   2. if the expected ID matches, run the decoder
//   3. swallow the error — we only care that we don't panic

func fuzzDecode(f *testing.F, seedMsgs []codec.Encoder, wantID byte,
	decode func(*codec.Reader, codec.Header) error) {
	for _, m := range seedMsgs {
		w := codec.NewWriter(64)
		if err := m.EncodeTo(w); err != nil {
			f.Fatalf("seed encode: %v", err)
		}
		f.Add(w.Bytes())
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		r := codec.NewReader(input)
		h, err := r.DecodeHeader()
		if err != nil {
			return
		}
		if h.ID != wantID {
			return
		}
		_ = decode(r, h)
	})
}

func FuzzDecodeInit(f *testing.F) {
	seeds := []codec.Encoder{
		&Init{Version: ProtoVersion, ZID: ZenohID{Bytes: []byte{1, 2, 3, 4}}, WhatAmI: WhatAmIClient, HasSizeInfo: true, Resolution: DefaultResolution, BatchSize: 65535},
		&Init{Ack: true, Version: ProtoVersion, ZID: ZenohID{Bytes: []byte{5, 6, 7, 8}}, WhatAmI: WhatAmIRouter, HasSizeInfo: true, Resolution: DefaultResolution, BatchSize: 8192, Cookie: []byte("c")},
	}
	fuzzDecode(f, seeds, IDTransportInit, func(r *codec.Reader, h codec.Header) error {
		_, err := DecodeInit(r, h)
		return err
	})
}

func FuzzDecodeOpen(f *testing.F) {
	seeds := []codec.Encoder{
		&Open{Lease: 10000, InitialSN: 42, Cookie: []byte("c")},
		&Open{Ack: true, Lease: 10, InitialSN: 0},
	}
	fuzzDecode(f, seeds, IDTransportOpen, func(r *codec.Reader, h codec.Header) error {
		_, err := DecodeOpen(r, h)
		return err
	})
}

func FuzzDecodeClose(f *testing.F) {
	seeds := []codec.Encoder{
		&Close{Session: true, Reason: 0x00},
		&Close{Session: false, Reason: 0x07},
	}
	fuzzDecode(f, seeds, IDTransportClose, func(r *codec.Reader, h codec.Header) error {
		_, err := DecodeClose(r, h)
		return err
	})
}

func FuzzDecodeKeepAlive(f *testing.F) {
	seeds := []codec.Encoder{&KeepAlive{}}
	fuzzDecode(f, seeds, IDTransportKeepAlive, func(r *codec.Reader, h codec.Header) error {
		_, err := DecodeKeepAlive(r, h)
		return err
	})
}

func FuzzDecodeFrame(f *testing.F) {
	seeds := []codec.Encoder{
		&Frame{Reliable: true, SeqNum: 1, Body: []byte{}},
		&Frame{Reliable: false, SeqNum: 42, Body: []byte{0x01, 0x02}},
	}
	fuzzDecode(f, seeds, IDTransportFrame, func(r *codec.Reader, h codec.Header) error {
		_, err := DecodeFrame(r, h)
		return err
	})
}

func FuzzDecodeFragment(f *testing.F) {
	seeds := []codec.Encoder{
		&Fragment{Reliable: true, More: true, SeqNum: 1, Body: []byte("chunk")},
		&Fragment{Reliable: false, More: false, SeqNum: 2, Body: []byte{}},
	}
	fuzzDecode(f, seeds, IDTransportFragment, func(r *codec.Reader, h codec.Header) error {
		_, err := DecodeFragment(r, h)
		return err
	})
}

func FuzzDecodePush(f *testing.F) {
	seeds := []codec.Encoder{
		&Push{KeyExpr: WireExpr{Scope: 0, Suffix: "demo/*"}, Body: &PutBody{Payload: []byte("hi")}},
		&Push{KeyExpr: WireExpr{Scope: 0, Suffix: "x"}, Body: &DelBody{}},
	}
	fuzzDecode(f, seeds, IDNetworkPush, func(r *codec.Reader, h codec.Header) error {
		_, err := DecodePush(r, h)
		return err
	})
}

func FuzzDecodeRequest(f *testing.F) {
	cm := ConsolidationLatest
	seeds := []codec.Encoder{
		&Request{RequestID: 1, KeyExpr: WireExpr{Scope: 0, Suffix: "demo/**"}, Body: &QueryBody{Consolidation: &cm, Parameters: "q=1"}},
		&Request{RequestID: 9, KeyExpr: WireExpr{Scope: 0, Suffix: "x"}, Extensions: []codec.Extension{QueryTargetExt(QueryTargetAll)}, Body: &QueryBody{}},
	}
	fuzzDecode(f, seeds, IDNetworkRequest, func(r *codec.Reader, h codec.Header) error {
		_, err := DecodeRequest(r, h)
		return err
	})
}

func FuzzDecodeResponse(f *testing.F) {
	seeds := []codec.Encoder{
		&Response{RequestID: 1, KeyExpr: WireExpr{Scope: 0, Suffix: "demo"}, Body: &ReplyBody{Inner: &PutBody{Payload: []byte("ok")}}},
		&Response{RequestID: 2, KeyExpr: WireExpr{Scope: 0, Suffix: "err"}, Body: &ErrBody{Payload: []byte("boom")}},
	}
	fuzzDecode(f, seeds, IDNetworkResponse, func(r *codec.Reader, h codec.Header) error {
		_, err := DecodeResponse(r, h)
		return err
	})
}

func FuzzDecodeResponseFinal(f *testing.F) {
	seeds := []codec.Encoder{&ResponseFinal{RequestID: 42}}
	fuzzDecode(f, seeds, IDNetworkResponseFinal, func(r *codec.Reader, h codec.Header) error {
		_, err := DecodeResponseFinal(r, h)
		return err
	})
}

func FuzzDecodeDeclare(f *testing.F) {
	seeds := []codec.Encoder{
		&Declare{Body: NewDeclareSubscriber(1, WireExpr{Scope: 0, Suffix: "demo/**"})},
		&Declare{Body: NewDeclareQueryable(2, WireExpr{Scope: 0, Suffix: "q/**"})},
		&Declare{Body: NewDeclareToken(3, WireExpr{Scope: 0, Suffix: "tok"})},
		&Declare{Body: &UndeclareEntity{Kind: IDUndeclareSubscriber, EntityID: 1, WireExpr: WireExpr{Scope: 0, Suffix: "demo/**"}}},
	}
	fuzzDecode(f, seeds, IDNetworkDeclare, func(r *codec.Reader, h codec.Header) error {
		_, err := DecodeDeclare(r, h)
		return err
	})
}

func FuzzDecodeInterest(f *testing.F) {
	ke := WireExpr{Scope: 0, Suffix: "**"}
	seeds := []codec.Encoder{
		&Interest{InterestID: 1, Mode: InterestModeFinal},
		&Interest{InterestID: 2, Mode: InterestModeCurrentFuture, KeyExpr: &ke, Filter: InterestFilter{KeyExprs: true, Subscribers: true}},
	}
	fuzzDecode(f, seeds, IDNetworkInterest, func(r *codec.Reader, h codec.Header) error {
		_, err := DecodeInterest(r, h)
		return err
	})
}

func FuzzDecodeScout(f *testing.F) {
	seeds := []codec.Encoder{
		&Scout{Version: ProtoVersion, Matcher: MatcherRouter},
	}
	fuzzDecode(f, seeds, IDScoutScout, func(r *codec.Reader, h codec.Header) error {
		_, err := DecodeScout(r, h)
		return err
	})
}

func FuzzDecodeHello(f *testing.F) {
	seeds := []codec.Encoder{
		&Hello{Version: ProtoVersion, ZID: ZenohID{Bytes: []byte{1, 2, 3}}, WhatAmI: WhatAmIRouter},
	}
	fuzzDecode(f, seeds, IDScoutHello, func(r *codec.Reader, h codec.Header) error {
		_, err := DecodeHello(r, h)
		return err
	})
}

func FuzzDecodeJoin(f *testing.F) {
	seeds := []codec.Encoder{
		&Join{Version: ProtoVersion, ZID: ZenohID{Bytes: []byte{9, 8, 7}}, WhatAmI: WhatAmIPeer, HasSizeInfo: true, Resolution: DefaultResolution, BatchSize: 8192, Lease: 10_000},
		&Join{LeaseSeconds: true, Version: ProtoVersion, ZID: ZenohID{Bytes: []byte{1}}, WhatAmI: WhatAmIRouter},
	}
	fuzzDecode(f, seeds, IDTransportJoin, func(r *codec.Reader, h codec.Header) error {
		_, err := DecodeJoin(r, h)
		return err
	})
}

// The remaining decoders are not gated by a header ID — they read directly
// from a Reader positioned at their payload. Fuzz them by feeding raw
// bytes into the decoder and asserting no panic.

func FuzzDecodeEncoding(f *testing.F) {
	seeds := []Encoding{
		{ID: 0},
		{ID: 4, Schema: "utf-8"},
		{ID: 52, Schema: ""},
	}
	for _, e := range seeds {
		w := codec.NewWriter(16)
		if err := e.EncodeTo(w); err != nil {
			f.Fatalf("seed encode: %v", err)
		}
		f.Add(w.Bytes())
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		r := codec.NewReader(input)
		_, _ = DecodeEncoding(r)
	})
}

func FuzzDecodeTimestamp(f *testing.F) {
	seeds := []Timestamp{
		{NTP64: 0, ZID: ZenohID{Bytes: []byte{1}}},
		{NTP64: 1 << 40, ZID: ZenohID{Bytes: []byte{1, 2, 3, 4, 5, 6, 7, 8}}},
	}
	for _, ts := range seeds {
		w := codec.NewWriter(16)
		if err := ts.EncodeTo(w); err != nil {
			f.Fatalf("seed encode: %v", err)
		}
		f.Add(w.Bytes())
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		r := codec.NewReader(input)
		_, _ = DecodeTimestamp(r)
	})
}

func FuzzDecodeWireExpr(f *testing.F) {
	// Seeds: a named+scope=0 expr, and a named+scope=42 expr.
	seeds := []WireExpr{
		{Scope: 0, Suffix: "demo/**"},
		{Scope: 42, Suffix: ""},
		{Scope: 100, Suffix: "a/b/c"},
	}
	for _, ke := range seeds {
		w := codec.NewWriter(16)
		if err := ke.EncodeScope(w); err != nil {
			f.Fatalf("seed encode: %v", err)
		}
		f.Add(w.Bytes())
	}
	// named=true so the suffix is read; mapping bit doesn't alter decoding shape.
	f.Fuzz(func(t *testing.T, input []byte) {
		r := codec.NewReader(input)
		_, _ = DecodeWireExpr(r, true, false)
	})
}
