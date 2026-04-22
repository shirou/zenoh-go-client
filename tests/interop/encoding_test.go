//go:build interop

package interop

import (
	"fmt"
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/zenoh"
)

// TestGoEncodingCatalogPySub: every predefined encoding ID surfaces on the
// Python sample as the matching string prefix.
func TestGoEncodingCatalogPySub(t *testing.T) {
	requireZenohd(t)

	catalog := zenoh.AllPredefinedEncodings()

	const key = "interop/encoding_catalog"
	sub := startPython(t, "python_sub.py",
		"--key", key, "--count", fmt.Sprint(len(catalog)))
	defer sub.close(t)
	sub.waitFor(t, readyMarker, readyTimeout)

	session := openGoSession(t)
	defer session.Close()

	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}
	pub, err := session.DeclarePublisher(ke, nil)
	if err != nil {
		t.Fatalf("DeclarePublisher: %v", err)
	}
	defer pub.Drop()
	time.Sleep(subPropagationDelay)

	// Payload encodes the ID so samples can be correlated back to their
	// emission regardless of arrival order.
	for _, enc := range catalog {
		payload := fmt.Sprintf("enc-id-%d", enc.ID())
		err := pub.Put(zenoh.NewZBytesFromString(payload), &zenoh.PutOptions{
			Encoding: enc, HasEncoding: true,
		})
		if err != nil {
			t.Fatalf("Put(id=%d): %v", enc.ID(), err)
		}
	}

	got := make(map[string]string, len(catalog))
	for i := 0; i < len(catalog); i++ {
		line := sub.readLine(t, ioTimeout)
		if line == doneMarker {
			t.Fatalf("DONE before %d samples (got %d)", len(catalog), i)
		}
		rec, payload := decodeRecord(t, line)
		got[string(payload)] = rec.Encoding
	}
	sub.waitFor(t, doneMarker, ioTimeout)

	for _, enc := range catalog {
		want := enc.String()
		key := fmt.Sprintf("enc-id-%d", enc.ID())
		if g := got[key]; g != want {
			t.Errorf("id=%d: python saw encoding=%q, want %q", enc.ID(), g, want)
		}
	}
}

// TestGoEncodingSchemaAndCustom: "<prefix>;<schema>" round-trips, and an
// unrecognised prefix is preserved as the schema of the default encoding
// (zenoh-rust's documented fallback).
func TestGoEncodingSchemaAndCustom(t *testing.T) {
	requireZenohd(t)

	type tc struct {
		label   string
		enc     zenoh.Encoding
		wantStr string
	}
	cases := []tc{
		{
			label:   "textplain_schema",
			enc:     zenoh.EncodingTextPlain.WithSchema("utf-8"),
			wantStr: "text/plain;utf-8",
		},
		{
			label:   "json_schema",
			enc:     zenoh.EncodingApplicationJson.WithSchema("v1"),
			wantStr: "application/json;v1",
		},
		{
			label:   "unknown_prefix_as_schema",
			enc:     zenoh.NewEncodingFromString("custom/something-new"),
			wantStr: "zenoh/bytes;custom/something-new",
		},
	}

	const key = "interop/encoding_schema"
	sub := startPython(t, "python_sub.py",
		"--key", key, "--count", fmt.Sprint(len(cases)))
	defer sub.close(t)
	sub.waitFor(t, readyMarker, readyTimeout)

	session := openGoSession(t)
	defer session.Close()

	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}
	pub, err := session.DeclarePublisher(ke, nil)
	if err != nil {
		t.Fatalf("DeclarePublisher: %v", err)
	}
	defer pub.Drop()
	time.Sleep(subPropagationDelay)

	for _, c := range cases {
		err := pub.Put(zenoh.NewZBytesFromString(c.label), &zenoh.PutOptions{
			Encoding: c.enc, HasEncoding: true,
		})
		if err != nil {
			t.Fatalf("Put(%s): %v", c.label, err)
		}
	}

	got := map[string]string{}
	for i := 0; i < len(cases); i++ {
		line := sub.readLine(t, ioTimeout)
		if line == doneMarker {
			t.Fatalf("DONE before %d samples (got %d)", len(cases), i)
		}
		rec, payload := decodeRecord(t, line)
		got[string(payload)] = rec.Encoding
	}
	sub.waitFor(t, doneMarker, ioTimeout)

	for _, c := range cases {
		if g := got[c.label]; g != c.wantStr {
			t.Errorf("%s: python saw %q, want %q", c.label, g, c.wantStr)
		}
	}
}
