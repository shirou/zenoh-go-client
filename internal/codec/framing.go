package codec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MaxBatchSize is the largest single batch size on stream-framed transports.
// Dictated by the u16 LE length prefix.
const MaxBatchSize = 65535

// ErrBatchTooLarge is returned when trying to frame a batch larger than MaxBatchSize.
var ErrBatchTooLarge = errors.New("codec: batch exceeds u16 length prefix")

// EncodeStreamBatch writes [u16 LE length][batch...] to w.
// Returns ErrBatchTooLarge if len(batch) > MaxBatchSize.
func EncodeStreamBatch(w io.Writer, batch []byte) error {
	if len(batch) > MaxBatchSize {
		return fmt.Errorf("%w: got %d", ErrBatchTooLarge, len(batch))
	}
	var hdr [2]byte
	binary.LittleEndian.PutUint16(hdr[:], uint16(len(batch)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := w.Write(batch); err != nil {
		return err
	}
	return nil
}

// ReadStreamBatch reads a length-prefixed batch. It returns a slice aliasing
// the provided buf; caller must size buf to at least MaxBatchSize.
//
// On io.EOF from the length prefix read the function returns io.EOF; partial
// reads of the length or body yield io.ErrUnexpectedEOF.
func ReadStreamBatch(r io.Reader, buf []byte) (int, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, err
	}
	n := int(binary.LittleEndian.Uint16(hdr[:]))
	if len(buf) < n {
		return 0, fmt.Errorf("codec: read buffer too small (%d < %d)", len(buf), n)
	}
	if _, err := io.ReadFull(r, buf[:n]); err != nil {
		return 0, err
	}
	return n, nil
}
