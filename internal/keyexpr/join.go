package keyexpr

// Join returns the key expression "k / suffix" (with a `/` separator). The
// suffix itself must parse as a key expression (via Autocanonize) — it may
// contain multiple chunks.
//
// Example:
//
//	a, _ := New("demo")
//	b, _ := a.Join("foo/*")   // "demo/foo/*"
func (k KeyExpr) Join(suffix string) (KeyExpr, error) {
	if k.s == "" {
		return Autocanonize(suffix)
	}
	return Autocanonize(k.s + delimiter + suffix)
}

// Concat returns "k || suffix" — no `/` is inserted. The concatenated
// result must parse as a canonical key expression.
//
// Example:
//
//	a, _ := New("demo/foo")
//	b, _ := a.Concat("bar")   // "demo/foobar"
func (k KeyExpr) Concat(suffix string) (KeyExpr, error) {
	if suffix == "" {
		return k, nil
	}
	return Autocanonize(k.s + suffix)
}
