package wire

import (
	"bytes"
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/codec"
)

// ZenohID is a 1..16-byte identifier carried on the wire in little-endian
// order. In INIT / JOIN messages it is packed with zid_len (bits 7:4) and
// WhatAmI (bits 1:0) into a single byte followed by (1+zid_len) bytes.
//
// Bytes aliases the decoder buffer by default. Callers that must retain a
// ZenohID across batch reads should Clone it (use zenoh.IdFromWireID which
// does this at the public-API boundary).
type ZenohID struct {
	Bytes []byte
}

// IsValid reports whether the ZenohID is a legal 1..16 byte identifier.
func (z ZenohID) IsValid() bool { return len(z.Bytes) >= 1 && len(z.Bytes) <= 16 }

// Len returns the byte length (0 for zero-value).
func (z ZenohID) Len() int { return len(z.Bytes) }

// Equal reports whether two ZenohIDs have identical bytes.
func (z ZenohID) Equal(o ZenohID) bool { return bytes.Equal(z.Bytes, o.Bytes) }

// WhatAmI is the node role: 0=Router, 1=Peer, 2=Client, 3=reserved.
type WhatAmI uint8

const (
	WhatAmIRouter   WhatAmI = 0b00
	WhatAmIPeer     WhatAmI = 0b01
	WhatAmIClient   WhatAmI = 0b10
	WhatAmIReserved WhatAmI = 0b11
)

// EncodePackedZIDByte writes the (zid_len|reserved|WhatAmI) byte used in
// INIT and JOIN messages:
//
//	 7 6 5 4 3 2 1 0
//	+-+-+-+-+-+-+-+-+
//	|zid_len|X|X|WAI|   zid_len = (actual bytes - 1)
//	+-+-+-+-+-+-+-+-+
func EncodePackedZIDByte(w *codec.Writer, zidLen int, wai WhatAmI) error {
	if zidLen < 1 || zidLen > 16 {
		return fmt.Errorf("wire: zid_len out of range: %d", zidLen)
	}
	if wai > WhatAmIReserved {
		return fmt.Errorf("wire: WhatAmI out of range: %d", wai)
	}
	w.AppendByte(byte(zidLen-1)<<4 | byte(wai&0b11))
	return nil
}

// DecodePackedZIDByte extracts zid_len (actual byte count, 1..16) and WhatAmI.
func DecodePackedZIDByte(r *codec.Reader) (zidLen int, wai WhatAmI, err error) {
	b, err := r.ReadByte()
	if err != nil {
		return 0, 0, err
	}
	zidLen = int((b>>4)&0x0F) + 1
	wai = WhatAmI(b & 0b11)
	return zidLen, wai, nil
}

// EncodeZIDBytes writes `zid_len` raw bytes. It does not write the packed
// header byte — callers use EncodePackedZIDByte first.
func EncodeZIDBytes(w *codec.Writer, zid ZenohID, zidLen int) error {
	if len(zid.Bytes) != zidLen {
		return fmt.Errorf("wire: ZID length mismatch (have %d, want %d)", len(zid.Bytes), zidLen)
	}
	w.AppendBytes(zid.Bytes)
	return nil
}

// DecodeZIDBytes reads zid_len bytes and returns a ZenohID whose Bytes
// aliases the reader's backing buffer. Callers that retain the ZenohID past
// the next batch read must clone it (e.g. via zenoh.IdFromWireID).
func DecodeZIDBytes(r *codec.Reader, zidLen int) (ZenohID, error) {
	b, err := r.ReadN(zidLen)
	if err != nil {
		return ZenohID{}, err
	}
	return ZenohID{Bytes: b}, nil
}
