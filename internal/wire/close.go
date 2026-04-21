package wire

import (
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/codec"
)

// Close is the CLOSE transport message (ID 0x03).
//
// Flags: S (whole session if 1, single link if 0, bit 5) / Z (extensions).
//
// Wire:
//
//	header
//	reason u8
//	[extensions]
type Close struct {
	Session    bool // S flag: close whole session (true) vs this link only (false)
	Reason     uint8
	Extensions []codec.Extension
}

func (m *Close) EncodeTo(w *codec.Writer) error {
	h := codec.Header{
		ID: IDTransportClose,
		F1: m.Session,
		Z:  len(m.Extensions) > 0,
	}
	w.EncodeHeader(h)
	w.AppendByte(m.Reason)
	if h.Z {
		return w.EncodeExtensions(m.Extensions)
	}
	return nil
}

func DecodeClose(r *codec.Reader, h codec.Header) (*Close, error) {
	if h.ID != IDTransportClose {
		return nil, fmt.Errorf("wire: expected CLOSE header, got id=%#x", h.ID)
	}
	m := &Close{Session: h.F1}
	var err error
	if m.Reason, err = r.ReadByte(); err != nil {
		return nil, err
	}
	if h.Z {
		if m.Extensions, err = r.DecodeExtensions(); err != nil {
			return nil, fmt.Errorf("CLOSE extensions: %w", err)
		}
	}
	return m, nil
}
