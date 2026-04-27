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

// LaneSNs carries the per-priority initial sequence numbers a multicast
// peer advertises in its JOIN. Index = QoSPriority (0..7); the slice
// pair is [reliable, best_effort]. Wire layout in the QoS extension is
// the 16 z64s concatenated in priority-major, reliability-minor order:
// [Rel[0], BE[0], Rel[1], BE[1], ..., Rel[7], BE[7]].
type LaneSNs struct {
	Reliable   [8]uint64
	BestEffort [8]uint64
}

// AttachLaneSNs appends the per-lane SN extension (QoS ext, ID=0x01,
// M=true, ZBuf body) to j.Extensions. Existing QoS extensions are left
// alone — the caller is responsible for ensuring it doesn't duplicate.
func (j *Join) AttachLaneSNs(sns LaneSNs) error {
	w := codec.NewWriter(16 * 9) // 16 z64s, ≤ 9 bytes each
	for i := range 8 {
		w.EncodeZ64(sns.Reliable[i])
		w.EncodeZ64(sns.BestEffort[i])
	}
	body := w.Bytes()
	j.Extensions = append(j.Extensions, codec.Extension{
		Header: codec.ExtHeader{
			ID:        ExtIDQoS,
			Encoding:  codec.ExtEncZBuf,
			Mandatory: true,
		},
		ZBuf: body,
	})
	return nil
}

// DecodeLaneSNs extracts per-lane SNs from j.Extensions. Returns
// (nil, false, nil) when no QoS-ZBuf extension is present so callers
// can fall back to the aggregate NextSNRel/NextSNBE fields.
func (j *Join) DecodeLaneSNs() (*LaneSNs, bool, error) {
	for _, ext := range j.Extensions {
		if ext.Header.ID != ExtIDQoS || ext.Header.Encoding != codec.ExtEncZBuf {
			continue
		}
		r := codec.NewReader(ext.ZBuf)
		var sns LaneSNs
		for i := range 8 {
			v, err := r.DecodeZ64()
			if err != nil {
				return nil, false, fmt.Errorf("JOIN lane SN[%d].reliable: %w", i, err)
			}
			sns.Reliable[i] = v
			v, err = r.DecodeZ64()
			if err != nil {
				return nil, false, fmt.Errorf("JOIN lane SN[%d].best_effort: %w", i, err)
			}
			sns.BestEffort[i] = v
		}
		if r.Len() != 0 {
			return nil, false, fmt.Errorf("JOIN lane SN extension has %d trailing bytes", r.Len())
		}
		return &sns, true, nil
	}
	return nil, false, nil
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
