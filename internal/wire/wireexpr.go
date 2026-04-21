package wire

import "github.com/shirou/zenoh-go-client/internal/codec"

// WireExpr is a scope + suffix reference to a key expression:
//
//	scope:  z16 ExprId (0 = global scope, no aliased expression)
//	suffix: optional <u8;z16> appended to the scope-resolved base
//
// The presence/absence of the suffix and the mapping-space flag are
// conveyed by the enclosing message header's N and M flags, not in the
// WireExpr itself. EncodeScope/DecodeScope write just the scope+suffix
// bytes; callers set the outer N (Named) and M (Mapping) flags on the
// parent message header.
type WireExpr struct {
	Scope  uint16 // 0 = global
	Suffix string // empty when the outer N flag is 0
	// Mapping indicates "sender's mapping space" when true (outer M flag).
	// Stored here for convenience on receive; not written by EncodeScope.
	Mapping bool
}

// Named reports whether a key suffix is present (outer N flag = 1).
func (k WireExpr) Named() bool { return k.Suffix != "" }

// EncodeScope writes the scope and, if named, the suffix.
// The caller controls the outer message header's N and M flags.
func (k WireExpr) EncodeScope(w *codec.Writer) error {
	w.EncodeZ16(k.Scope)
	if k.Named() {
		if err := w.EncodeStringZ16(k.Suffix); err != nil {
			return err
		}
	}
	return nil
}

// DecodeScope reads the scope and, when named, the suffix.
// `named` reflects the outer N flag from the enclosing message header.
func DecodeWireExpr(r *codec.Reader, named, mapping bool) (WireExpr, error) {
	scope, err := r.DecodeZ16()
	if err != nil {
		return WireExpr{}, err
	}
	k := WireExpr{Scope: scope, Mapping: mapping}
	if named {
		s, err := r.DecodeStringZ16()
		if err != nil {
			return WireExpr{}, err
		}
		k.Suffix = s
	}
	return k, nil
}
