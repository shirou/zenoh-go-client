package zenoh

import "testing"

func TestEncodingStringRoundtrip(t *testing.T) {
	cases := []struct {
		enc   Encoding
		wantS string
	}{
		{EncodingZenohBytes, "zenoh/bytes"},
		{EncodingZenohString, "zenoh/string"},
		{EncodingTextPlain, "text/plain"},
		{EncodingApplicationJson, "application/json"},
		{EncodingImagePng, "image/png"},
		{EncodingVideoH264, "video/h264"},
		{EncodingAudioVorbis, "audio/vorbis"},
		{EncodingTextPlain.WithSchema("utf-8"), "text/plain;utf-8"},
	}
	for _, c := range cases {
		if got := c.enc.String(); got != c.wantS {
			t.Errorf("%+v.String() = %q, want %q", c.enc, got, c.wantS)
		}
		parsed := NewEncodingFromString(c.wantS)
		if parsed.ID() != c.enc.ID() || parsed.Schema() != c.enc.Schema() {
			t.Errorf("parse %q → %+v, want %+v", c.wantS, parsed, c.enc)
		}
	}
}

func TestEncodingUnknownString(t *testing.T) {
	// Unknown prefix falls back to ZENOH_BYTES with the full string stored
	// as the schema (matches zenoh-rust behavior).
	e := NewEncodingFromString("custom/thing;v1")
	if e.ID() != 0 {
		t.Errorf("unknown prefix should use id=0, got %d", e.ID())
	}
	if e.Schema() != "custom/thing;v1" {
		t.Errorf("unknown prefix schema = %q, want full string", e.Schema())
	}
}

func TestEncodingUnknownID(t *testing.T) {
	// Wire may deliver an id we don't know; String() must still render.
	e := Encoding{id: 9999}
	if got := e.String(); got != "<9999>" {
		t.Errorf("unknown id String() = %q, want <9999>", got)
	}
}
