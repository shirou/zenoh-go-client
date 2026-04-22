package session

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that every test in this package leaves zero stray
// goroutines after completion. Session.Close() is expected to join every
// goroutine it starts before returning.
//
// This test validates the coordination scaffold (closing channel,
// WaitGroup, closeOnce). Any reader/writer/keepalive goroutine that
// fails to honour the closing channel surfaces here as a leak.
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
