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

// TestSessionCloseNoLeak: an unopened session (no Run) closes cleanly and
// leaves no goroutines. Run-with-link scenarios are covered in run_test.go
// and their goleak enforcement comes via TestMain above.
func TestSessionCloseNoLeak(t *testing.T) {
	s := New()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !s.IsClosed() {
		t.Error("IsClosed should be true after Close()")
	}
}
