package wire

import (
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/codec"
)

// Timestamp is an HLC-compatible timestamp: NTP64 time + originator ZID.
//
// Wire format (inside extension bodies):
//
//	% ntp64 : z64  %
//	% zid_len : z8 %   1..16
//	~ ZID           ~
type Timestamp struct {
	NTP64 uint64
	ZID   ZenohID
}

// EncodeTo writes a Timestamp. Returns an error if the ZID is not 1..16 bytes.
func (t Timestamp) EncodeTo(w *codec.Writer) error {
	if !t.ZID.IsValid() {
		return fmt.Errorf("wire: Timestamp ZID must be 1..16 bytes (got %d)", t.ZID.Len())
	}
	w.EncodeZ64(t.NTP64)
	w.EncodeZ8(uint8(t.ZID.Len()))
	w.AppendBytes(t.ZID.Bytes)
	return nil
}

// DecodeTimestamp reads a Timestamp from r.
func DecodeTimestamp(r *codec.Reader) (Timestamp, error) {
	ntp, err := r.DecodeZ64()
	if err != nil {
		return Timestamp{}, err
	}
	zidLen, err := r.DecodeZ8()
	if err != nil {
		return Timestamp{}, err
	}
	zid, err := DecodeZIDBytes(r, int(zidLen))
	if err != nil {
		return Timestamp{}, err
	}
	return Timestamp{NTP64: ntp, ZID: zid}, nil
}
