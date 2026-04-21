package keyexpr

// Includes reports whether k's key set is a superset of other's — i.e.
// every concrete key matching other also matches k.
//
// Port of zenoh-rust LTRIncluder (io/zenoh-keyexpr/src/key_expr/include.rs):
// left-to-right scan; a `**` on the left can either end absorption (and
// match what's left on the right) or keep absorbing (consume one right
// chunk).
func (k KeyExpr) Includes(other KeyExpr) bool {
	if k.s == other.s {
		return true
	}
	return ltrIncludes(k.s, other.s)
}

func ltrIncludes(left, right string) bool {
	for {
		lchunk, lrest := splitChunk(left)
		lempty := lrest == ""

		if lchunk == doubleWild {
			// `**` can stop absorbing: match whatever is left on the
			// right against the rest of left.
			if lempty || ltrIncludes(lrest, right) {
				return true
			}
			// Otherwise consume one chunk from the right and try again.
			_, rrest := splitChunk(right)
			if rrest == "" {
				return false
			}
			right = rrest
			continue
		}

		rchunk, rrest := splitChunk(right)
		if rchunk == "" || rchunk == doubleWild {
			// A `**` on the right can match more chunks than any non-`**`
			// on the left — so left cannot include right.
			return false
		}
		if !chunkIncludes(lchunk, rchunk) {
			return false
		}
		rempty := rrest == ""
		if lempty {
			return rempty
		}
		left, right = lrest, rrest
	}
}

// chunkIncludes reports whether a non-`**` left chunk includes the
// right chunk. `*` includes everything; literal only includes itself.
func chunkIncludes(lchunk, rchunk string) bool {
	if lchunk == rchunk {
		return true
	}
	return lchunk == singleWild
}
