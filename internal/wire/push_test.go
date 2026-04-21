package wire

import (
	"bytes"
	"testing"

	"github.com/shirou/zenoh-go-client/internal/codec"
)

func TestPushPutRoundtrip(t *testing.T) {
	ts := Timestamp{
		NTP64: 0x1234567890ABCDEF,
		ZID:   ZenohID{Bytes: []byte{1, 2, 3, 4}},
	}
	orig := &Push{
		KeyExpr: WireExpr{Scope: 0, Suffix: "demo/example/hello"},
		Body: &PutBody{
			Timestamp: &ts,
			Encoding:  &Encoding{ID: 2, Schema: "application/json"},
			Payload:   []byte(`{"hello":"world"}`),
		},
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
	got, err := DecodePush(r, h)
	if err != nil {
		t.Fatal(err)
	}
	if got.KeyExpr.Suffix != "demo/example/hello" {
		t.Errorf("KeyExpr.Suffix = %q", got.KeyExpr.Suffix)
	}
	put, ok := got.Body.(*PutBody)
	if !ok {
		t.Fatalf("body type = %T, want *PutBody", got.Body)
	}
	if !bytes.Equal(put.Payload, orig.Body.(*PutBody).Payload) {
		t.Errorf("payload mismatch")
	}
	if put.Encoding == nil || put.Encoding.ID != 2 || put.Encoding.Schema != "application/json" {
		t.Errorf("encoding lost: %+v", put.Encoding)
	}
	if put.Timestamp == nil || put.Timestamp.NTP64 != ts.NTP64 {
		t.Errorf("timestamp lost")
	}
}

func TestPushDelRoundtrip(t *testing.T) {
	orig := &Push{
		KeyExpr: WireExpr{Scope: 0, Suffix: "demo/x"},
		Body:    &DelBody{},
	}
	w := codec.NewWriter(16)
	if err := orig.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	r := codec.NewReader(w.Bytes())
	h, _ := r.DecodeHeader()
	got, err := DecodePush(r, h)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Body.(*DelBody); !ok {
		t.Fatalf("body type = %T, want *DelBody", got.Body)
	}
}

func TestPushMinimal(t *testing.T) {
	// PUT with no timestamp / no encoding / no extensions.
	orig := &Push{
		KeyExpr: WireExpr{Scope: 0, Suffix: "a"},
		Body:    &PutBody{Payload: []byte("p")},
	}
	w := codec.NewWriter(8)
	if err := orig.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	r := codec.NewReader(w.Bytes())
	h, _ := r.DecodeHeader()
	got, err := DecodePush(r, h)
	if err != nil {
		t.Fatal(err)
	}
	put := got.Body.(*PutBody)
	if put.Timestamp != nil || put.Encoding != nil {
		t.Errorf("optional fields should be nil when flags are 0")
	}
	if string(put.Payload) != "p" {
		t.Errorf("payload = %q", put.Payload)
	}
}
