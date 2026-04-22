//go:build interop

package interop

import (
	"bytes"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/zenoh"
)

// TestGoPublisherDeletePySub: Publisher.Delete on Go emits a sample that
// Python sees as SampleKind.DELETE.
func TestGoPublisherDeletePySub(t *testing.T) {
	requireZenohd(t)

	const (
		key   = "interop/delete"
		count = 2 // one Put then one Delete
	)

	sub := startPython(t, "python_sub.py", "--key", key, "--count", fmt.Sprint(count))
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

	if err := pub.Put(zenoh.NewZBytesFromString("first"), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := pub.Delete(nil); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	kinds := make([]zenoh.SampleKind, 0, count)
	for i := 0; i < count; i++ {
		line := sub.readLine(t, ioTimeout)
		if line == doneMarker {
			t.Fatalf("%s before %d samples; got %d", doneMarker, count, i)
		}
		rec, _ := decodeRecord(t, line)
		kinds = append(kinds, rec.SampleKind())
	}
	sub.waitFor(t, doneMarker, ioTimeout)

	want := []zenoh.SampleKind{zenoh.SampleKindPut, zenoh.SampleKindDelete}
	if !slices.Equal(kinds, want) {
		t.Errorf("kinds = %v, want %v", kinds, want)
	}
}

// TestGoWildcardSubscribe: Python subscribes with wildcards, Go publishes
// on concrete keys; the received subset matches KE semantics.
//
// Two variants in one driver:
//   - wildcard "interop/wildcard_ds/**"      matches everything under the prefix
//   - wildcard "interop/wildcard_ss/*/x"     matches only single-segment then "x"
func TestGoWildcardSubscribe(t *testing.T) {
	requireZenohd(t)

	type variant struct {
		name     string
		wildcard string
		puts     []string // concrete keys to publish on
		hits     []string // subset expected to reach the subscriber
	}
	variants := []variant{
		{
			name:     "double_star",
			wildcard: "interop/wildcard_ds/**",
			puts:     []string{"interop/wildcard_ds/x", "interop/wildcard_ds/y/z", "interop/other/x"},
			hits:     []string{"interop/wildcard_ds/x", "interop/wildcard_ds/y/z"},
		},
		{
			name:     "single_star_x",
			wildcard: "interop/wildcard_ss/*/x",
			puts: []string{
				"interop/wildcard_ss/a/x",   // hit
				"interop/wildcard_ss/b/x",   // hit
				"interop/wildcard_ss/a",     // miss (too short)
				"interop/wildcard_ss/a/y",   // miss (last seg mismatch)
				"interop/wildcard_ss/a/x/y", // miss (too long)
			},
			hits: []string{"interop/wildcard_ss/a/x", "interop/wildcard_ss/b/x"},
		},
	}

	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			sub := startPython(t, "python_sub.py",
				"--key", v.wildcard, "--count", fmt.Sprint(len(v.hits)))
			defer sub.close(t)
			sub.waitFor(t, readyMarker, readyTimeout)

			session := openGoSession(t)
			defer session.Close()

			time.Sleep(subPropagationDelay)

			for _, k := range v.puts {
				ke, err := zenoh.NewKeyExpr(k)
				if err != nil {
					t.Fatalf("NewKeyExpr(%q): %v", k, err)
				}
				if err := session.Put(ke, zenoh.NewZBytesFromString(k), nil); err != nil {
					t.Fatalf("Put(%q): %v", k, err)
				}
			}

			got := make(map[string]bool)
			for i := 0; i < len(v.hits); i++ {
				line := sub.readLine(t, ioTimeout)
				if line == doneMarker {
					t.Fatalf("DONE before %d hits (got %d): %v", len(v.hits), i, got)
				}
				rec, _ := decodeRecord(t, line)
				got[rec.Key] = true
			}
			// Short drain window: any stray sample arriving after the
			// expected count means the wildcard matched wider than the KE
			// contract claims.
			drainDeadline := time.After(150 * time.Millisecond)
		drain:
			for {
				select {
				case line, ok := <-sub.lines:
					if !ok || line == doneMarker {
						break drain
					}
					rec, _ := decodeRecord(t, line)
					t.Errorf("unexpected extra sample on %q", rec.Key)
				case <-drainDeadline:
					break drain
				}
			}

			for _, want := range v.hits {
				if !got[want] {
					t.Errorf("missing hit %q", want)
				}
			}
		})
	}
}

// TestGoPayloadVariants: UTF-8 / binary / JSON / empty payload round-trip;
// an empty Put is distinguishable from a Delete.
func TestGoPayloadVariants(t *testing.T) {
	requireZenohd(t)

	type emission struct {
		name     string
		doDelete bool
		encoding *zenoh.Encoding // nil -> don't set
		payload  []byte
	}
	utf8Enc := zenoh.EncodingTextPlain.WithSchema("utf-8")
	jsonEnc := zenoh.EncodingApplicationJson
	binEnc := zenoh.EncodingApplicationOctetStream
	emissions := []emission{
		{name: "utf8", encoding: &utf8Enc, payload: []byte("日本語 + ascii")},
		{name: "binary", encoding: &binEnc, payload: []byte{0x00, 0x01, 0xff, 0xfe, 0x80}},
		{name: "json", encoding: &jsonEnc, payload: []byte(`{"x":1,"y":"z"}`)},
		{name: "empty_put", payload: []byte{}},
		{name: "empty_delete", doDelete: true},
	}

	const key = "interop/payload_variants"
	sub := startPython(t, "python_sub.py",
		"--key", key, "--count", fmt.Sprint(len(emissions)))
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

	for _, e := range emissions {
		if e.doDelete {
			if err := pub.Delete(nil); err != nil {
				t.Fatalf("Delete(%s): %v", e.name, err)
			}
			continue
		}
		var opts *zenoh.PutOptions
		if e.encoding != nil {
			opts = &zenoh.PutOptions{Encoding: *e.encoding, HasEncoding: true}
		}
		if err := pub.Put(zenoh.NewZBytes(e.payload), opts); err != nil {
			t.Fatalf("Put(%s): %v", e.name, err)
		}
	}

	for i, e := range emissions {
		line := sub.readLine(t, ioTimeout)
		if line == doneMarker {
			t.Fatalf("DONE before emission %d (%s)", i, e.name)
		}
		rec, payload := decodeRecord(t, line)

		if e.doDelete {
			if rec.SampleKind() != zenoh.SampleKindDelete {
				t.Errorf("%s: kind=%v, want Delete", e.name, rec.SampleKind())
			}
			continue
		}
		if rec.SampleKind() != zenoh.SampleKindPut {
			t.Errorf("%s: kind=%v, want Put", e.name, rec.SampleKind())
		}
		if !bytes.Equal(payload, e.payload) {
			t.Errorf("%s: payload=%x, want %x", e.name, payload, e.payload)
		}
		if e.encoding != nil {
			if want := e.encoding.String(); rec.Encoding != want {
				t.Errorf("%s: encoding=%q, want %q", e.name, rec.Encoding, want)
			}
		}
	}
	sub.waitFor(t, doneMarker, ioTimeout)
}
