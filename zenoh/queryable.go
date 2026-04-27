package zenoh

import (
	"log/slog"
	"runtime/debug"
	"sync/atomic"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/session"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// Queryable is a registered handler that answers inbound queries whose
// key expression intersects a bound key. Always Drop when done to release
// the dispatcher goroutine and unregister from the router.
type Queryable struct {
	session *Session
	keyExpr KeyExpr
	id      uint32
	dropFn  func()
	dropped atomic.Bool
}

// QueryableOptions controls queryable declaration. Only Complete is
// implemented currently (via the QueryableInfo extension).
type QueryableOptions struct {
	Complete bool
}

// QueryHandler is what DeclareQueryable takes: a synchronous callback
// invoked for every inbound query. The framework automatically emits a
// RESPONSE_FINAL after the callback returns. The callback MUST finish all
// its Reply/ReplyErr calls before returning.
//
// A dedicated dispatcher goroutine calls this handler; user code may
// block without stalling the session reader.
type QueryHandler func(*Query)

// DeclareQueryable registers a queryable on keyExpr. Inbound REQUEST
// messages that intersect the key are dispatched to handler in a dedicated
// goroutine.
func (s *Session) DeclareQueryable(keyExpr KeyExpr, handler QueryHandler, opts *QueryableOptions) (*Queryable, error) {
	if s.closed.Load() {
		return nil, ErrSessionClosed
	}
	if keyExpr.IsZero() {
		return nil, ErrInvalidKeyExpr
	}
	if handler == nil {
		return nil, ErrInvalidKeyExpr // reuse; callers shouldn't pass nil
	}

	id := s.inner.IDs().AllocQblsID()

	// Dispatcher goroutine: deliver QueryReceived → handler, auto-final.
	inQ := make(chan session.QueryReceived, 32)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				slog.Default().Error("zenoh: queryable dispatcher panicked",
					"panic", r, "stack", string(debug.Stack()))
			}
		}()
		for qr := range inQ {
			s.runQueryHandler(handler, qr)
		}
	}()

	deliverFn := func(qr session.QueryReceived) {
		select {
		case inQ <- qr:
		default:
			slog.Default().Warn("zenoh: queryable dispatcher queue full, dropping query")
		}
	}
	complete := opts != nil && opts.Complete
	s.inner.RegisterQueryable(id, keyExpr.internalKeyExpr(), deliverFn, complete)

	if err := s.sendDeclareQueryable(id, keyExpr, opts); err != nil {
		s.inner.UnregisterQueryable(id)
		close(inQ)
		<-done
		return nil, err
	}

	return &Queryable{
		session: s,
		keyExpr: keyExpr,
		id:      id,
		dropFn: func() {
			close(inQ)
			<-done
		},
	}, nil
}

// KeyExpr returns the queryable's bound key expression.
func (q *Queryable) KeyExpr() KeyExpr { return q.keyExpr }

// Drop undeclares the queryable and stops its dispatcher goroutine.
func (q *Queryable) Drop() {
	if !q.dropped.CompareAndSwap(false, true) {
		return
	}
	q.session.inner.UnregisterQueryable(q.id)
	_ = q.session.sendUndeclareQueryable(q.id, q.keyExpr)
	if q.dropFn != nil {
		q.dropFn()
	}
}

// runQueryHandler invokes the user handler for one query, then emits
// RESPONSE_FINAL. Panics in the user handler, ReplyErr, or sendFinal are
// all recovered independently so a single failure can't prevent the
// RESPONSE_FINAL from being attempted and can't kill the dispatcher
// goroutine for subsequent queries.
func (s *Session) runQueryHandler(handler QueryHandler, qr session.QueryReceived) {
	query := buildQuery(s, qr)

	// User handler — panics surface as ERR reply.
	func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Default().Error("zenoh: query handler panicked",
					"panic", r, "stack", string(debug.Stack()))
				safeCall(func() {
					_ = query.ReplyErr(NewZBytesFromString("queryable handler panicked"), nil)
				})
			}
		}()
		handler(query)
	}()

	// RESPONSE_FINAL in its own recover boundary.
	safeCall(func() {
		if err := query.sendFinal(); err != nil {
			slog.Default().Warn("zenoh: RESPONSE_FINAL send failed", "err", err)
		}
	})
}

// safeCall runs f with a recover() boundary that logs any panic. Used on
// cleanup paths so a panic in one cleanup step doesn't skip the next.
func safeCall(f func()) {
	defer func() {
		if r := recover(); r != nil {
			slog.Default().Error("zenoh: cleanup step panicked",
				"panic", r, "stack", string(debug.Stack()))
		}
	}()
	f()
}

// sendDeclareQueryable builds the DECLARE[D_QUERYABLE] with the optional
// QueryableInfo extension (complete flag, distance=0 for local).
func (s *Session) sendDeclareQueryable(id uint32, keyExpr KeyExpr, opts *QueryableOptions) error {
	return s.enqueueDeclare(buildDeclareQueryable(id, keyExpr, opts))
}

// sendDeclareQueryableOn is the per-Runtime variant for replay.
func (s *Session) sendDeclareQueryableOn(rt *session.Runtime, id uint32, keyExpr KeyExpr, opts *QueryableOptions) error {
	return s.enqueueDeclareOn(rt, buildDeclareQueryable(id, keyExpr, opts))
}

func buildDeclareQueryable(id uint32, keyExpr KeyExpr, opts *QueryableOptions) *wire.Declare {
	body := wire.NewDeclareQueryable(id, keyExpr.toWire())
	if opts != nil && opts.Complete {
		body.Extensions = append(body.Extensions, queryableInfoExt(true, 0))
	}
	return &wire.Declare{Body: body}
}

// sendUndeclareQueryable emits U_QUERYABLE with the required WireExpr ext.
func (s *Session) sendUndeclareQueryable(id uint32, keyExpr KeyExpr) error {
	return s.enqueueDeclare(&wire.Declare{
		Body: &wire.UndeclareEntity{
			Kind:     wire.IDUndeclareQueryable,
			EntityID: id,
			WireExpr: keyExpr.toWire(),
		},
	})
}

// queryableInfoExt builds the QueryableInfo extension (Z64, id=0x01,
// M=false). Z64 payload layout: bit 0 = Complete; bits 17:1 = distance.
func queryableInfoExt(complete bool, distance uint16) codec.Extension {
	var v uint64 = uint64(distance) << 1
	if complete {
		v |= 1
	}
	return codec.NewZ64Ext(wire.QueryableInfoExtID, false, v)
}
