package session

import (
	"sync"
	"testing"
)

// TestRuntimeSetRegisterUnregister exercises the Session.runtimes registry
// without spinning up actual Runtimes — the registry is a plain map that
// stores any *Runtime including a stand-in zero value.
func TestRuntimeSetRegisterUnregister(t *testing.T) {
	s := New()
	rtA := &Runtime{}
	rtB := &Runtime{}

	zidA := []byte{0xAA, 0x01}
	zidB := []byte{0xBB, 0x02}

	s.RegisterRuntime(zidA, rtA)
	s.RegisterRuntime(zidB, rtB)

	if got := s.RuntimeFor(zidA); got != rtA {
		t.Errorf("RuntimeFor(A) = %p, want %p", got, rtA)
	}
	if got := s.RuntimeFor(zidB); got != rtB {
		t.Errorf("RuntimeFor(B) = %p, want %p", got, rtB)
	}
	if got := s.AnyRuntime(); got != rtA && got != rtB {
		t.Errorf("AnyRuntime returned a stranger: %p", got)
	}

	snap := s.SnapshotRuntimes()
	if len(snap) != 2 {
		t.Fatalf("SnapshotRuntimes len = %d, want 2", len(snap))
	}

	count := 0
	s.ForEachRuntime(func(_ []byte, _ *Runtime) { count++ })
	if count != 2 {
		t.Errorf("ForEachRuntime visited %d, want 2", count)
	}

	// Unregister with a non-matching rt should not delete.
	s.UnregisterRuntime(zidA, rtB)
	if s.RuntimeFor(zidA) != rtA {
		t.Errorf("UnregisterRuntime with mismatched rt removed entry")
	}

	s.UnregisterRuntime(zidA, rtA)
	if got := s.RuntimeFor(zidA); got != nil {
		t.Errorf("after Unregister, RuntimeFor(A) = %p, want nil", got)
	}

	// Nil rt parameter is a force-delete.
	s.UnregisterRuntime(zidB, nil)
	if got := s.RuntimeFor(zidB); got != nil {
		t.Errorf("force-Unregister(B) failed: %p", got)
	}

	if got := s.AnyRuntime(); got != nil {
		t.Errorf("AnyRuntime after empty = %p, want nil", got)
	}
}

// TestRuntimeSetConcurrent stresses the map under parallel goroutines so
// that race detector can catch any missing locking.
func TestRuntimeSetConcurrent(t *testing.T) {
	s := New()
	const N = 32
	rts := make([]*Runtime, N)
	for i := range rts {
		rts[i] = &Runtime{}
	}

	var wg sync.WaitGroup
	for i := range N {
		wg.Go(func() {
			zid := []byte{byte(i)}
			s.RegisterRuntime(zid, rts[i])
			_ = s.RuntimeFor(zid)
			s.ForEachRuntime(func(_ []byte, _ *Runtime) {})
		})
	}
	wg.Wait()

	if got := len(s.SnapshotRuntimes()); got != N {
		t.Errorf("after concurrent register len=%d, want %d", got, N)
	}

	for i := range N {
		wg.Go(func() {
			s.UnregisterRuntime([]byte{byte(i)}, rts[i])
		})
	}
	wg.Wait()

	if got := len(s.SnapshotRuntimes()); got != 0 {
		t.Errorf("after concurrent unregister len=%d, want 0", got)
	}
}

// TestRuntimeSetOverwrite verifies that re-registering the same key
// replaces the entry rather than appending.
func TestRuntimeSetOverwrite(t *testing.T) {
	s := New()
	zid := []byte{0x42}
	rt1 := &Runtime{}
	rt2 := &Runtime{}
	s.RegisterRuntime(zid, rt1)
	s.RegisterRuntime(zid, rt2)
	if got := s.RuntimeFor(zid); got != rt2 {
		t.Errorf("after overwrite RuntimeFor = %p, want %p", got, rt2)
	}
	if n := len(s.SnapshotRuntimes()); n != 1 {
		t.Errorf("after overwrite len = %d, want 1", n)
	}
}
