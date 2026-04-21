package wire

import (
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/codec"
)

// PutBody is a PUT sub-message (ID 0x01) nested inside a PUSH or REPLY.
//
// Flags: T (Timestamp present, bit 5) / E (Encoding present, bit 6) / Z.
//
// Wire:
//
//	header
//	Timestamp     — if T==1
//	Encoding      — if E==1
//	[put_exts]    — if Z==1 (SourceInfo, Shm, Attachment)
//	payload <u8;z32>
type PutBody struct {
	Timestamp  *Timestamp
	Encoding   *Encoding
	Extensions []codec.Extension
	Payload    []byte // aliases decoder buffer on decode; caller copies if needed
}

func (p *PutBody) SubID() byte { return IDDataPut }

func (p *PutBody) EncodePushBody(w *codec.Writer) error { return p.EncodeTo(w) }

// EncodeTo writes a PUT sub-message. Kept separate from EncodePushBody so
// REPLY (which reuses the same layout) can share it.
func (p *PutBody) EncodeTo(w *codec.Writer) error {
	h := codec.Header{
		ID: IDDataPut,
		F1: p.Timestamp != nil,
		F2: p.Encoding != nil,
		Z:  len(p.Extensions) > 0,
	}
	w.EncodeHeader(h)
	if h.F1 {
		if err := p.Timestamp.EncodeTo(w); err != nil {
			return err
		}
	}
	if h.F2 {
		if err := p.Encoding.EncodeTo(w); err != nil {
			return err
		}
	}
	if h.Z {
		if err := w.EncodeExtensions(p.Extensions); err != nil {
			return err
		}
	}
	if err := w.EncodeBytesZ32(p.Payload); err != nil {
		return fmt.Errorf("PUT payload: %w", err)
	}
	return nil
}

func decodePutBody(r *codec.Reader, h codec.Header) (*PutBody, error) {
	if h.ID != IDDataPut {
		return nil, fmt.Errorf("wire: expected PUT sub-msg, got id=%#x", h.ID)
	}
	p := &PutBody{}
	if h.F1 {
		ts, err := DecodeTimestamp(r)
		if err != nil {
			return nil, fmt.Errorf("PUT timestamp: %w", err)
		}
		p.Timestamp = &ts
	}
	if h.F2 {
		enc, err := DecodeEncoding(r)
		if err != nil {
			return nil, fmt.Errorf("PUT encoding: %w", err)
		}
		p.Encoding = &enc
	}
	if h.Z {
		exts, err := r.DecodeExtensions()
		if err != nil {
			return nil, fmt.Errorf("PUT extensions: %w", err)
		}
		p.Extensions = exts
	}
	payload, err := r.DecodeBytesZ32()
	if err != nil {
		return nil, fmt.Errorf("PUT payload: %w", err)
	}
	p.Payload = payload
	return p, nil
}

// DelBody is a DEL sub-message (ID 0x02). No payload.
//
// Flags: T (Timestamp present, bit 5) / Z.
type DelBody struct {
	Timestamp  *Timestamp
	Extensions []codec.Extension
}

func (d *DelBody) SubID() byte { return IDDataDel }

func (d *DelBody) EncodePushBody(w *codec.Writer) error { return d.EncodeTo(w) }

func (d *DelBody) EncodeTo(w *codec.Writer) error {
	h := codec.Header{
		ID: IDDataDel,
		F1: d.Timestamp != nil,
		Z:  len(d.Extensions) > 0,
	}
	w.EncodeHeader(h)
	if h.F1 {
		if err := d.Timestamp.EncodeTo(w); err != nil {
			return err
		}
	}
	if h.Z {
		return w.EncodeExtensions(d.Extensions)
	}
	return nil
}

func decodeDelBody(r *codec.Reader, h codec.Header) (*DelBody, error) {
	if h.ID != IDDataDel {
		return nil, fmt.Errorf("wire: expected DEL sub-msg, got id=%#x", h.ID)
	}
	d := &DelBody{}
	if h.F1 {
		ts, err := DecodeTimestamp(r)
		if err != nil {
			return nil, err
		}
		d.Timestamp = &ts
	}
	if h.Z {
		exts, err := r.DecodeExtensions()
		if err != nil {
			return nil, err
		}
		d.Extensions = exts
	}
	return d, nil
}
