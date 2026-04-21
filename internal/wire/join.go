package wire

import (
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/codec"
)

// Join is the JOIN transport message (ID 0x07) used on multicast transports.
//
// Flags: T (lease unit seconds, bit 5) / S (resolution+batch present, bit 6) / Z.
//
// Wire:
//
//	header
//	version u8
//	packed zid_byte
//	ZID (1+zid_len bytes)
//	resolution byte  — if S==1
//	batch_size u16le — if S==1
//	lease z64 (unit per T)
//	next_sn_reliable    z64
//	next_sn_best_effort z64
//	[extensions]
type Join struct {
	LeaseSeconds bool
	HasSizeInfo  bool
	Version      uint8
	ZID          ZenohID
	WhatAmI      WhatAmI
	Resolution   Resolution
	BatchSize    uint16
	Lease        uint64
	NextSNRel    uint64
	NextSNBE     uint64
	Extensions   []codec.Extension
}

func (m *Join) EncodeTo(w *codec.Writer) error {
	h := codec.Header{
		ID: IDTransportJoin,
		F1: m.LeaseSeconds,
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
	w.EncodeZ64(m.Lease)
	w.EncodeZ64(m.NextSNRel)
	w.EncodeZ64(m.NextSNBE)
	if h.Z {
		return w.EncodeExtensions(m.Extensions)
	}
	return nil
}

func DecodeJoin(r *codec.Reader, h codec.Header) (*Join, error) {
	if h.ID != IDTransportJoin {
		return nil, fmt.Errorf("wire: expected JOIN header, got id=%#x", h.ID)
	}
	m := &Join{LeaseSeconds: h.F1, HasSizeInfo: h.F2}
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
	if m.Lease, err = r.DecodeZ64(); err != nil {
		return nil, err
	}
	if m.NextSNRel, err = r.DecodeZ64(); err != nil {
		return nil, err
	}
	if m.NextSNBE, err = r.DecodeZ64(); err != nil {
		return nil, err
	}
	if h.Z {
		if m.Extensions, err = r.DecodeExtensions(); err != nil {
			return nil, fmt.Errorf("JOIN extensions: %w", err)
		}
	}
	return m, nil
}
