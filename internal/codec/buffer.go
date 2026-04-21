package codec

import (
	"errors"
	"fmt"
)

// ErrUnexpectedEOF is returned when a decoder runs out of bytes.
var ErrUnexpectedEOF = errors.New("codec: unexpected end of buffer")

// ErrOverflow is returned when a VLE-encoded integer does not fit the
// target width. This protects decoders from malicious peers emitting
// overlong encodings.
var ErrOverflow = errors.New("codec: integer overflow")

// Writer is an append-only byte buffer used by encoders. The zero-value is
// ready to use.
//
// Writer is intentionally small — just a byte slice — so most call sites
// can stack-allocate or reuse buffers via Reset.
type Writer struct {
	buf []byte
}

// NewWriter returns a Writer backed by a freshly allocated buffer with the
// given initial capacity.
func NewWriter(capacity int) *Writer {
	return &Writer{buf: make([]byte, 0, capacity)}
}

// WriterFromSlice adopts an existing slice as the writer's backing buffer.
// Useful for hot-path writers that want to reuse a caller-owned scratch
// buffer between encodes without allocating.
//
// The caller should truncate the slice to length zero first if it wants a
// fresh write; WriterFromSlice does not clear.
func WriterFromSlice(buf []byte) *Writer {
	return &Writer{buf: buf}
}

// Bytes returns the accumulated bytes.
//
// The returned slice aliases the Writer's internal buffer. It remains valid
// ONLY until the next call to AppendByte, AppendBytes, any Encode method,
// or Reset on this Writer — any of which may trigger slice re-allocation
// and invalidate the returned pointer. Callers that need durable bytes
// MUST copy the result.
func (w *Writer) Bytes() []byte { return w.buf }

// Len returns the number of bytes written.
func (w *Writer) Len() int { return len(w.buf) }

// Reset discards previously written bytes, keeping the backing array for
// reuse.
func (w *Writer) Reset() { w.buf = w.buf[:0] }

// AppendByte appends a single byte.
func (w *Writer) AppendByte(b byte) { w.buf = append(w.buf, b) }

// AppendBytes appends all bytes from p.
func (w *Writer) AppendBytes(p []byte) { w.buf = append(w.buf, p...) }

// AppendU16LE appends a fixed 2-byte little-endian uint16.
// Used wherever the spec specifies a fixed-width u16 LE field
// (e.g. INIT/JOIN batch_size, stream framing length prefix).
func (w *Writer) AppendU16LE(v uint16) {
	w.buf = append(w.buf, byte(v), byte(v>>8))
}

// ReadU16LE reads a fixed 2-byte little-endian uint16.
func (r *Reader) ReadU16LE() (uint16, error) {
	b, err := r.ReadN(2)
	if err != nil {
		return 0, err
	}
	return uint16(b[0]) | uint16(b[1])<<8, nil
}

// Encoder is implemented by any wire message that can serialise itself
// into a Writer. Shared by transport.EncodeNetworkMessage, the handshake
// writeTransport helper, Session.enqueueNetwork, and test fixtures.
type Encoder interface {
	EncodeTo(w *Writer) error
}

// Reader is a read cursor over a byte slice.
//
// All decoders share a Reader so they can compose without copying: the
// decoder advances Reader.off by the number of bytes consumed.
type Reader struct {
	buf []byte
	off int
}

// NewReader returns a Reader positioned at the start of buf. The slice is
// not copied.
func NewReader(buf []byte) *Reader { return &Reader{buf: buf} }

// Len returns the number of bytes remaining.
func (r *Reader) Len() int { return len(r.buf) - r.off }

// Bytes returns the unread bytes.
//
// The returned slice aliases the Reader's backing buffer. Higher-level
// wire decoders that expose sub-slices to callers (e.g. Frame.Body,
// Fragment.Body) should document whether the caller may retain the slice
// or must copy it, because the parent buffer is typically reused across
// batch reads.
func (r *Reader) Bytes() []byte { return r.buf[r.off:] }

// Offset returns the current read offset from the start of the backing buffer.
func (r *Reader) Offset() int { return r.off }

// ReadByte consumes and returns one byte.
func (r *Reader) ReadByte() (byte, error) {
	if r.off >= len(r.buf) {
		return 0, ErrUnexpectedEOF
	}
	b := r.buf[r.off]
	r.off++
	return b, nil
}

// PeekByte returns the next byte without consuming it.
func (r *Reader) PeekByte() (byte, error) {
	if r.off >= len(r.buf) {
		return 0, ErrUnexpectedEOF
	}
	return r.buf[r.off], nil
}

// ReadN consumes n bytes and returns a slice aliasing the buffer.
func (r *Reader) ReadN(n int) ([]byte, error) {
	if n < 0 {
		return nil, fmt.Errorf("codec: negative read length %d", n)
	}
	if r.off+n > len(r.buf) {
		return nil, ErrUnexpectedEOF
	}
	out := r.buf[r.off : r.off+n]
	r.off += n
	return out, nil
}

// Skip advances the cursor by n bytes.
func (r *Reader) Skip(n int) error {
	if n < 0 {
		return fmt.Errorf("codec: negative skip length %d", n)
	}
	if r.off+n > len(r.buf) {
		return ErrUnexpectedEOF
	}
	r.off += n
	return nil
}

// SubReader returns a new Reader covering the next n bytes. The parent's
// offset is advanced by n. Use this when parsing a length-prefixed payload
// whose interior must not be over-read.
func (r *Reader) SubReader(n int) (*Reader, error) {
	b, err := r.ReadN(n)
	if err != nil {
		return nil, err
	}
	return NewReader(b), nil
}
