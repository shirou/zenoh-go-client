package zenoh

import (
	"context"

	"github.com/shirou/zenoh-go-client/internal/session"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// Query is one inbound query delivered to a Queryable's handler.
// Reply / ReplyDel / ReplyErr emit response messages addressed to the
// querier. RESPONSE_FINAL is sent automatically when the handler returns;
// the handler MUST NOT call it itself.
type Query struct {
	session    *Session
	requestID  uint32
	keyExpr    KeyExpr
	parameters string
}

// KeyExpr returns the query's target key expression.
func (q *Query) KeyExpr() KeyExpr { return q.keyExpr }

// Parameters returns the query's parameters string (may be empty).
func (q *Query) Parameters() string { return q.parameters }

// QueryReplyOptions sets per-reply fields. A nil *QueryReplyOptions is
// treated as "all defaults".
type QueryReplyOptions struct {
	Encoding     Encoding
	HasEncoding  bool
	TimeStamp    TimeStamp
	HasTimeStamp bool
}

// Reply sends a successful RESPONSE carrying a PUT sub-message.
//
// A single query may receive multiple Reply / ReplyDel calls; the
// terminating RESPONSE_FINAL is emitted when the handler returns.
func (q *Query) Reply(keyExpr KeyExpr, payload ZBytes, opts *QueryReplyOptions) error {
	if keyExpr.IsZero() {
		return ErrInvalidKeyExpr
	}
	body := &wire.ReplyBody{
		Inner: &wire.PutBody{
			Payload:   payload.unsafeBytes(),
			Encoding:  encodingFromOpts(opts),
			Timestamp: timestampFromOpts(opts),
		},
	}
	return q.sendResponse(keyExpr, body)
}

// ReplyDel sends a successful RESPONSE carrying a DEL sub-message.
func (q *Query) ReplyDel(keyExpr KeyExpr, opts *QueryReplyOptions) error {
	if keyExpr.IsZero() {
		return ErrInvalidKeyExpr
	}
	body := &wire.ReplyBody{
		Inner: &wire.DelBody{
			Timestamp: timestampFromOpts(opts),
		},
	}
	return q.sendResponse(keyExpr, body)
}

// ReplyErr sends an error RESPONSE with the given payload.
func (q *Query) ReplyErr(payload ZBytes, encoding *Encoding) error {
	var enc *wire.Encoding
	if encoding != nil {
		w := encoding.toWire()
		enc = &w
	}
	body := &wire.ErrBody{Encoding: enc, Payload: payload.unsafeBytes()}
	return q.sendResponse(q.keyExpr, body)
}

func (q *Query) sendResponse(keyExpr KeyExpr, body wire.ResponseBody) error {
	// Control-plane use of context.Background(): the reply runs inside
	// the queryable's dispatch callback and has no caller ctx; the
	// runtime shuts down via Session.Close → LinkClosed.
	res := &wire.Response{
		RequestID: q.requestID,
		KeyExpr:   keyExpr.toWire(),
		Body:      body,
	}
	return q.session.enqueueNetwork(context.Background(), res, wire.QoSPriorityData, true, false)
}

// sendFinal emits RESPONSE_FINAL for this query.
//
// MUST share the Data priority lane with the preceding REPLYs — otherwise
// the router's flush-by-priority ordering places RESPONSE_FINAL on a
// different FRAME stream and it overtakes the replies, so the querier sees
// "final without replies".
func (q *Query) sendFinal() error {
	// Same rationale as sendResponse: fires inside the dispatch callback
	// after the handler returns, no caller ctx.
	rf := &wire.ResponseFinal{RequestID: q.requestID}
	return q.session.enqueueNetwork(context.Background(), rf, wire.QoSPriorityData, true, false)
}

func encodingFromOpts(opts *QueryReplyOptions) *wire.Encoding {
	if opts == nil || !opts.HasEncoding {
		return nil
	}
	e := opts.Encoding.toWire()
	return &e
}

func timestampFromOpts(opts *QueryReplyOptions) *wire.Timestamp {
	if opts == nil || !opts.HasTimeStamp {
		return nil
	}
	ts := wire.Timestamp{NTP64: opts.TimeStamp.Time(), ZID: opts.TimeStamp.Id().ToWireID()}
	return &ts
}

// buildQuery converts an internal QueryReceived to a public Query.
func buildQuery(s *Session, q session.QueryReceived) *Query {
	return &Query{
		session:    s,
		requestID:  q.RequestID,
		keyExpr:    KeyExpr{inner: q.ParsedKeyExpr},
		parameters: q.Parameters,
	}
}
