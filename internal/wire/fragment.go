package wire

import (
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/codec"
)

// Fragment is the FRAGMENT transport message (ID 0x06).
//
// Flags: R (reliable channel, bit 5) / M (more fragments follow, bit 6) / Z.
//
// Wire:
//
//	header
//	seq_num z64
//	[extensions]  — First/Drop require Patch ≥ 1 (not used in MVP)
//	{raw fragment bytes}
type Fragment struct {
	Reliable   bool
	More       bool // M flag: true = more fragments will follow
	SeqNum     uint64
	Extensions []codec.Extension
	// Body is the raw fragment bytes.
	//
	// The slice aliases the parent decoder's buffer; the reassembler MUST
	// copy bytes into its own accumulation buffer rather than retaining
	// this slice past the next batch read.
	Body []byte
}

func (m *Fragment) EncodeTo(w *codec.Writer) error {
	h := codec.Header{
		ID: IDTransportFragment,
		F1: m.Reliable,
		F2: m.More,
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

func DecodeFragment(r *codec.Reader, h codec.Header) (*Fragment, error) {
	if h.ID != IDTransportFragment {
		return nil, fmt.Errorf("wire: expected FRAGMENT header, got id=%#x", h.ID)
	}
	m := &Fragment{Reliable: h.F1, More: h.F2}
	var err error
	if m.SeqNum, err = r.DecodeZ64(); err != nil {
		return nil, err
	}
	if h.Z {
		if m.Extensions, err = r.DecodeExtensions(); err != nil {
			return nil, fmt.Errorf("FRAGMENT extensions: %w", err)
		}
	}
	m.Body = r.Bytes()
	_ = r.Skip(r.Len())
	return m, nil
}
