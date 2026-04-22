package keyexpr

import (
	"fmt"
	"strings"
)

// Wildcard tokens used throughout the package.
const (
	singleWild     = "*"
	doubleWild     = "**"
	delimiter      = "/"
	forbiddenChars = "#?$"
)

// KeyExpr is a validated, canonical-form key expression. The zero value is
// not a valid key expression — always construct through New or Autocanonize.
type KeyExpr struct {
	s string
}

// New validates that s is in canonical form and returns a KeyExpr wrapping
// it. Returns an error if s is empty, contains forbidden characters, has
// empty chunks, or is not in canonical form (e.g., `**/*` is rejected —
// Autocanonize would rewrite it to `*/**`).
func New(s string) (KeyExpr, error) {
	if err := validate(s); err != nil {
		return KeyExpr{}, err
	}
	return KeyExpr{s: s}, nil
}

// Autocanonize normalises s (collapses `**/**` → `**`, rewrites `**/*` as
// `*/**`, strips empty chunks / boundary slashes) and returns the canonical
// KeyExpr.
func Autocanonize(s string) (KeyExpr, error) {
	canon, err := canonize(s)
	if err != nil {
		return KeyExpr{}, err
	}
	return New(canon)
}

// String returns the canonical key expression text.
func (k KeyExpr) String() string { return k.s }

// IsZero reports whether the value is the zero KeyExpr.
func (k KeyExpr) IsZero() bool { return k.s == "" }

// Equal reports whether two key expressions have identical canonical form.
// Canonicalisation guarantees set equality ↔ string equality.
func (k KeyExpr) Equal(o KeyExpr) bool { return k.s == o.s }

func validate(s string) error {
	if s == "" {
		return fmt.Errorf("keyexpr: empty")
	}
	if strings.HasPrefix(s, delimiter) {
		return fmt.Errorf("keyexpr: leading '/' in %q", s)
	}
	if strings.HasSuffix(s, delimiter) {
		return fmt.Errorf("keyexpr: trailing '/' in %q", s)
	}
	if strings.ContainsAny(s, forbiddenChars) {
		return fmt.Errorf("keyexpr: forbidden character in %q (%s reserved)", s, forbiddenChars)
	}

	var prev string
	for c := range strings.SplitSeq(s, delimiter) {
		if c == "" {
			return fmt.Errorf("keyexpr: empty chunk in %q", s)
		}
		// No `$*`-style partial-chunk wildcards; a chunk containing '*'
		// must be exactly "*" or "**".
		if strings.Contains(c, "*") && c != singleWild && c != doubleWild {
			return fmt.Errorf("keyexpr: invalid chunk %q (only literal, '*', or '**' allowed)", c)
		}
		if prev == doubleWild {
			if c == doubleWild {
				return fmt.Errorf("keyexpr: `**/**` is not canonical; use `**`")
			}
			if c == singleWild {
				return fmt.Errorf("keyexpr: `**/*` is not canonical; use `*/**`")
			}
		}
		prev = c
	}
	return nil
}

// canonize rewrites s to canonical form using a single pass that normalises
// each newly-appended chunk against the current tail. This handles
// arbitrarily interleaved `**,**,*` sequences correctly (two separate
// passes would miss adjacencies introduced by the `**/*` → `*/**` swap).
//
// Forbidden characters (`#?$`) are not checked here; the subsequent
// validate step catches them.
func canonize(s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("keyexpr: empty")
	}

	compact := make([]string, 0, 8)
	for c := range strings.SplitSeq(s, delimiter) {
		if c == "" {
			continue // tolerate `//` and leading/trailing `/`
		}
		compact = append(compact, c)
		// After each append, normalise the tail until no rule fires.
		for len(compact) >= 2 {
			last := compact[len(compact)-1]
			prev := compact[len(compact)-2]
			switch {
			case last == doubleWild && prev == doubleWild:
				compact = compact[:len(compact)-1]
			case last == singleWild && prev == doubleWild:
				compact[len(compact)-2] = singleWild
				compact[len(compact)-1] = doubleWild
			default:
				goto next
			}
		}
	next:
	}

	if len(compact) == 0 {
		return "", fmt.Errorf("keyexpr: no chunks after canonisation of %q", s)
	}
	return strings.Join(compact, delimiter), nil
}
