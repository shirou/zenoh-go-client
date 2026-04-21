package zenoh

import (
	"errors"
	"testing"
)

func TestPriority(t *testing.T) {
	if !PriorityData.IsValid() {
		t.Errorf("PriorityData should be valid")
	}
	if Priority(8).IsValid() {
		t.Errorf("Priority(8) should be invalid (out of 3-bit range)")
	}
	if PriorityDefault != PriorityData {
		t.Errorf("default priority should be Data (5), got %d", PriorityDefault)
	}
}

func TestWhatAmI(t *testing.T) {
	tests := []struct {
		w    WhatAmI
		want string
	}{
		{WhatAmIRouter, "Router"},
		{WhatAmIPeer, "Peer"},
		{WhatAmIClient, "Client"},
	}
	for _, tt := range tests {
		if got := tt.w.String(); got != tt.want {
			t.Errorf("%d.String() = %q, want %q", tt.w, got, tt.want)
		}
	}
}

func TestIdHex(t *testing.T) {
	orig := []byte{0x12, 0x34, 0x56, 0x78}
	id, err := NewIdFromBytes(orig)
	if err != nil {
		t.Fatal(err)
	}
	if id.String() != "12345678" {
		t.Errorf("Id.String() = %q, want %q", id.String(), "12345678")
	}
	id2, err := NewIdFromHex("12345678")
	if err != nil {
		t.Fatal(err)
	}
	if !id.Equal(id2) {
		t.Errorf("Id.Equal() mismatch: %v vs %v", id, id2)
	}
}

func TestIdBounds(t *testing.T) {
	if _, err := NewIdFromBytes(nil); err == nil {
		t.Error("expected error for empty Id")
	}
	if _, err := NewIdFromBytes(make([]byte, 17)); err == nil {
		t.Error("expected error for 17-byte Id")
	}
	id, err := NewIdFromBytes(make([]byte, 1))
	if err != nil || id.Len() != 1 {
		t.Errorf("1-byte Id: err=%v len=%d", err, id.Len())
	}
	id16, err := NewIdFromBytes(make([]byte, 16))
	if err != nil || id16.Len() != 16 {
		t.Errorf("16-byte Id: err=%v len=%d", err, id16.Len())
	}
}

func TestIdZero(t *testing.T) {
	var zero Id
	if !zero.IsZero() {
		t.Error("zero Id should report IsZero()")
	}
	if zero.Len() != 0 {
		t.Errorf("zero Id len = %d, want 0", zero.Len())
	}
}

func TestIdWireIDRoundtrip(t *testing.T) {
	id, _ := NewIdFromBytes([]byte{1, 2, 3, 4, 5, 6, 7, 8})
	wid := id.ToWireID()
	if len(wid.Bytes) != 8 {
		t.Errorf("ToWireID bytes len = %d", len(wid.Bytes))
	}
	// Mutate the wire bytes; original Id must be unaffected (defensive copy).
	wid.Bytes[0] = 0xFF
	if id.Bytes()[0] != 1 {
		t.Error("ToWireID should copy bytes; mutation leaked back to Id")
	}

	round := IdFromWireID(wid)
	if round.Len() != 8 || round.Bytes()[0] != 0xFF {
		t.Errorf("IdFromWireID: got %v", round.Bytes())
	}

	// Zero roundtrip
	var zero Id
	if !IdFromWireID(zero.ToWireID()).IsZero() {
		t.Error("zero Id roundtrip should stay zero")
	}
}

func TestCloseReasonError(t *testing.T) {
	err := CloseReasonError(CloseReasonConnectionToSelf)
	if err.Code != -107 {
		t.Errorf("Code = %d, want -107", err.Code)
	}

	// errors.Is works by matching Code
	target := ZError{Code: -107}
	if !errors.Is(err, target) {
		t.Errorf("errors.Is should match by Code")
	}
}

func TestErrNotImplemented(t *testing.T) {
	var err error = ErrNotImplemented
	if !errors.Is(err, ErrNotImplemented) {
		t.Error("errors.Is should match ErrNotImplemented")
	}
}

// TestSentinelCodesDistinct ensures each sentinel has a unique Code so that
// errors.Is with the Code-based comparison doesn't conflate them.
func TestSentinelCodesDistinct(t *testing.T) {
	sentinels := []ZError{
		ErrNotImplemented,
		ErrAlreadyDropped,
		ErrConnectionLost,
		ErrPayloadTooLarge,
		ErrAllEndpointsFailed,
		ErrSessionClosed,
		ErrSessionNotReady,
	}
	seen := map[int]string{}
	for _, s := range sentinels {
		if prev, dup := seen[s.Code]; dup {
			t.Errorf("sentinel Code %d reused: %q and %q", s.Code, prev, s.Msg)
		}
		seen[s.Code] = s.Msg
	}

	// errors.Is must differentiate NotImplemented from SessionNotReady.
	if errors.Is(ErrSessionNotReady, ErrNotImplemented) {
		t.Error("ErrSessionNotReady should not match ErrNotImplemented")
	}
}
