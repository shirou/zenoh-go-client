package wire

import (
	"bytes"
	"testing"

	"github.com/shirou/zenoh-go-client/internal/codec"
)

func TestRequestRoundtrip(t *testing.T) {
	cm := ConsolidationNone
	orig := &Request{
		RequestID: 42,
		KeyExpr:   WireExpr{Scope: 0, Suffix: "demo/example/*"},
		Body: &QueryBody{
			Consolidation: &cm,
			Parameters:    "foo=bar&baz=1",
		},
	}
	w := codec.NewWriter(64)
	if err := orig.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	r := codec.NewReader(w.Bytes())
	h, _ := r.DecodeHeader()
	got, err := DecodeRequest(r, h)
	if err != nil {
		t.Fatal(err)
	}
	if got.RequestID != 42 {
		t.Errorf("RequestID = %d", got.RequestID)
	}
	if got.KeyExpr.Suffix != "demo/example/*" {
		t.Errorf("KeyExpr = %q", got.KeyExpr.Suffix)
	}
	if got.Body.Consolidation == nil || *got.Body.Consolidation != ConsolidationNone {
		t.Errorf("Consolidation lost: %+v", got.Body.Consolidation)
	}
	if got.Body.Parameters != "foo=bar&baz=1" {
		t.Errorf("Parameters = %q", got.Body.Parameters)
	}
}

func TestRequestExtensionsRoundtrip(t *testing.T) {
	orig := &Request{
		RequestID: 7,
		KeyExpr:   WireExpr{Scope: 0, Suffix: "demo/**"},
		Extensions: []codec.Extension{
			QueryTargetExt(QueryTargetAll),
			BudgetExt(16),
			TimeoutExt(5000),
		},
		Body: &QueryBody{},
	}
	w := codec.NewWriter(64)
	if err := orig.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	r := codec.NewReader(w.Bytes())
	h, _ := r.DecodeHeader()
	got, err := DecodeRequest(r, h)
	if err != nil {
		t.Fatal(err)
	}
	if e := codec.FindExt(got.Extensions, ReqExtIDQueryTarget); e == nil || QueryTarget(e.Z64) != QueryTargetAll {
		t.Errorf("QueryTarget ext lost: %+v", e)
	}
	if e := codec.FindExt(got.Extensions, ReqExtIDBudget); e == nil || uint32(e.Z64) != 16 {
		t.Errorf("Budget ext lost: %+v", e)
	}
	if e := codec.FindExt(got.Extensions, ReqExtIDTimeout); e == nil || e.Z64 != 5000 {
		t.Errorf("Timeout ext lost: %+v", e)
	}
}

func TestResponseReplyRoundtrip(t *testing.T) {
	orig := &Response{
		RequestID: 7,
		KeyExpr:   WireExpr{Scope: 0, Suffix: "demo/ok"},
		Body: &ReplyBody{
			Inner: &PutBody{Payload: []byte("ack")},
		},
	}
	w := codec.NewWriter(32)
	if err := orig.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	r := codec.NewReader(w.Bytes())
	h, _ := r.DecodeHeader()
	got, err := DecodeResponse(r, h)
	if err != nil {
		t.Fatal(err)
	}
	reply, ok := got.Body.(*ReplyBody)
	if !ok {
		t.Fatalf("body type = %T, want *ReplyBody", got.Body)
	}
	put, ok := reply.Inner.(*PutBody)
	if !ok {
		t.Fatalf("inner type = %T, want *PutBody", reply.Inner)
	}
	if !bytes.Equal(put.Payload, []byte("ack")) {
		t.Errorf("payload = %q", put.Payload)
	}
}

func TestResponseErrRoundtrip(t *testing.T) {
	enc := Encoding{ID: 4, Schema: "text/plain"}
	orig := &Response{
		RequestID: 9,
		KeyExpr:   WireExpr{Scope: 0, Suffix: "demo/err"},
		Body: &ErrBody{
			Encoding: &enc,
			Payload:  []byte("boom"),
		},
	}
	w := codec.NewWriter(32)
	if err := orig.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	r := codec.NewReader(w.Bytes())
	h, _ := r.DecodeHeader()
	got, err := DecodeResponse(r, h)
	if err != nil {
		t.Fatal(err)
	}
	errBody, ok := got.Body.(*ErrBody)
	if !ok {
		t.Fatalf("body type = %T, want *ErrBody", got.Body)
	}
	if errBody.Encoding == nil || errBody.Encoding.Schema != "text/plain" {
		t.Errorf("encoding = %+v", errBody.Encoding)
	}
	if string(errBody.Payload) != "boom" {
		t.Errorf("payload = %q", errBody.Payload)
	}
}

func TestResponseFinalRoundtrip(t *testing.T) {
	orig := &ResponseFinal{RequestID: 99}
	w := codec.NewWriter(8)
	if err := orig.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	r := codec.NewReader(w.Bytes())
	h, _ := r.DecodeHeader()
	got, err := DecodeResponseFinal(r, h)
	if err != nil {
		t.Fatal(err)
	}
	if got.RequestID != 99 {
		t.Errorf("RequestID = %d", got.RequestID)
	}
}
