// Package codec implements the byte-level wire primitives used by the Zenoh
// 1.0 protocol (wire version 0x09):
//
//   - Variable-length integer encoding (VLE / LEB128) in widths z8/z16/z32/z64
//   - TCP stream framing: [u16 LE length][batch bytes]
//   - Message header byte (ID in bits 4:0, message-specific flags in 5/6, Z in 7)
//   - Extension TLV (Z / ENC / M / ExtID) with Unit, Z64, ZBuf bodies
//   - Length-prefixed byte arrays (<u8;z8>, <u8;z16>) and UTF-8 strings
//
// The codec is pure: it operates on bytes only and has no knowledge of
// higher-level message types or network I/O. Callers that need a
// message-level view use the internal/wire package which sits on top.
//
// All encoders append to a caller-provided *Writer. All decoders read from a
// *Reader which tracks an offset over a byte slice and never allocates. Both
// types are lightweight value types; they may be passed by pointer or
// embedded as needed.
package codec
