package codec

import (
	"fmt"
	"unicode/utf8"
)

// Length-prefixed byte arrays. Spec notation:
//
//	<u8;z8>  → z8 length prefix, max 255 bytes
//	<u8;z16> → z16 length prefix, max 65535 bytes
//	<u8;z32> → z32 length prefix, max 2^32-1 bytes
//
// String variants carry UTF-8 bytes; length is byte-count.

// EncodeBytesZ8 writes <u8;z8>.
func (w *Writer) EncodeBytesZ8(b []byte) error {
	if len(b) > 0xFF {
		return fmt.Errorf("codec: byte slice too long for z8 length (%d)", len(b))
	}
	w.EncodeZ8(uint8(len(b)))
	w.AppendBytes(b)
	return nil
}

// EncodeBytesZ16 writes <u8;z16>.
func (w *Writer) EncodeBytesZ16(b []byte) error {
	if len(b) > 0xFFFF {
		return fmt.Errorf("codec: byte slice too long for z16 length (%d)", len(b))
	}
	w.EncodeZ16(uint16(len(b)))
	w.AppendBytes(b)
	return nil
}

// EncodeBytesZ32 writes <u8;z32>.
func (w *Writer) EncodeBytesZ32(b []byte) error {
	if uint64(len(b)) > 0xFFFFFFFF {
		return fmt.Errorf("codec: byte slice too long for z32 length (%d)", len(b))
	}
	w.EncodeZ32(uint32(len(b)))
	w.AppendBytes(b)
	return nil
}

// EncodeStringZ8 writes a UTF-8 string with a z8 length.
func (w *Writer) EncodeStringZ8(s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("codec: invalid UTF-8 string")
	}
	return w.EncodeBytesZ8([]byte(s))
}

// EncodeStringZ16 writes a UTF-8 string with a z16 length.
func (w *Writer) EncodeStringZ16(s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("codec: invalid UTF-8 string")
	}
	return w.EncodeBytesZ16([]byte(s))
}

// DecodeBytesZ8 reads <u8;z8> and returns a slice aliasing the buffer.
func (r *Reader) DecodeBytesZ8() ([]byte, error) {
	n, err := r.DecodeZ8()
	if err != nil {
		return nil, err
	}
	return r.ReadN(int(n))
}

// DecodeBytesZ16 reads <u8;z16> and returns a slice aliasing the buffer.
func (r *Reader) DecodeBytesZ16() ([]byte, error) {
	n, err := r.DecodeZ16()
	if err != nil {
		return nil, err
	}
	return r.ReadN(int(n))
}

// DecodeBytesZ32 reads <u8;z32> and returns a slice aliasing the buffer.
func (r *Reader) DecodeBytesZ32() ([]byte, error) {
	n, err := r.DecodeZ32()
	if err != nil {
		return nil, err
	}
	return r.ReadN(int(n))
}

// DecodeStringZ8 reads a z8-prefixed UTF-8 string. Validation is deferred to
// the caller if strict checking is needed.
func (r *Reader) DecodeStringZ8() (string, error) {
	b, err := r.DecodeBytesZ8()
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// DecodeStringZ16 reads a z16-prefixed UTF-8 string.
func (r *Reader) DecodeStringZ16() (string, error) {
	b, err := r.DecodeBytesZ16()
	if err != nil {
		return "", err
	}
	return string(b), nil
}
