package wire

import (
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/codec"
)

// WhatAmIMatcher is the 3-bit bitmap in the SCOUT WHAT sub-field indicating
// which peer roles the scouter is interested in receiving HELLOs from.
//
// Spec scouting/pages/scout.adoc:
//
//	bit 0 = Router
//	bit 1 = Peer
//	bit 2 = Client
//
// Unlike WhatAmI (a single role value 0..2), WhatAmIMatcher is a bitmap.
// Multiple bits may be set.
type WhatAmIMatcher uint8

const (
	MatcherRouter WhatAmIMatcher = 1 << 0
	MatcherPeer   WhatAmIMatcher = 1 << 1
	MatcherClient WhatAmIMatcher = 1 << 2

	// MatcherAny matches Router + Peer + Client.
	MatcherAny WhatAmIMatcher = MatcherRouter | MatcherPeer | MatcherClient
)

// Matches reports whether the matcher includes the given WhatAmI role.
// Relies on MatcherRouter/Peer/Client being (1 << Router/Peer/Client) where
// Router=0, Peer=1, Client=2.
func (m WhatAmIMatcher) Matches(role WhatAmI) bool {
	if role > WhatAmIClient {
		return false
	}
	return m&(1<<role) != 0
}

// Scout is the SCOUT scouting message (ID 0x01, scouting ID space).
//
// Spec scouting/pages/scout.adoc:
//
//	header:  |Z|X|X|  SCOUT  |   only Z at bit 7; bits 5/6 reserved
//	version: u8
//	packed:  |zid_len(4)|I(1)|WHAT(3)|
//	           - zid_len at bits 7:4 (only meaningful when I==1; value = len-1)
//	           - I at bit 3 (ZID present)
//	           - WHAT at bits 2:0 (WhatAmIMatcher bitmap)
//	ZID: (1+zid_len) bytes, present only when I==1
//	[extensions] if Z==1
type Scout struct {
	Version    uint8
	Matcher    WhatAmIMatcher
	// ZID is the scouter's ZenohID. When zero-value (len 0) the I flag is
	// cleared and no bytes follow; otherwise it is emitted.
	ZID        ZenohID
	Extensions []codec.Extension
}

// HasZID reports whether the ZenohID field is populated (drives the I flag).
func (m *Scout) HasZID() bool { return m.ZID.IsValid() }

func (m *Scout) EncodeTo(w *codec.Writer) error {
	if m.Matcher & ^MatcherAny != 0 {
		return fmt.Errorf("wire: SCOUT matcher has invalid bits set: %#b", m.Matcher)
	}

	h := codec.Header{
		ID: IDScoutScout,
		Z:  len(m.Extensions) > 0,
	}
	w.EncodeHeader(h)
	w.AppendByte(m.Version)

	hasZID := m.HasZID()
	packed := byte(m.Matcher & MatcherAny) // WHAT at bits 2:0
	if hasZID {
		packed |= 1 << 3                   // I at bit 3
		packed |= byte(m.ZID.Len()-1) << 4 // zid_len at bits 7:4
	}
	w.AppendByte(packed)
	if hasZID {
		if err := EncodeZIDBytes(w, m.ZID, m.ZID.Len()); err != nil {
			return err
		}
	}

	if h.Z {
		return w.EncodeExtensions(m.Extensions)
	}
	return nil
}

func DecodeScout(r *codec.Reader, h codec.Header) (*Scout, error) {
	if h.ID != IDScoutScout {
		return nil, fmt.Errorf("wire: expected SCOUT header, got id=%#x", h.ID)
	}
	m := &Scout{}
	var err error
	if m.Version, err = r.ReadByte(); err != nil {
		return nil, err
	}
	packed, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	m.Matcher = WhatAmIMatcher(packed & 0b111)
	iFlag := packed&(1<<3) != 0
	if iFlag {
		zidLen := int((packed>>4)&0x0F) + 1
		if m.ZID, err = DecodeZIDBytes(r, zidLen); err != nil {
			return nil, err
		}
	}
	if h.Z {
		if m.Extensions, err = r.DecodeExtensions(); err != nil {
			return nil, fmt.Errorf("SCOUT extensions: %w", err)
		}
	}
	return m, nil
}

// Hello is the HELLO scouting message (ID 0x02, scouting ID space).
//
// Spec scouting/pages/hello.adoc:
//
//	header:  |Z|X|L|  HELLO  |   L at bit 5; Z at bit 7
//	version: u8
//	packed:  |zid_len(4)|X X|WAI(2)|   packed as INIT (WAI at bits 1:0)
//	ZID: 1+zid_len bytes
//	if L==1:
//	  loc_count: z8
//	  loc_count × <utf8;z8> locator strings
//	[extensions] if Z==1
type Hello struct {
	Version    uint8
	WhatAmI    WhatAmI
	ZID        ZenohID
	// Locators is emitted inline when non-empty (L flag set).
	Locators   []string
	Extensions []codec.Extension
}

func (m *Hello) EncodeTo(w *codec.Writer) error {
	h := codec.Header{
		ID: IDScoutHello,
		F1: len(m.Locators) > 0, // L flag at bit 5
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
	if h.F1 {
		if len(m.Locators) > 0xFF {
			return fmt.Errorf("wire: HELLO too many locators (%d)", len(m.Locators))
		}
		w.EncodeZ8(uint8(len(m.Locators)))
		for _, loc := range m.Locators {
			// Spec: locators are <utf8;z8>, NOT z16.
			if err := w.EncodeStringZ8(loc); err != nil {
				return err
			}
		}
	}
	if h.Z {
		return w.EncodeExtensions(m.Extensions)
	}
	return nil
}

func DecodeHello(r *codec.Reader, h codec.Header) (*Hello, error) {
	if h.ID != IDScoutHello {
		return nil, fmt.Errorf("wire: expected HELLO header, got id=%#x", h.ID)
	}
	m := &Hello{}
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
	if h.F1 {
		n, err := r.DecodeZ8()
		if err != nil {
			return nil, err
		}
		m.Locators = make([]string, n)
		for i := range m.Locators {
			// Spec: <utf8;z8>.
			if m.Locators[i], err = r.DecodeStringZ8(); err != nil {
				return nil, err
			}
		}
	}
	if h.Z {
		if m.Extensions, err = r.DecodeExtensions(); err != nil {
			return nil, fmt.Errorf("HELLO extensions: %w", err)
		}
	}
	return m, nil
}
