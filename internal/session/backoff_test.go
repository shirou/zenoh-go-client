package session

import (
	"testing"
	"time"
)

func TestBackoffSequence(t *testing.T) {
	b := NewBackoff(ReconnectConfig{
		InitialMs: 100, MaxMs: 800, IncreaseFactor: 2.0,
	})
	// 100, 200, 400, 800, 800, 800...
	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		800 * time.Millisecond,
		800 * time.Millisecond,
	}
	for i, d := range want {
		got := b.Next()
		if got != d {
			t.Errorf("step %d: got %v, want %v", i, got, d)
		}
	}
}

func TestBackoffReset(t *testing.T) {
	b := NewBackoff(ReconnectConfig{InitialMs: 50, MaxMs: 1000, IncreaseFactor: 2})
	_ = b.Next()
	_ = b.Next()
	b.Reset()
	if got := b.Next(); got != 50*time.Millisecond {
		t.Errorf("post-reset = %v, want 50ms", got)
	}
}
