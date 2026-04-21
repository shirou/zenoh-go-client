// Package wire defines Go structs for every Zenoh 1.0 wire message and the
// primitive values they carry (ZenohID, WireExpr, Timestamp, Encoding,
// Resolution, etc.). Each type provides an EncodeTo(*codec.Writer) and a
// matching package-level decoder.
//
// The split between internal/codec and internal/wire mirrors the split
// between "bytes" and "typed messages" in the Rust `zenoh-codec` and
// `zenoh-protocol` crates.
//
// These types are internal; the public-facing API in package zenoh wraps
// them.
package wire
