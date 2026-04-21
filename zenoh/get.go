package zenoh

import (
	"context"
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
	// Consolidation is the client-side reply consolidation mode.
	// HasConsolidation=false falls back to ConsolidationAuto (= Latest):
	// same-key replies are deduped and only the newest-timestamped one
	// per key is delivered. Set HasConsolidation=true +
	// ConsolidationNone to stream every reply in arrival order instead.
	//
	// Behaviour note: earlier pre-release builds of this library
	// defaulted to streaming. Callers relying on that behaviour must now
	// set HasConsolidation=true + ConsolidationNone explicitly.
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

	// cancelCtx is an optional extra cancellation source merged into the
	// Get's context. Kept unexported so only code built with
	// zenoh_unstable can set it (via the CancellationToken helper in
	// cancellation_token_unstable.go).
	cancelCtx context.Context
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
// closes). Equivalent to GetWithContext(context.Background(), …).
//
// Client-side consolidation is applied in the translator according to
// opts.Consolidation:
//
//	None           — stream every reply in arrival order.
//	Monotonic      — per key, drop replies whose timestamp is ≤ the
//	                 newest one already emitted for that key.
//	Latest / Auto  — buffer every reply and, on RESPONSE_FINAL, emit at
//	                 most one per key (the one with the largest timestamp;
//	                 ties broken by arrival order). Error replies are
//	                 flushed first in arrival order, then data replies in
//	                 first-arrival order.
//
// The consolidation mode is ALSO advertised on the REQUEST so the router
// can short-circuit where possible; the client-side pass is a safety net
// for multi-source deployments and for modes the router elides.
func (s *Session) Get(keyExpr KeyExpr, opts *GetOptions) (<-chan Reply, error) {
	return s.GetWithContext(context.Background(), keyExpr, opts)
}

// GetWithContext is Get with a caller-supplied context. Cancelling ctx
// closes the reply channel on the next reply boundary. opts.Timeout is
// additionally enforced locally as a deadline — a laggy router cannot leak
// the Get forever.
func (s *Session) GetWithContext(ctx context.Context, keyExpr KeyExpr, opts *GetOptions) (<-chan Reply, error) {
	if s.closed.Load() {
		return nil, ErrSessionClosed
	}
	if keyExpr.IsZero() {
		return nil, ErrInvalidKeyExpr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Merge any unstable CancellationToken context so either source
	// unblocks the Get. stop must be called when the Get finishes so the
	// context.AfterFunc registration on the token doesn't leak.
	stopCancelMerge := func() {}
	if opts != nil && opts.cancelCtx != nil {
		ctx, stopCancelMerge = mergeCancelContexts(ctx, opts.cancelCtx)
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
	var timeout time.Duration
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
			timeout = opts.Timeout
		}
	}
	req := &wire.Request{
		RequestID:  reqID,
		KeyExpr:    keyExpr.toWire(),
		Extensions: exts,
		Body:       body,
	}
	if err := s.enqueueNetwork(ctx, req, wire.QoSPriorityData, true, false); err != nil {
		stopCancelMerge()
		s.inner.CancelGet(reqID)
		return nil, err
	}

	// Spawn a translator goroutine: internal InboundReply → public Reply.
	// A finaliser goroutine races ctx.Done and the Timeout deadline against
	// RESPONSE_FINAL, cancelling the Get when either fires first. The
	// translator always exits cleanly because CancelGet closes inboundCh.
	out := make(chan Reply, bufSize)
	translatorExited := make(chan struct{})
	if ctx.Done() != nil || timeout > 0 {
		go s.runGetCancel(ctx, timeout, reqID, translatorExited)
	}
	mode := ConsolidationAuto
	if opts != nil && opts.HasConsolidation {
		mode = opts.Consolidation
	}
	go func() {
		defer stopCancelMerge()
		runReplyTranslator(ctx, inboundCh, out, translatorExited, mode)
	}()
	return out, nil
}

// runReplyTranslator reads raw internal replies from inboundCh, applies
// the consolidation mode, and forwards the resulting Replies to out.
// Closes out (and translatorExited) on inboundCh close or ctx cancel.
func runReplyTranslator(ctx context.Context, inboundCh <-chan session.InboundReply,
	out chan<- Reply, translatorExited chan<- struct{}, mode ConsolidationMode) {
	defer close(out)
	defer close(translatorExited)

	switch mode {
	case ConsolidationNone:
		runStreamTranslator(ctx, inboundCh, out)
	case ConsolidationMonotonic:
		runMonotonicTranslator(ctx, inboundCh, out)
	default: // Auto / Latest
		runLatestTranslator(ctx, inboundCh, out)
	}
}

func runStreamTranslator(ctx context.Context, inboundCh <-chan session.InboundReply, out chan<- Reply) {
	streamReplies(ctx, inboundCh, out, func(r Reply) (Reply, bool) { return r, true })
}

// runMonotonicTranslator emits a reply only when its timestamp is
// strictly greater than the largest seen so far for that key. Replies
// without a timestamp are always forwarded and never update the per-key
// high-water mark — the "unknown" timestamp is not allowed to poison the
// comparison. A source that never stamps therefore behaves like
// ConsolidationNone, which matches zenoh-rust.
func runMonotonicTranslator(ctx context.Context, inboundCh <-chan session.InboundReply, out chan<- Reply) {
	latest := make(map[string]uint64)
	streamReplies(ctx, inboundCh, out, func(reply Reply) (Reply, bool) {
		if reply.hasSample {
			if t, hasTS := reply.sample.TimeStamp(); hasTS {
				ts := t.Time()
				key := reply.keyExpr.inner.String()
				if prev, seen := latest[key]; seen && ts <= prev {
					return reply, false
				}
				latest[key] = ts
			}
		}
		return reply, true
	})
}

// streamReplies reads inbound replies, runs each through decide to get
// the public Reply and a keep flag, and forwards accepted ones to out.
// Exits cleanly on inboundCh close or ctx cancel.
func streamReplies(ctx context.Context, inboundCh <-chan session.InboundReply,
	out chan<- Reply, decide func(Reply) (Reply, bool)) {
	for raw := range inboundCh {
		reply, keep := decide(buildPublicReply(raw))
		if !keep {
			continue
		}
		select {
		case out <- reply:
		case <-ctx.Done():
			return
		}
	}
}

// runLatestTranslator buffers every reply and, once inboundCh closes,
// emits one reply per key — the one whose timestamp is greatest (or the
// last-arriving among those without a timestamp). Error replies are held
// in a separate slice so they always surface intact; keying them on the
// key-expression map would risk collisions with pathological keys.
func runLatestTranslator(ctx context.Context, inboundCh <-chan session.InboundReply, out chan<- Reply) {
	type entry struct {
		reply Reply
		ts    uint64
		hasTS bool
	}
	buf := make(map[string]entry)
	order := make([]string, 0, 8) // preserve first-insertion order for stable flush
	var errReplies []Reply

	take := func(raw session.InboundReply) {
		reply := buildPublicReply(raw)
		if !reply.hasSample {
			errReplies = append(errReplies, reply)
			return
		}
		key := reply.keyExpr.inner.String()
		var ts uint64
		var hasTS bool
		if t, ok := reply.sample.TimeStamp(); ok {
			ts, hasTS = t.Time(), true
		}
		if prev, seen := buf[key]; seen {
			if hasTS && prev.hasTS && ts <= prev.ts {
				return
			}
			// New entry wins (newer timestamp, or prev had no ts).
			buf[key] = entry{reply: reply, ts: ts, hasTS: hasTS}
			return
		}
		buf[key] = entry{reply: reply, ts: ts, hasTS: hasTS}
		order = append(order, key)
	}

	for {
		select {
		case raw, ok := <-inboundCh:
			if !ok {
				// Final — flush errors first (arrival order), then
				// data replies in first-insertion order.
				for _, r := range errReplies {
					select {
					case out <- r:
					case <-ctx.Done():
						return
					}
				}
				for _, k := range order {
					select {
					case out <- buf[k].reply:
					case <-ctx.Done():
						return
					}
				}
				return
			}
			take(raw)
		case <-ctx.Done():
			return
		}
	}
}

// mergeCancelContexts returns a context cancelled when EITHER parent is
// cancelled. The returned stop func releases the AfterFunc watcher and
// cancels the merged context; both are idempotent, so it is safe to call
// even after b has already fired (in which case cancel is a no-op).
func mergeCancelContexts(a, b context.Context) (context.Context, func()) {
	merged, cancel := context.WithCancel(a)
	stopAfterFunc := context.AfterFunc(b, cancel)
	return merged, func() {
		stopAfterFunc()
		cancel()
	}
}

// runGetCancel waits for ctx cancellation, a timeout expiry, or the Get to
// terminate naturally (translatorExited closed). When ctx/timeout fires
// first, it calls CancelGet so the translator exits and the public reply
// channel closes.
func (s *Session) runGetCancel(ctx context.Context, timeout time.Duration, reqID uint32, translatorExited <-chan struct{}) {
	var timer *time.Timer
	var timerC <-chan time.Time
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		timerC = timer.C
	}
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	select {
	case <-ctx.Done():
		s.inner.CancelGet(reqID)
	case <-timerC:
		s.inner.CancelGet(reqID)
	case <-translatorExited:
		// Translator already exited; nothing to cancel.
	}
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
