package wire

import (
	"bytes"
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/codec"
)

// Init is the INIT transport message (ID 0x01) used during the session
// establishment handshake. Ack=false means InitSyn (initiator → responder);
// Ack=true means InitAck (responder → initiator) and carries the cookie.
//
// Flags: A (Ack, bit 5) / S (resolution+batch present, bit 6) / Z (extensions).
//
// Wire:
//
//	header
//	version u8
//	packed zid_byte (zid_len|X|X|WAI)
//	ZID (1+zid_len bytes)
//	resolution byte  — if S==1
//	batch_size u16le — if S==1
//	cookie <u8;z16>  — if A==1
//	[extensions]     — if Z==1
type Init struct {
	Ack         bool
	Version     uint8
	ZID         ZenohID
	WhatAmI     WhatAmI
	HasSizeInfo bool // S flag
	Resolution  Resolution
	BatchSize   uint16
	Cookie      []byte // present only when Ack==true
	Extensions  []codec.Extension
}

// EncodeTo writes the INIT message.
func (m *Init) EncodeTo(w *codec.Writer) error {
	h := codec.Header{
		ID: codec.HdrIDMask & IDTransportInit,
		F1: m.Ack,
		F2: m.HasSizeInfo,
		Z:  len(m.Extensions) > 0,
	}
	w.EncodeHeader(h)
	w.AppendByte(m.Version)
	if err := EncodePackedZIDByte(w, m.ZID.Len(), m.WhatAmI); err != nil {
		return err
	}
	if err := EncodeZIDBytes(w, m.ZID, m.ZID.Len()); err != nil {
		return err
	}
	if m.HasSizeInfo {
		w.AppendByte(byte(m.Resolution))
		w.AppendU16LE(m.BatchSize)
	}
	if m.Ack {
		if err := w.EncodeBytesZ16(m.Cookie); err != nil {
			return fmt.Errorf("INIT cookie: %w", err)
		}
	}
	if h.Z {
		if err := w.EncodeExtensions(m.Extensions); err != nil {
			return err
		}
	}
	return nil
}

// DecodeInit parses an INIT message (header already consumed by the caller).
// Pass the unpacked header so the decoder can honour its flags.
func DecodeInit(r *codec.Reader, h codec.Header) (*Init, error) {
	if h.ID != IDTransportInit {
		return nil, fmt.Errorf("wire: expected INIT header, got id=%#x", h.ID)
	}
	m := &Init{
		Ack:         h.F1,
		HasSizeInfo: h.F2,
	}
	var err error
	if m.Version, err = r.ReadByte(); err != nil {
		return nil, err
	}
	zidLen, wai, err := DecodePackedZIDByte(r)
	if err != nil {
		return nil, err
	}
	m.WhatAmI = wai
	if m.ZID, err = DecodeZIDBytes(r, zidLen); err != nil {
		return nil, err
	}
	if m.HasSizeInfo {
		resByte, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		m.Resolution = Resolution(resByte)
		if m.BatchSize, err = r.ReadU16LE(); err != nil {
			return nil, err
		}
	}
	if m.Ack {
		aliased, err := r.DecodeBytesZ16()
		if err != nil {
			return nil, fmt.Errorf("INIT cookie: %w", err)
		}
		// Cookie is retained beyond the current batch (OPEN SYN echoes it
		// back), so copy it out of the reader buffer.
		m.Cookie = bytes.Clone(aliased)
	}
	if h.Z {
		if m.Extensions, err = r.DecodeExtensions(); err != nil {
			return nil, fmt.Errorf("INIT extensions: %w", err)
		}
	}
	return m, nil
}
