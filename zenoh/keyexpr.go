package zenoh

import (
	"github.com/shirou/zenoh-go-client/internal/keyexpr"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// KeyExpr is a validated zenoh key expression. The zero value is invalid;
// construct via NewKeyExpr or NewKeyExprAutocanonized.
type KeyExpr struct {
	inner keyexpr.KeyExpr
}

// NewKeyExpr validates that s is in canonical form and returns the KeyExpr.
func NewKeyExpr(s string) (KeyExpr, error) {
	k, err := keyexpr.New(s)
	if err != nil {
		return KeyExpr{}, err
	}
	return KeyExpr{inner: k}, nil
}

// NewKeyExprAutocanonized normalises s to canonical form and returns the
// resulting KeyExpr.
func NewKeyExprAutocanonized(s string) (KeyExpr, error) {
	k, err := keyexpr.Autocanonize(s)
	if err != nil {
		return KeyExpr{}, err
	}
	return KeyExpr{inner: k}, nil
}

// String returns the canonical text.
func (k KeyExpr) String() string { return k.inner.String() }

// IsZero reports whether the value is the zero KeyExpr.
func (k KeyExpr) IsZero() bool { return k.inner.IsZero() }

// Intersects reports whether k and other share at least one concrete key.
func (k KeyExpr) Intersects(other KeyExpr) bool { return k.inner.Intersects(other.inner) }

// Includes reports whether k's set is a superset of other's.
func (k KeyExpr) Includes(other KeyExpr) bool { return k.inner.Includes(other.inner) }

// Join returns "k/suffix".
func (k KeyExpr) Join(suffix string) (KeyExpr, error) {
	joined, err := k.inner.Join(suffix)
	if err != nil {
		return KeyExpr{}, err
	}
	return KeyExpr{inner: joined}, nil
}

// Concat returns "k || suffix" (no slash inserted).
func (k KeyExpr) Concat(suffix string) (KeyExpr, error) {
	concat, err := k.inner.Concat(suffix)
	if err != nil {
		return KeyExpr{}, err
	}
	return KeyExpr{inner: concat}, nil
}

// toWire returns the WireExpr for full-string encoding (scope=0 + suffix).
// Every emission sends the full key-expression string; aliasing and
// alias-table support can be added later.
func (k KeyExpr) toWire() wire.WireExpr {
	return wire.WireExpr{Scope: 0, Suffix: k.inner.String()}
}
