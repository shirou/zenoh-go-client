package wire

import (
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/codec"
)

// Frame is the FRAME transport message (ID 0x05).
//
// Flags: R (reliable channel, bit 5) / reserved (bit 6) / Z (extensions, bit 7).
//
// Wire:
//
//	header
//	seq_num z64 (resolution-dependent width; we carry as u64)
//	[extensions]        — QoS priority lane is the canonical extension
//	{NetworkMessage...} — back-to-back serialised NetworkMessages follow
//
// A FRAME does not necessarily extend to the end of the enclosing batch: the
// reference transport opens a new FRAME mid-batch whenever the reliability or
// priority of the next message differs, and other transport messages
// (KEEPALIVE, CLOSE) may follow (zenoh-codec transport/batch.rs). The frame
// body ends at the first header byte that is not a NetworkMessage; the caller
// iterates messages and detects that boundary (network and transport IDs
// occupy disjoint ranges).
type Frame struct {
	Reliable   bool
	SeqNum     uint64
	Extensions []codec.Extension
	// Body holds the NetworkMessage bytes to serialise after the
	// header/seq/extensions when encoding. DecodeFrame does NOT populate
	// it — the body stays in the reader for the caller to iterate.
	Body []byte
}

func (m *Frame) EncodeTo(w *codec.Writer) error {
	h := codec.Header{
		ID: IDTransportFrame,
		F1: m.Reliable,
		Z:  len(m.Extensions) > 0,
	}
	w.EncodeHeader(h)
	w.EncodeZ64(m.SeqNum)
	if h.Z {
		if err := w.EncodeExtensions(m.Extensions); err != nil {
			return err
		}
	}
	w.AppendBytes(m.Body)
	return nil
}

// DecodeFrame reads a FRAME header, sequence number, and extension chain,
// leaving the reader positioned at the first NetworkMessage of the frame
// body. The caller iterates messages until the batch is exhausted or the
// next header byte is not a network message ID (the next transport message
// of the batch starts there).
func DecodeFrame(r *codec.Reader, h codec.Header) (*Frame, error) {
	if h.ID != IDTransportFrame {
		return nil, fmt.Errorf("wire: expected FRAME header, got id=%#x", h.ID)
	}
	m := &Frame{Reliable: h.F1}
	var err error
	if m.SeqNum, err = r.DecodeZ64(); err != nil {
		return nil, err
	}
	if h.Z {
		if m.Extensions, err = r.DecodeExtensions(); err != nil {
			return nil, fmt.Errorf("FRAME extensions: %w", err)
		}
	}
	return m, nil
}
