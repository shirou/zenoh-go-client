package zenoh

import (
	"time"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/session"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// QueryTarget selects which matching queryables receive a Get. Wire values
// match the spec (query.adoc §QueryTarget Extension).
type QueryTarget uint8

const (
	QueryTargetBestMatching QueryTarget = 0x00 // default — nearest matching queryable
	QueryTargetAll          QueryTarget = 0x01 // all matching queryables
	QueryTargetAllComplete  QueryTarget = 0x02 // all matching queryables whose Complete flag is set

	// QueryTargetDefault mirrors zenoh-go; kept for API parity.
	QueryTargetDefault = QueryTargetBestMatching
)

// Reply is one response delivered to a Get caller. Either the Sample
// accessor returns a data sample, or Err returns an error payload.
type Reply struct {
	keyExpr   KeyExpr
	sample    Sample
	hasSample bool
	errBody   *wire.ErrBody
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
	Consolidation    ConsolidationMode
	HasConsolidation bool
	Parameters       string // optional query parameters string

	// Target selects which matching queryables receive the query.
	// HasTarget=false omits the QueryTarget extension and lets the router
	// apply the spec default (BestMatching). Because the zero Target value
	// is also BestMatching, callers who want that explicit must set
	// HasTarget=true.
	Target    QueryTarget
	HasTarget bool

	// Budget caps the number of replies the querier will accept; routers
	// SHOULD stop forwarding further RESPONSE messages after the cap is
	// reached. Zero = unlimited (extension omitted). Spec calls for a
	// non-zero u32.
	Budget uint32

	// Timeout bounds how long the querier waits for replies. Zero = no
	// querier-side limit (extension omitted); the router MAY still impose
	// its own timeout.
	Timeout time.Duration

	// Buffer is the reply-channel capacity; 0 → default 16. Overflow drops
	// replies silently.
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
	var exts []codec.Extension
	if opts != nil {
		if opts.HasConsolidation {
			cm := wire.ConsolidationMode(opts.Consolidation)
			body.Consolidation = &cm
		}
		body.Parameters = opts.Parameters
		if opts.HasTarget {
			exts = append(exts, wire.QueryTargetExt(wire.QueryTarget(opts.Target)))
		}
		if opts.Budget > 0 {
			exts = append(exts, wire.BudgetExt(opts.Budget))
		}
		if ms := opts.Timeout.Milliseconds(); ms > 0 {
			// Sub-millisecond durations would round to 0 and be confused with
			// "unset", so only emit the Timeout extension when we have at
			// least 1 ms to carry.
			exts = append(exts, wire.TimeoutExt(uint64(ms)))
		}
	}
	req := &wire.Request{
		RequestID:  reqID,
		KeyExpr:    keyExpr.toWire(),
		Extensions: exts,
		Body:       body,
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
