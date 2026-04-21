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
//	{NetworkMessage...} — back-to-back serialised NetworkMessages fill the rest
//
// The body bytes after the extension chain are *not* consumed by Frame. The
// caller decides how to iterate NetworkMessages; Frame exposes them via Body.
//
// FRAME is assumed to consume the rest of the enclosing stream batch — per
// spec `batching.adoc`, a batch contains self-delimiting NetworkMessages
// back-to-back, and there is no trailing transport message after the FRAME
// body within a single batch.
type Frame struct {
	Reliable   bool
	SeqNum     uint64
	Extensions []codec.Extension
	// Body is the remaining bytes after the header/seq/extensions.
	//
	// The slice aliases the parent decoder's buffer (which is usually a
	// reader reading into a reusable batch buffer). Callers MUST finish
	// parsing Body before the next batch read, or copy the bytes.
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

// DecodeFrame reads a FRAME and sets Body to the remaining bytes of the
// enclosing batch. The caller is expected to have wrapped a sub-reader
// covering just this batch.
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
	m.Body = r.Bytes()
	// Consume the body from the reader so subsequent reads see EOF.
	_ = r.Skip(r.Len())
	return m, nil
}
