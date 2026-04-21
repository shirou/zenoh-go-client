package keyexpr

import "strings"

// Intersects reports whether the two key expressions share at least one
// concrete key in common.
func (k KeyExpr) Intersects(other KeyExpr) bool {
	// Fast path: two wildcard-free KEs intersect iff they are equal.
	// Router forwarding for concrete PUSH keys against concrete subscriber
	// keys lands here most often.
	if !hasWildcard(k.s) && !hasWildcard(other.s) {
		return k.s == other.s
	}
	return chunkIntersect(k.s, other.s)
}

func hasWildcard(s string) bool { return strings.IndexByte(s, '*') >= 0 }

// chunkIntersect runs the recursive algorithm. a and b are raw canonical
// key-expression strings; both are assumed well-formed.
func chunkIntersect(a, b string) bool {
	for a != "" && b != "" {
		ca, ra := splitChunk(a)
		cb, rb := splitChunk(b)

		switch {
		case ca == doubleWild:
			// a has `**` here. Either it absorbs the current chunk of b
			// (stay on **), or it stops absorbing (advance past **).
			if ra == "" {
				// `**` at end of a matches anything remaining in b.
				return true
			}
			if rb == "" {
				// b has no chunks left; only matches if a's ** ends.
				return chunkIntersect(ra, b) // try ending **
			}
			if chunkIntersect(ra, b) {
				return true
			}
			// ** absorbs cb and we retry with (a, rb).
			b = rb
		case cb == doubleWild:
			if rb == "" {
				return true
			}
			if ra == "" {
				return chunkIntersect(a, rb)
			}
			if chunkIntersect(a, rb) {
				return true
			}
			a = ra
		case singleChunkIntersect(ca, cb):
			a, b = ra, rb
		default:
			return false
		}
	}
	// Both empty → match. `**` alone still matches an empty tail.
	return (a == "" || a == doubleWild) && (b == "" || b == doubleWild)
}

// singleChunkIntersect reports whether two non-`**` chunks can match the
// same literal chunk value.
func singleChunkIntersect(a, b string) bool {
	if a == b {
		return true
	}
	// `*` matches any non-empty literal; the other side could be `*` too
	// or any literal chunk.
	return a == singleWild || b == singleWild
}

// splitChunk returns (first chunk, rest-after-slash). When there is no
// slash, rest is "".
func splitChunk(s string) (string, string) {
	first, rest, _ := strings.Cut(s, delimiter)
	return first, rest
}
