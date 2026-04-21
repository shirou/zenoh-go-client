package session

import (
	"testing"
)

func TestNewIsInit(t *testing.T) {
	s := New()
	if got := s.State(); got != StateInit {
		t.Errorf("State after New = %v, want Init", got)
	}
	if s.IsClosed() {
		t.Error("fresh session should not be closed")
	}
	_ = s.Close()
}

func TestCloseIdempotent(t *testing.T) {
	s := New()
	// Two concurrent Close calls must both return without panic/deadlock.
	done := make(chan struct{}, 2)
	for range 2 {
		go func() {
			_ = s.Close()
			done <- struct{}{}
		}()
	}
	<-done
	<-done
	if !s.IsClosed() {
		t.Error("IsClosed should be true after Close")
	}
}

func TestStateTransitions(t *testing.T) {
	s := New()
	defer s.Close()
	if !s.state.CAS(StateInit, StateDialing) {
		t.Fatal("Init -> Dialing CAS failed")
	}
	if !s.state.CAS(StateDialing, StateHandshaking) {
		t.Fatal("Dialing -> Handshaking CAS failed")
	}
	if !s.state.CAS(StateHandshaking, StateActive) {
		t.Fatal("Handshaking -> Active CAS failed")
	}
	if s.state.CAS(StateDialing, StateActive) {
		t.Error("CAS from stale state should fail")
	}
	if got := s.State(); got != StateActive {
		t.Errorf("State = %v, want Active", got)
	}
}

func TestIDAllocators(t *testing.T) {
	s := New()
	defer s.Close()

	// ExprID must start at 1 (0 reserved for global scope).
	if got := s.IDs().AllocExprID(); got != 1 {
		t.Errorf("first ExprID = %d, want 1", got)
	}
	if got := s.IDs().AllocExprID(); got != 2 {
		t.Errorf("second ExprID = %d, want 2", got)
	}

	// Each allocator is independent.
	if got := s.IDs().AllocSubsID(); got != 1 {
		t.Errorf("first SubsID = %d, want 1", got)
	}
	if got := s.IDs().AllocQblsID(); got != 1 {
		t.Errorf("first QblsID = %d, want 1", got)
	}
	if got := s.IDs().AllocRequestID(); got != 1 {
		t.Errorf("first RequestID = %d, want 1", got)
	}
}

func TestIDAllocatorsConcurrent(t *testing.T) {
	s := New()
	defer s.Close()

	const N = 1000
	const G = 8
	ch := make(chan uint32, N*G)
	done := make(chan struct{})
	for range G {
		go func() {
			for range N {
				ch <- s.IDs().AllocSubsID()
			}
			done <- struct{}{}
		}()
	}
	for range G {
		<-done
	}
	close(ch)

	seen := make(map[uint32]struct{}, N*G)
	for id := range ch {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate SubsID: %d", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != N*G {
		t.Errorf("got %d IDs, want %d", len(seen), N*G)
	}
}
