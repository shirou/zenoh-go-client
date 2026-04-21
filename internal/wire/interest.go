package wire

import (
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/codec"
)

// InterestMode is the 2-bit Mod field on the INTEREST header (bits 6:5).
type InterestMode uint8

const (
	InterestModeFinal         InterestMode = 0b00
	InterestModeCurrent       InterestMode = 0b01
	InterestModeFuture        InterestMode = 0b10
	InterestModeCurrentFuture InterestMode = 0b11
)

// InterestFilter is the set of declaration kinds an Interest subscribes to.
//
// Spec session/pages/interests.adoc §Options Byte:
//
//	K (bit 0) — key expression declarations
//	S (bit 1) — subscriber declarations
//	Q (bit 2) — queryable declarations
//	T (bit 3) — token declarations
//	A (bit 7) — aggregate replies
//
// The R (Restricted), N (Named), M (Mapping) flags of the options byte are
// derived from the Interest.KeyExpr field: when KeyExpr != nil we set R, and
// N/M are computed from the WireExpr. Callers should not set them manually.
type InterestFilter struct {
	KeyExprs    bool
	Subscribers bool
	Queryables  bool
	Tokens      bool
	Aggregate   bool
}

// options byte bit positions.
const (
	interestOptBitKeyExprs   = 0
	interestOptBitSubs       = 1
	interestOptBitQueryables = 2
	interestOptBitTokens     = 3
	interestOptBitRestricted = 4
	interestOptBitNamed      = 5
	interestOptBitMapping    = 6
	interestOptBitAggregate  = 7
)

// Interest is the INTEREST network message (ID 0x19).
//
// Wire shape depends on Mode:
//   - Final (0b00): header + interest_id only (no options byte, no key expr)
//   - Current / Future / CurrentFuture: header + interest_id + options byte +
//     optional WireExpr when Restricted (KeyExpr != nil)
type Interest struct {
	InterestID uint32
	Mode       InterestMode
	Filter     InterestFilter // ignored when Mode == Final
	// KeyExpr non-nil means the interest is Restricted to that key expression;
	// the R/N/M flags are derived from its presence and content. nil means
	// unrestricted.
	KeyExpr    *WireExpr
	Extensions []codec.Extension
}

func (m *Interest) EncodeTo(w *codec.Writer) error {
	h := codec.Header{
		ID: IDNetworkInterest,
		F1: m.Mode&0b01 != 0, // Mod[0]
		F2: m.Mode&0b10 != 0, // Mod[1]
		Z:  len(m.Extensions) > 0,
	}
	w.EncodeHeader(h)
	w.EncodeZ32(m.InterestID)

	if m.Mode != InterestModeFinal {
		var opts byte
		if m.Filter.KeyExprs {
			opts |= 1 << interestOptBitKeyExprs
		}
		if m.Filter.Subscribers {
			opts |= 1 << interestOptBitSubs
		}
		if m.Filter.Queryables {
			opts |= 1 << interestOptBitQueryables
		}
		if m.Filter.Tokens {
			opts |= 1 << interestOptBitTokens
		}
		if m.Filter.Aggregate {
			opts |= 1 << interestOptBitAggregate
		}
		// R, N, M are meaningful only when the Interest is restricted to a
		// specific key expression. They are computed from KeyExpr, not taken
		// from user input, so we can't produce an illegal combination.
		if m.KeyExpr != nil {
			opts |= 1 << interestOptBitRestricted
			if m.KeyExpr.Named() {
				opts |= 1 << interestOptBitNamed
			}
			if m.KeyExpr.Mapping {
				opts |= 1 << interestOptBitMapping
			}
		}
		w.AppendByte(opts)

		if m.KeyExpr != nil {
			if err := m.KeyExpr.EncodeScope(w); err != nil {
				return err
			}
		}
	}

	if h.Z {
		return w.EncodeExtensions(m.Extensions)
	}
	return nil
}

func DecodeInterest(r *codec.Reader, h codec.Header) (*Interest, error) {
	if h.ID != IDNetworkInterest {
		return nil, fmt.Errorf("wire: expected INTEREST header, got id=%#x", h.ID)
	}
	var mode InterestMode
	if h.F1 {
		mode |= 0b01
	}
	if h.F2 {
		mode |= 0b10
	}
	m := &Interest{Mode: mode}
	var err error
	if m.InterestID, err = r.DecodeZ32(); err != nil {
		return nil, err
	}

	if mode != InterestModeFinal {
		opts, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		m.Filter = InterestFilter{
			KeyExprs:    opts&(1<<interestOptBitKeyExprs) != 0,
			Subscribers: opts&(1<<interestOptBitSubs) != 0,
			Queryables:  opts&(1<<interestOptBitQueryables) != 0,
			Tokens:      opts&(1<<interestOptBitTokens) != 0,
			Aggregate:   opts&(1<<interestOptBitAggregate) != 0,
		}
		if opts&(1<<interestOptBitRestricted) != 0 {
			named := opts&(1<<interestOptBitNamed) != 0
			mapping := opts&(1<<interestOptBitMapping) != 0
			ke, err := DecodeWireExpr(r, named, mapping)
			if err != nil {
				return nil, err
			}
			m.KeyExpr = &ke
		}
	}

	if h.Z {
		if m.Extensions, err = r.DecodeExtensions(); err != nil {
			return nil, fmt.Errorf("INTEREST extensions: %w", err)
		}
	}
	return m, nil
}
