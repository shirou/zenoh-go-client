package wire

import (
	"bytes"
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/codec"
)

// Open is the OPEN transport message (ID 0x02). Ack=false → OpenSyn (echoes
// the cookie from InitAck); Ack=true → OpenAck (no cookie).
//
// Flags: A (Ack, bit 5) / T (lease unit seconds if 1, ms if 0) / Z (extensions).
//
// Wire:
//
//	header
//	lease      z64 (unit per T flag)
//	initial_sn z64
//	cookie     <u8;z16>  — if A==0 (OpenSyn)
//	[extensions]
type Open struct {
	Ack          bool
	LeaseSeconds bool // T flag
	Lease        uint64
	InitialSN    uint64
	Cookie       []byte // present only when Ack==false
	Extensions   []codec.Extension
}

func (m *Open) EncodeTo(w *codec.Writer) error {
	h := codec.Header{
		ID: IDTransportOpen,
		F1: m.Ack,
		F2: m.LeaseSeconds,
		Z:  len(m.Extensions) > 0,
	}
	w.EncodeHeader(h)
	w.EncodeZ64(m.Lease)
	w.EncodeZ64(m.InitialSN)
	if !m.Ack {
		if err := w.EncodeBytesZ16(m.Cookie); err != nil {
			return fmt.Errorf("OPEN cookie: %w", err)
		}
	}
	if h.Z {
		if err := w.EncodeExtensions(m.Extensions); err != nil {
			return err
		}
	}
	return nil
}

func DecodeOpen(r *codec.Reader, h codec.Header) (*Open, error) {
	if h.ID != IDTransportOpen {
		return nil, fmt.Errorf("wire: expected OPEN header, got id=%#x", h.ID)
	}
	m := &Open{
		Ack:          h.F1,
		LeaseSeconds: h.F2,
	}
	var err error
	if m.Lease, err = r.DecodeZ64(); err != nil {
		return nil, err
	}
	if m.InitialSN, err = r.DecodeZ64(); err != nil {
		return nil, err
	}
	if !m.Ack {
		cookie, err := r.DecodeBytesZ16()
		if err != nil {
			return nil, fmt.Errorf("OPEN cookie: %w", err)
		}
		m.Cookie = bytes.Clone(cookie)
	}
	if h.Z {
		if m.Extensions, err = r.DecodeExtensions(); err != nil {
			return nil, fmt.Errorf("OPEN extensions: %w", err)
		}
	}
	return m, nil
}
