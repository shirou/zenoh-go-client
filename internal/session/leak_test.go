package session

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that every test in this package leaves zero stray
// goroutines after completion. Session.Close() is expected to join every
// goroutine it starts before returning.
//
// Phase 0 intent: this test validates only the *coordination scaffold*
// (closing channel, WaitGroup, closeOnce). Phase 2 adds real reader/writer/
// keepalive goroutines; if any of them fail to honour the closing channel
// this test will begin failing — which is exactly the early-warning we
// want.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestSessionCloseNoLeak exercises the skeleton lifecycle:
// construct a session, start its (currently empty) goroutine loops, close
// it, and verify there are no leftover goroutines.
func TestSessionCloseNoLeak(t *testing.T) {
	s := New()
	s.RunLoops()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !s.IsClosed() {
		t.Error("IsClosed should be true after Close()")
	}
}
