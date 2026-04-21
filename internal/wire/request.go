package wire

import (
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/codec"
)

// QueryTarget controls which matching queryables receive a query. Wire
// values match the spec.
type QueryTarget uint8

const (
	QueryTargetBestMatching QueryTarget = 0x00
	QueryTargetAll          QueryTarget = 0x01
	QueryTargetAllComplete  QueryTarget = 0x02

	QueryTargetDefault = QueryTargetBestMatching
)

// ConsolidationMode controls how overlapping replies are consolidated.
type ConsolidationMode uint8

const (
	ConsolidationAuto      ConsolidationMode = 0x00
	ConsolidationNone      ConsolidationMode = 0x01
	ConsolidationMonotonic ConsolidationMode = 0x02
	ConsolidationLatest    ConsolidationMode = 0x03
)

// REQUEST extension IDs (Z64 unless noted).
const (
	ReqExtIDQoS         = 0x01
	ReqExtIDTimestamp   = 0x02
	ReqExtIDNodeID      = 0x03
	ReqExtIDQueryTarget = 0x04
	ReqExtIDBudget      = 0x05
	ReqExtIDTimeout     = 0x06
)

// Request is the REQUEST network message (ID 0x1C). Carries a QUERY body.
//
// Flags: N (Named suffix, bit 5) / M (Mapping, bit 6) / Z (extensions).
type Request struct {
	RequestID  uint32
	KeyExpr    WireExpr
	Extensions []codec.Extension
	Body       *QueryBody
}

// EncodeTo emits the REQUEST outer layer plus its QUERY body.
func (m *Request) EncodeTo(w *codec.Writer) error {
	if m.Body == nil {
		return fmt.Errorf("wire: REQUEST with nil body")
	}
	h := codec.Header{
		ID: IDNetworkRequest,
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
	return m.Body.EncodeTo(w)
}

// DecodeRequest reads a REQUEST network message (outer header already consumed).
func DecodeRequest(r *codec.Reader, h codec.Header) (*Request, error) {
	if h.ID != IDNetworkRequest {
		return nil, fmt.Errorf("wire: expected REQUEST header, got id=%#x", h.ID)
	}
	m := &Request{}
	var err error
	if m.RequestID, err = r.DecodeZ32(); err != nil {
		return nil, err
	}
	if m.KeyExpr, err = DecodeWireExpr(r, h.F1, h.F2); err != nil {
		return nil, err
	}
	if h.Z {
		if m.Extensions, err = r.DecodeExtensions(); err != nil {
			return nil, fmt.Errorf("REQUEST extensions: %w", err)
		}
	}
	body, err := decodeQueryBody(r)
	if err != nil {
		return nil, fmt.Errorf("REQUEST body: %w", err)
	}
	m.Body = body
	return m, nil
}

// QueryBody is the QUERY data sub-message (ID 0x03) that lives inside a REQUEST.
//
// Flags: C (Consolidation present, bit 5) / P (Parameters present, bit 6) / Z.
type QueryBody struct {
	Consolidation    *ConsolidationMode // nil = flag C=0
	Parameters       string             // empty = flag P=0
	Extensions       []codec.Extension
}

// EncodeTo writes a QUERY sub-message (the C/P flags are derived from the
// non-nil/non-empty checks).
func (q *QueryBody) EncodeTo(w *codec.Writer) error {
	h := codec.Header{
		ID: IDDataQuery,
		F1: q.Consolidation != nil,
		F2: q.Parameters != "",
		Z:  len(q.Extensions) > 0,
	}
	w.EncodeHeader(h)
	if h.F1 {
		w.AppendByte(byte(*q.Consolidation))
	}
	if h.F2 {
		if err := w.EncodeStringZ16(q.Parameters); err != nil {
			return err
		}
	}
	if h.Z {
		return w.EncodeExtensions(q.Extensions)
	}
	return nil
}

func decodeQueryBody(r *codec.Reader) (*QueryBody, error) {
	h, err := r.DecodeHeader()
	if err != nil {
		return nil, err
	}
	if h.ID != IDDataQuery {
		return nil, fmt.Errorf("wire: expected QUERY sub-msg, got id=%#x", h.ID)
	}
	q := &QueryBody{}
	if h.F1 {
		b, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		cm := ConsolidationMode(b)
		q.Consolidation = &cm
	}
	if h.F2 {
		s, err := r.DecodeStringZ16()
		if err != nil {
			return nil, err
		}
		q.Parameters = s
	}
	if h.Z {
		if q.Extensions, err = r.DecodeExtensions(); err != nil {
			return nil, fmt.Errorf("QUERY extensions: %w", err)
		}
	}
	return q, nil
}
