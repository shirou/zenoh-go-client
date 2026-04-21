package codec

import "testing"

func TestHeaderPackUnpack(t *testing.T) {
	tests := []struct {
		h       Header
		wantB   byte
	}{
		{Header{ID: 0x01}, 0x01},
		{Header{ID: 0x01, F1: true}, 0x21},
		{Header{ID: 0x01, F2: true}, 0x41},
		{Header{ID: 0x01, F1: true, F2: true}, 0x61},
		{Header{ID: 0x1E, Z: true}, 0x9E},
		{Header{ID: 0x1D, F1: true, F2: true, Z: true}, 0xFD}, // PUSH with N|M|Z
	}
	for _, tt := range tests {
		b := PackHeader(tt.h)
		if b != tt.wantB {
			t.Errorf("PackHeader(%+v) = %#x, want %#x", tt.h, b, tt.wantB)
		}
		got := UnpackHeader(tt.wantB)
		if got != tt.h {
			t.Errorf("UnpackHeader(%#x) = %+v, want %+v", tt.wantB, got, tt.h)
		}
	}
}

func TestHeaderMaskingLogic(t *testing.T) {
	// ID > 31 would collide with flag bits; pack must mask.
	h := Header{ID: 0xFF, F1: true, F2: true, Z: true}
	b := PackHeader(h)
	got := UnpackHeader(b)
	if got.ID != 0x1F {
		t.Errorf("ID not masked to 5 bits: got %#x", got.ID)
	}
	if !got.F1 || !got.F2 || !got.Z {
		t.Errorf("flags should be preserved")
	}
}
