// Package keyexpr implements zenoh key-expression validation, canonical-form
// enforcement, and the set relations (intersects, includes) that routers
// use to decide whether a PUSH or a subscriber declaration reach each other.
//
// A key expression is a `/`-separated list of chunks where each chunk is
// either a literal string or one of the wildcards `*` (matches any single
// non-empty chunk) or `**` (matches zero or more chunks).
//
// This package implements the "classical" subset of the zenoh key-expression
// grammar: no `$*` partial-chunk wildcards (Zenoh-specific DSL extension).
// `$`, `#`, and `?` are rejected as forbidden characters.
//
// Spec: `repo/zenoh-spec/docs/modules/concepts/pages/key-expressions.adoc`.
package keyexpr
