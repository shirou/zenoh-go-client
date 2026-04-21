package wire

import (
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/codec"
)

// KeepAlive is the KEEP_ALIVE transport message (ID 0x04). The body is empty
// unless an extension chain is attached.
type KeepAlive struct {
	Extensions []codec.Extension
}

func (m *KeepAlive) EncodeTo(w *codec.Writer) error {
	h := codec.Header{
		ID: IDTransportKeepAlive,
		Z:  len(m.Extensions) > 0,
	}
	w.EncodeHeader(h)
	if h.Z {
		return w.EncodeExtensions(m.Extensions)
	}
	return nil
}

func DecodeKeepAlive(r *codec.Reader, h codec.Header) (*KeepAlive, error) {
	if h.ID != IDTransportKeepAlive {
		return nil, fmt.Errorf("wire: expected KEEPALIVE header, got id=%#x", h.ID)
	}
	m := &KeepAlive{}
	if h.Z {
		var err error
		if m.Extensions, err = r.DecodeExtensions(); err != nil {
			return nil, fmt.Errorf("KEEPALIVE extensions: %w", err)
		}
	}
	return m, nil
}
