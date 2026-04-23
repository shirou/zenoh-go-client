package transport

import (
	"bytes"
	"errors"
	"testing"

	"github.com/shirou/zenoh-go-client/internal/wire"
)

// TestFragmentReassembleRoundtrip: fragment a 3 KB message into ~512-byte
// chunks, feed each fragment to the reassembler, and verify we recover the
// original bytes.
func TestFragmentReassembleRoundtrip(t *testing.T) {
	const total = 3 * 1024
	msg := make([]byte, total)
	for i := range msg {
		msg[i] = byte(i * 7)
	}

	frag := &Fragmenter{MaxBodySize: 512}
	reassembler := NewReassembler(1 << 20)
	lane := LaneKey{Priority: uint8(wire.QoSPriorityData), Reliable: true}

	var completed []byte
	nextSN, err := frag.Fragment(msg, true, 100, nil, func(f *wire.Fragment) error {
		out, err := reassembler.Push(lane, f.SeqNum, f.More, f.Body)
		if err != nil {
			return err
		}
		if out != nil {
			completed = out
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(completed, msg) {
		t.Errorf("roundtrip mismatch: completed len=%d, want %d", len(completed), total)
	}
	// ceil(3072 / (512-16)) = ceil(6.21) = 7 fragments; next SN = 107.
	expectedNext := uint64(100 + 7)
	if nextSN != expectedNext {
		t.Errorf("nextSN = %d, want %d", nextSN, expectedNext)
	}
}

// TestFragmentSingleChunk: message smaller than MaxBodySize still produces
// one FRAGMENT with More=false.
func TestFragmentSingleChunk(t *testing.T) {
	frag := &Fragmenter{MaxBodySize: 1024}
	msg := []byte("tiny")
	var count int
	_, err := frag.Fragment(msg, true, 0, nil, func(f *wire.Fragment) error {
		count++
		if f.More {
			t.Error("single chunk should have More=false")
		}
		if !bytes.Equal(f.Body, msg) {
			t.Error("body mismatch")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected 1 fragment, got %d", count)
	}
}

// TestReassemblerSNGap: inject a fragment with a skipped seq_num. The
// reassembler must discard the in-flight state and return ErrReassemblySNGap.
func TestReassemblerSNGap(t *testing.T) {
	r := NewReassembler(1 << 20)
	lane := LaneKey{Priority: 5, Reliable: true}

	_, _ = r.Push(lane, 10, true, []byte("aaa"))
	_, err := r.Push(lane, 12, false, []byte("bbb")) // skipped SN 11
	if !errors.Is(err, ErrReassemblySNGap) {
		t.Errorf("expected ErrReassemblySNGap, got %v", err)
	}
	// After the gap, the lane should accept a fresh reassembly starting at
	// any SN.
	out, err := r.Push(lane, 42, false, []byte("fresh"))
	if err != nil {
		t.Fatalf("post-gap fresh push: %v", err)
	}
	if string(out) != "fresh" {
		t.Errorf("post-gap body = %q, want fresh", out)
	}
}

// TestReassemblerTooLarge: accumulated fragments exceeding MaxMessageSize
// are rejected.
func TestReassemblerTooLarge(t *testing.T) {
	r := NewReassembler(4) // very small cap
	lane := LaneKey{Priority: 5, Reliable: false}

	_, _ = r.Push(lane, 1, true, []byte("abc"))
	_, err := r.Push(lane, 2, true, []byte("defgh"))
	if !errors.Is(err, ErrReassemblyTooLarge) {
		t.Errorf("expected ErrReassemblyTooLarge, got %v", err)
	}
}

// TestReassemblerLanesIndependent: two lanes accumulate in parallel without
// interfering.
func TestReassemblerLanesIndependent(t *testing.T) {
	r := NewReassembler(1024)
	laneA := LaneKey{Priority: 5, Reliable: true}
	laneB := LaneKey{Priority: 3, Reliable: false}

	_, _ = r.Push(laneA, 0, true, []byte("AA"))
	_, _ = r.Push(laneB, 0, true, []byte("BB"))
	_, _ = r.Push(laneA, 1, true, []byte("aa"))
	_, _ = r.Push(laneB, 1, true, []byte("bb"))
	outA, err := r.Push(laneA, 2, false, []byte("!"))
	if err != nil {
		t.Fatal(err)
	}
	outB, err := r.Push(laneB, 2, false, []byte("?"))
	if err != nil {
		t.Fatal(err)
	}
	if string(outA) != "AAaa!" {
		t.Errorf("laneA = %q", outA)
	}
	if string(outB) != "BBbb?" {
		t.Errorf("laneB = %q", outB)
	}
}

// TestReassemblerCapBoundary asserts the cap is inclusive: pushing
// exactly MaxMessageSize bytes is accepted, then a single additional byte
// is rejected with ErrReassemblyTooLarge. The behaviour is independent of
// the specific cap value (1 MiB used to keep the test fast), so this
// stands in for the production 1 GiB cap which can't be exercised
// economically in a unit test.
func TestReassemblerCapBoundary(t *testing.T) {
	const limit = 1 << 20
	r := NewReassembler(limit)
	lane := LaneKey{Priority: 5, Reliable: true}

	body := make([]byte, limit)
	if _, err := r.Push(lane, 0, true, body); err != nil {
		t.Fatalf("push at cap: %v", err)
	}
	if _, err := r.Push(lane, 1, true, []byte{0x55}); !errors.Is(err, ErrReassemblyTooLarge) {
		t.Errorf("push cap+1: got %v, want ErrReassemblyTooLarge", err)
	}
}

// TestReassemblerClearResets: Clear drops in-flight state, after which a
// fresh fragment opens a new reassembly from scratch.
func TestReassemblerClearResets(t *testing.T) {
	r := NewReassembler(1024)
	lane := LaneKey{Priority: 5, Reliable: true}
	_, _ = r.Push(lane, 10, true, []byte("stale"))
	r.Clear(lane)
	out, err := r.Push(lane, 100, false, []byte("fresh"))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "fresh" {
		t.Errorf("got %q, want fresh", out)
	}
}
