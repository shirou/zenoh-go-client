package transport

import (
	"bytes"
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// FragmentSink receives one FRAGMENT at a time and emits it to the wire.
// The batcher or writer provides this callback.
type FragmentSink func(*wire.Fragment) error

// Fragmenter splits a fully-serialised NetworkMessage byte stream into
// FRAGMENT transport messages, each small enough to fit in one batch.
//
// Per the Zenoh 1.0 spec:
//   - M=1 on every fragment except the last
//   - seq_num shares the FRAME sequence space of the same lane
type Fragmenter struct {
	// MaxBodySize is the upper bound on each FRAGMENT body (not including
	// the header + seq_num + extension overhead). Callers derive it from
	// the negotiated batch_size minus per-message overhead.
	MaxBodySize int
}

// fragmentOverhead budgets for: header byte + z64 seq_num (up to 10 bytes) +
// optional extension chain. We reserve a conservative 16 bytes.
const fragmentOverhead = 16

// Fragment splits msgBytes into a sequence of wire.Fragment messages and
// passes each to sink in order. seqNumStart is the initial seq_num for the
// first fragment; each subsequent fragment increments by 1.
//
// Returns the next-available seq_num (i.e. seqNumStart + number of fragments).
func (f *Fragmenter) Fragment(msgBytes []byte, reliable bool, seqNumStart uint64, sink FragmentSink) (uint64, error) {
	if f.MaxBodySize <= fragmentOverhead {
		return seqNumStart, fmt.Errorf("fragmenter: MaxBodySize %d too small (need > %d)", f.MaxBodySize, fragmentOverhead)
	}
	chunk := f.MaxBodySize - fragmentOverhead
	sn := seqNumStart
	for len(msgBytes) > 0 {
		n := min(chunk, len(msgBytes))
		more := n < len(msgBytes)
		frag := &wire.Fragment{
			Reliable: reliable,
			More:     more,
			SeqNum:   sn,
			Body:     msgBytes[:n],
		}
		if err := sink(frag); err != nil {
			return sn, err
		}
		msgBytes = msgBytes[n:]
		sn++
	}
	return sn, nil
}

// EncodeNetworkMessage serialises a network-layer wire message into a byte
// slice ready to be placed inside a FRAME body or fragmented.
//
// The caller usually feeds the result either directly into a batch buffer
// (when it fits) or into Fragmenter.Fragment (when it does not).
func EncodeNetworkMessage(msg interface {
	EncodeTo(w *codec.Writer) error
}) ([]byte, error) {
	w := codec.NewWriter(256)
	if err := msg.EncodeTo(w); err != nil {
		return nil, err
	}
	return bytes.Clone(w.Bytes()), nil
}
