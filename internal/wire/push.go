package wire

import (
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/codec"
)

// Push is the PUSH network message (ID 0x1D). Carries a PUT or DEL
// sub-message in its body.
//
// Flags: N (Named suffix, bit 5) / M (Mapping, bit 6) / Z (extensions).
//
// Wire:
//
//	header
//	key_scope z16
//	key_suffix <u8;z16> — if N==1
//	[extensions] — if Z==1 (QoS / Timestamp / NodeId / ...)
//	PushBody (PUT 0x01 or DEL 0x02)
type Push struct {
	KeyExpr    WireExpr
	Extensions []codec.Extension
	Body       PushBody // non-nil: PutBody or DelBody
}

// PushBody is the union of data sub-messages that can appear inside a
// PUSH (and also inside REPLY).
type PushBody interface {
	SubID() byte
	EncodePushBody(w *codec.Writer) error
}

func (m *Push) EncodeTo(w *codec.Writer) error {
	if m.Body == nil {
		return fmt.Errorf("wire: PUSH with nil body")
	}
	h := codec.Header{
		ID: IDNetworkPush,
		F1: m.KeyExpr.Named(),
		F2: m.KeyExpr.Mapping,
		Z:  len(m.Extensions) > 0,
	}
	w.EncodeHeader(h)
	if err := m.KeyExpr.EncodeScope(w); err != nil {
		return err
	}
	if h.Z {
		if err := w.EncodeExtensions(m.Extensions); err != nil {
			return err
		}
	}
	return m.Body.EncodePushBody(w)
}

// DecodePush reads a PUSH network message (the outer header has already
// been consumed by the caller).
func DecodePush(r *codec.Reader, h codec.Header) (*Push, error) {
	if h.ID != IDNetworkPush {
		return nil, fmt.Errorf("wire: expected PUSH header, got id=%#x", h.ID)
	}
	m := &Push{}
	ke, err := DecodeWireExpr(r, h.F1, h.F2)
	if err != nil {
		return nil, err
	}
	m.KeyExpr = ke
	if h.Z {
		if m.Extensions, err = r.DecodeExtensions(); err != nil {
			return nil, fmt.Errorf("PUSH extensions: %w", err)
		}
	}
	m.Body, err = decodePushBody(r)
	if err != nil {
		return nil, fmt.Errorf("PUSH body: %w", err)
	}
	return m, nil
}

func decodePushBody(r *codec.Reader) (PushBody, error) {
	h, err := r.DecodeHeader()
	if err != nil {
		return nil, err
	}
	switch h.ID {
	case IDDataPut:
		return decodePutBody(r, h)
	case IDDataDel:
		return decodeDelBody(r, h)
	default:
		return nil, fmt.Errorf("wire: unknown PUSH body id=%#x", h.ID)
	}
}
