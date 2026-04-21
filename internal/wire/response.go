package wire

import (
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/codec"
)

// Response is the RESPONSE network message (ID 0x1B). Carries one REPLY
// or ERR sub-message, correlated to a REQUEST via RequestID.
//
// Flags: N (Named suffix) / M (Mapping) / Z (extensions).
type Response struct {
	RequestID  uint32
	KeyExpr    WireExpr
	Extensions []codec.Extension
	Body       ResponseBody
}

// ResponseBody is either a ReplyBody (successful) or an ErrBody (error).
type ResponseBody interface {
	SubID() byte
	EncodeResponseBody(w *codec.Writer) error
}

func (m *Response) EncodeTo(w *codec.Writer) error {
	if m.Body == nil {
		return fmt.Errorf("wire: RESPONSE with nil body")
	}
	h := codec.Header{
		ID: IDNetworkResponse,
		F1: m.KeyExpr.Named(),
		F2: m.KeyExpr.Mapping,
		Z:  len(m.Extensions) > 0,
	}
	w.EncodeHeader(h)
	w.EncodeZ32(m.RequestID)
	if err := m.KeyExpr.EncodeScope(w); err != nil {
		return err
	}
	if h.Z {
		if err := w.EncodeExtensions(m.Extensions); err != nil {
			return err
		}
	}
	return m.Body.EncodeResponseBody(w)
}

// DecodeResponse reads a RESPONSE (outer header already consumed).
func DecodeResponse(r *codec.Reader, h codec.Header) (*Response, error) {
	if h.ID != IDNetworkResponse {
		return nil, fmt.Errorf("wire: expected RESPONSE header, got id=%#x", h.ID)
	}
	m := &Response{}
	var err error
	if m.RequestID, err = r.DecodeZ32(); err != nil {
		return nil, err
	}
	if m.KeyExpr, err = DecodeWireExpr(r, h.F1, h.F2); err != nil {
		return nil, err
	}
	if h.Z {
		if m.Extensions, err = r.DecodeExtensions(); err != nil {
			return nil, fmt.Errorf("RESPONSE extensions: %w", err)
		}
	}
	body, err := decodeResponseBody(r)
	if err != nil {
		return nil, fmt.Errorf("RESPONSE body: %w", err)
	}
	m.Body = body
	return m, nil
}

func decodeResponseBody(r *codec.Reader) (ResponseBody, error) {
	h, err := r.DecodeHeader()
	if err != nil {
		return nil, err
	}
	switch h.ID {
	case IDDataReply:
		return decodeReplyBody(r, h)
	case IDDataErr:
		return decodeErrBody(r, h)
	default:
		return nil, fmt.Errorf("wire: unknown RESPONSE body id=%#x", h.ID)
	}
}

// ReplyBody is the REPLY sub-message (0x04). Wraps a PUT or DEL PushBody.
//
// Flags: C (Consolidation) / reserved / Z.
type ReplyBody struct {
	Consolidation *ConsolidationMode
	Extensions    []codec.Extension
	Inner         PushBody // PutBody or DelBody
}

func (r *ReplyBody) SubID() byte { return IDDataReply }

func (r *ReplyBody) EncodeResponseBody(w *codec.Writer) error {
	if r.Inner == nil {
		return fmt.Errorf("wire: REPLY with nil inner PushBody")
	}
	h := codec.Header{
		ID: IDDataReply,
		F1: r.Consolidation != nil,
		Z:  len(r.Extensions) > 0,
	}
	w.EncodeHeader(h)
	if h.F1 {
		w.AppendByte(byte(*r.Consolidation))
	}
	if h.Z {
		if err := w.EncodeExtensions(r.Extensions); err != nil {
			return err
		}
	}
	return r.Inner.EncodePushBody(w)
}

func decodeReplyBody(r *codec.Reader, h codec.Header) (*ReplyBody, error) {
	if h.ID != IDDataReply {
		return nil, fmt.Errorf("wire: expected REPLY sub-msg, got id=%#x", h.ID)
	}
	body := &ReplyBody{}
	if h.F1 {
		b, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		cm := ConsolidationMode(b)
		body.Consolidation = &cm
	}
	if h.Z {
		var err error
		if body.Extensions, err = r.DecodeExtensions(); err != nil {
			return nil, fmt.Errorf("REPLY extensions: %w", err)
		}
	}
	inner, err := decodePushBody(r)
	if err != nil {
		return nil, fmt.Errorf("REPLY inner: %w", err)
	}
	body.Inner = inner
	return body, nil
}

// ErrBody is the ERR sub-message (0x05). Carries an application-defined
// error payload with optional encoding.
//
// Flags: reserved / E (Encoding present, bit 6) / Z.
type ErrBody struct {
	Encoding   *Encoding
	Extensions []codec.Extension
	Payload    []byte // aliases decoder buffer on decode
}

func (e *ErrBody) SubID() byte { return IDDataErr }

func (e *ErrBody) EncodeResponseBody(w *codec.Writer) error {
	h := codec.Header{
		ID: IDDataErr,
		F2: e.Encoding != nil,
		Z:  len(e.Extensions) > 0,
	}
	w.EncodeHeader(h)
	if h.F2 {
		if err := e.Encoding.EncodeTo(w); err != nil {
			return err
		}
	}
	if h.Z {
		if err := w.EncodeExtensions(e.Extensions); err != nil {
			return err
		}
	}
	return w.EncodeBytesZ32(e.Payload)
}

func decodeErrBody(r *codec.Reader, h codec.Header) (*ErrBody, error) {
	if h.ID != IDDataErr {
		return nil, fmt.Errorf("wire: expected ERR sub-msg, got id=%#x", h.ID)
	}
	body := &ErrBody{}
	if h.F2 {
		enc, err := DecodeEncoding(r)
		if err != nil {
			return nil, fmt.Errorf("ERR encoding: %w", err)
		}
		body.Encoding = &enc
	}
	if h.Z {
		var err error
		if body.Extensions, err = r.DecodeExtensions(); err != nil {
			return nil, fmt.Errorf("ERR extensions: %w", err)
		}
	}
	payload, err := r.DecodeBytesZ32()
	if err != nil {
		return nil, fmt.Errorf("ERR payload: %w", err)
	}
	body.Payload = payload
	return body, nil
}

// ResponseFinal is the RESPONSE_FINAL network message (ID 0x1A). Signals
// end-of-replies for a given request_id.
//
// Flags: Z (extensions) only.
type ResponseFinal struct {
	RequestID  uint32
	Extensions []codec.Extension
}

func (m *ResponseFinal) EncodeTo(w *codec.Writer) error {
	h := codec.Header{
		ID: IDNetworkResponseFinal,
		Z:  len(m.Extensions) > 0,
	}
	w.EncodeHeader(h)
	w.EncodeZ32(m.RequestID)
	if h.Z {
		return w.EncodeExtensions(m.Extensions)
	}
	return nil
}

func DecodeResponseFinal(r *codec.Reader, h codec.Header) (*ResponseFinal, error) {
	if h.ID != IDNetworkResponseFinal {
		return nil, fmt.Errorf("wire: expected RESPONSE_FINAL header, got id=%#x", h.ID)
	}
	m := &ResponseFinal{}
	var err error
	if m.RequestID, err = r.DecodeZ32(); err != nil {
		return nil, err
	}
	if h.Z {
		if m.Extensions, err = r.DecodeExtensions(); err != nil {
			return nil, fmt.Errorf("RESPONSE_FINAL extensions: %w", err)
		}
	}
	return m, nil
}
