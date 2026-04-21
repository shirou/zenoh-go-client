package zenoh

import "bytes"

// ZBytes is a zenoh payload — an opaque byte slice. The zero value is an
// empty payload. Construct from a []byte (copied) or a string (converted).
type ZBytes struct {
	b []byte
}

// NewZBytes returns a ZBytes with a copy of data.
func NewZBytes(data []byte) ZBytes { return ZBytes{b: bytes.Clone(data)} }

// NewZBytesFromString returns a ZBytes containing the UTF-8 bytes of s.
func NewZBytesFromString(s string) ZBytes { return ZBytes{b: []byte(s)} }

// Bytes returns a copy of the payload bytes.
func (z ZBytes) Bytes() []byte { return bytes.Clone(z.b) }

// String interprets the payload bytes as a UTF-8 string.
func (z ZBytes) String() string { return string(z.b) }

// Len returns the payload length in bytes.
func (z ZBytes) Len() int { return len(z.b) }

// unsafeBytes returns a direct reference to the payload slice (no copy).
// Used internally on encode paths where we know the caller won't mutate.
func (z ZBytes) unsafeBytes() []byte { return z.b }
