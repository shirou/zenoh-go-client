package wire

import (
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/codec"
)

// MaxEncodingID is the largest encoding ID that fits in the 31-bit field.
const MaxEncodingID uint32 = 0x7FFFFFFF

// Encoding is the wire representation of a zenoh payload encoding.
//
// Wire format:
//
//	~ id_and_S : z32 ~   upper 31 bits = encoding_id, bit 0 = schema flag
//	~ schema         ~   if S==1: <u8;z8> schema bytes
type Encoding struct {
	ID     uint32 // 0..MaxEncodingID
	Schema string // empty = no schema (S=0)
}

// EncodeTo writes the encoding field. Returns an error if ID > MaxEncodingID
// (would overflow the 31-bit wire field and silently produce a different ID).
func (e Encoding) EncodeTo(w *codec.Writer) error {
	if e.ID > MaxEncodingID {
		return fmt.Errorf("wire: encoding ID %d exceeds 31-bit wire field (max %d)", e.ID, MaxEncodingID)
	}
	idAndS := e.ID << 1
	if e.Schema != "" {
		idAndS |= 1
	}
	w.EncodeZ32(idAndS)
	if e.Schema != "" {
		if err := w.EncodeStringZ8(e.Schema); err != nil {
			return err
		}
	}
	return nil
}

// DecodeEncoding reads an encoding field.
func DecodeEncoding(r *codec.Reader) (Encoding, error) {
	idAndS, err := r.DecodeZ32()
	if err != nil {
		return Encoding{}, err
	}
	e := Encoding{ID: idAndS >> 1}
	if idAndS&1 == 1 {
		s, err := r.DecodeStringZ8()
		if err != nil {
			return Encoding{}, err
		}
		e.Schema = s
	}
	return e, nil
}
