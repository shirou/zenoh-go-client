package zenoh

import (
	"github.com/shirou/zenoh-go-client/internal/session"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// Reply is one response delivered to a Get caller. Either the Sample
// accessor returns a data sample, or Err returns an error payload.
type Reply struct {
	keyExpr  KeyExpr
	sample   Sample
	hasSample bool
	errBody  *wire.ErrBody
}

// KeyExpr returns the key expression the replier used.
func (r Reply) KeyExpr() KeyExpr { return r.keyExpr }

// IsOk reports whether this is a data reply (Sample) rather than an error.
func (r Reply) IsOk() bool { return r.hasSample }

// Sample returns the sample and true if this is a data reply; zero Sample
// and false if this is an error reply.
func (r Reply) Sample() (Sample, bool) { return r.sample, r.hasSample }

// Err returns the error payload and true if this is an error reply.
func (r Reply) Err() (ZBytes, Encoding, bool) {
	if r.errBody == nil {
		return ZBytes{}, Encoding{}, false
	}
	enc := Encoding{}
	if r.errBody.Encoding != nil {
		enc = encodingFromWire(*r.errBody.Encoding)
	}
	return NewZBytes(r.errBody.Payload), enc, true
}

// GetOptions controls a Session.Get call.
type GetOptions struct {
	Consolidation     ConsolidationMode
	HasConsolidation  bool
	Parameters        string  // optional query parameters string
	// Buffer is the reply-channel capacity; 0 → default 16. Overflow drops
	// replies silently (Budget extension support to propagate upstream is
	// a Phase 6+ enhancement).
	Buffer int
}

// ConsolidationMode mirrors the zenoh-spec ConsolidationMode values.
type ConsolidationMode uint8

const (
	ConsolidationAuto      ConsolidationMode = 0x00
	ConsolidationNone      ConsolidationMode = 0x01
	ConsolidationMonotonic ConsolidationMode = 0x02
	ConsolidationLatest    ConsolidationMode = 0x03
)

// Get sends a REQUEST addressed to keyExpr and returns a channel that
// yields each Reply until RESPONSE_FINAL (at which point the channel
// closes). Reply consolidation is currently not applied on the client
// side; the router performs routing-level consolidation via the mode
// advertised in the REQUEST.
func (s *Session) Get(keyExpr KeyExpr, opts *GetOptions) (<-chan Reply, error) {
	if s.closed.Load() {
		return nil, ErrSessionClosed
	}
	if keyExpr.IsZero() {
		return nil, ErrInvalidKeyExpr
	}

	reqID := s.inner.IDs().AllocRequestID()
	bufSize := 16
	if opts != nil && opts.Buffer > 0 {
		bufSize = opts.Buffer
	}
	inboundCh := s.inner.RegisterGet(reqID, bufSize)

	// Build and send REQUEST.
	body := &wire.QueryBody{}
	if opts != nil {
		if opts.HasConsolidation {
			cm := wire.ConsolidationMode(opts.Consolidation)
			body.Consolidation = &cm
		}
		body.Parameters = opts.Parameters
	}
	req := &wire.Request{
		RequestID: reqID,
		KeyExpr:   keyExpr.toWire(),
		Body:      body,
	}
	if err := s.enqueueNetwork(req, wire.QoSPriorityData, true, false); err != nil {
		s.inner.CancelGet(reqID)
		return nil, err
	}

	// Spawn a translator goroutine: internal InboundReply → public Reply.
	out := make(chan Reply, bufSize)
	go func() {
		defer close(out)
		for raw := range inboundCh {
			out <- buildPublicReply(raw)
		}
	}()
	return out, nil
}

func buildPublicReply(raw session.InboundReply) Reply {
	r := Reply{keyExpr: KeyExpr{inner: raw.ParsedKeyExpr}}
	switch {
	case raw.Put != nil:
		r.sample = Sample{
			keyExpr: r.keyExpr,
			payload: NewZBytes(raw.Put.Payload),
			kind:    SampleKindPut,
		}
		if raw.Put.Encoding != nil {
			r.sample.encoding = encodingFromWire(*raw.Put.Encoding)
			r.sample.hasEnc = true
		}
		if raw.Put.Timestamp != nil {
			r.sample.ts = NewTimeStamp(raw.Put.Timestamp.NTP64, IdFromWireID(raw.Put.Timestamp.ZID))
			r.sample.hasTS = true
		}
		r.hasSample = true
	case raw.Del != nil:
		r.sample = Sample{keyExpr: r.keyExpr, kind: SampleKindDelete}
		if raw.Del.Timestamp != nil {
			r.sample.ts = NewTimeStamp(raw.Del.Timestamp.NTP64, IdFromWireID(raw.Del.Timestamp.ZID))
			r.sample.hasTS = true
		}
		r.hasSample = true
	case raw.Err != nil:
		r.errBody = raw.Err
	}
	return r
}
