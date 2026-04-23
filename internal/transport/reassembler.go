package transport

import (
	"bytes"
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// LaneKey identifies one of the 16 QoS lanes (8 priorities × reliable/best_effort).
type LaneKey struct {
	Priority uint8
	Reliable bool
}

// LanePriorityFromExts extracts the QoS priority encoded on a FRAME or
// FRAGMENT extension chain. Falls back to the spec-default Data priority
// when no QoS Z64 extension is present — used by both the inbound reader
// and reassembler-test fixtures so the lane keying stays in lockstep.
func LanePriorityFromExts(exts []codec.Extension) uint8 {
	if e := codec.FindExt(exts, wire.ExtIDQoS); e != nil && e.Header.Encoding == codec.ExtEncZ64 {
		return uint8(wire.DecodeQoSZ64(e.Z64).Priority)
	}
	return uint8(wire.QoSPriorityData)
}

// Reassembler accumulates FRAGMENT bodies per lane into a complete message.
//
// Semantics (matching zenoh-rust's DefragBuffer):
//   - Fragments on each lane must arrive in seq_num order.
//   - A gap (non-contiguous seq_num) clears the buffer and drops the message.
//   - When More=false arrives, the accumulated bytes are returned.
//   - Per-lane buffer capacity is bounded by MaxMessageSize; exceeding it
//     discards the in-flight reassembly and returns ErrReassemblyTooLarge.
type Reassembler struct {
	MaxMessageSize int
	lanes          map[LaneKey]*reassemblyState
}

// NewReassembler constructs a reassembler with a per-lane byte cap.
func NewReassembler(maxMessageSize int) *Reassembler {
	return &Reassembler{
		MaxMessageSize: maxMessageSize,
		lanes:          map[LaneKey]*reassemblyState{},
	}
}

type reassemblyState struct {
	expectedSN uint64
	buf        bytes.Buffer
	active     bool
}

// ErrReassemblyTooLarge is returned when accumulated fragments would exceed
// the configured max. The in-flight reassembly for that lane is cleared.
var ErrReassemblyTooLarge = fmt.Errorf("reassembler: message exceeds max size")

// ErrReassemblySNGap is returned when a fragment's seq_num is not the
// expected next value on its lane. The in-flight reassembly is cleared.
var ErrReassemblySNGap = fmt.Errorf("reassembler: fragment seq_num gap")

// Push consumes one FRAGMENT. If the fragment completes a message (More=false),
// the full encoded bytes are returned; otherwise the returned slice is nil.
//
// Callers pass the fragment body by reference (aliased). When Push returns
// a completed-message slice, it is freshly allocated and the caller owns it.
func (r *Reassembler) Push(lane LaneKey, seqNum uint64, more bool, body []byte) ([]byte, error) {
	st, ok := r.lanes[lane]
	if !ok {
		st = &reassemblyState{}
		r.lanes[lane] = st
	}

	if !st.active {
		// First fragment for this lane — open a new reassembly.
		st.active = true
		st.expectedSN = seqNum
		st.buf.Reset()
	} else if seqNum != st.expectedSN {
		// Gap — discard everything on this lane.
		r.clear(st)
		return nil, ErrReassemblySNGap
	}

	if st.buf.Len()+len(body) > r.MaxMessageSize {
		r.clear(st)
		return nil, ErrReassemblyTooLarge
	}
	st.buf.Write(body)
	st.expectedSN = seqNum + 1

	if more {
		return nil, nil
	}

	// Completed — hand out a fresh copy, reset state.
	out := bytes.Clone(st.buf.Bytes())
	r.clear(st)
	return out, nil
}

// Clear forgets any in-flight reassembly on the given lane. Called on
// reconnect or CLOSE so stale partial messages don't re-appear.
func (r *Reassembler) Clear(lane LaneKey) {
	if st, ok := r.lanes[lane]; ok {
		r.clear(st)
	}
}

func (r *Reassembler) clear(st *reassemblyState) {
	st.active = false
	st.expectedSN = 0
	st.buf.Reset()
}
