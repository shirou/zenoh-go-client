package session

import (
	"bytes"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// EncodeCloseMessage produces the framed bytes of a session-scoped CLOSE
// with the given reason, ready to be written as a stream batch.
func EncodeCloseMessage(reason uint8) ([]byte, error) {
	w := codec.NewWriter(8)
	if err := (&wire.Close{Session: true, Reason: reason}).EncodeTo(w); err != nil {
		return nil, err
	}
	return bytes.Clone(w.Bytes()), nil
}
