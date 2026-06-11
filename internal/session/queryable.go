package session

import (
	"sync"
	"sync/atomic"

	"github.com/shirou/zenoh-go-client/internal/keyexpr"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// QueryReceived is the internal representation of an inbound query handed
// to a matching Queryable's callback. The zenoh package wraps it into a
// public Query.
type QueryReceived struct {
	RequestID     uint32
	KeyExpr       string
	ParsedKeyExpr keyexpr.KeyExpr
	Parameters    string
	Consolidation wire.ConsolidationMode

	// remaining counts deliveries of this request still outstanding across
	// every matching queryable. Shared by all copies handed out for one
	// REQUEST; see QueryDone.
	remaining *atomic.Int32
}

// QueryDone records that one delivered copy of the query has been fully
// handled and reports whether it was the last one. RESPONSE_FINAL must be
// sent exactly once per request — after the last matching queryable
// finishes — or the router closes the query early and discards the
// remaining queryables' replies (rust queryable.rs sends the final when the
// last Query clone drops). A QueryReceived that never went through
// dispatchQuery has no counter and counts as last.
func (q *QueryReceived) QueryDone() bool {
	if q.remaining == nil {
		return true
	}
	return q.remaining.Add(-1) == 0
}

// QueryDeliverFn is the callback a queryable registers to handle inbound
// queries.
type QueryDeliverFn func(QueryReceived)

type queryableEntry struct {
	id       uint32
	keyExpr  keyexpr.KeyExpr
	deliver  QueryDeliverFn
	complete bool // QueryableInfo.Complete flag for D_QUERYABLE replay on reconnect
}

type queryables struct {
	mu   sync.RWMutex
	byID map[uint32]*queryableEntry
}

func (s *Session) regQueryables() *queryables {
	s.qblsOnce.Do(func() {
		s.qbls = &queryables{byID: map[uint32]*queryableEntry{}}
	})
	return s.qbls
}

// RegisterQueryable stores a callback to be invoked for every inbound
// REQUEST that intersects ke. complete mirrors the QueryableInfo.Complete
// flag so the reconnect orchestrator can replay the exact D_QUERYABLE
// extension chain.
func (s *Session) RegisterQueryable(id uint32, ke keyexpr.KeyExpr, deliver QueryDeliverFn, complete bool) {
	reg := s.regQueryables()
	reg.mu.Lock()
	reg.byID[id] = &queryableEntry{id: id, keyExpr: ke, deliver: deliver, complete: complete}
	reg.mu.Unlock()
}

// ForEachQueryable invokes fn for every registered queryable. See
// ForEachSubscriber for lock-holding semantics.
func (s *Session) ForEachQueryable(fn func(id uint32, ke keyexpr.KeyExpr, complete bool)) {
	reg := s.regQueryables()
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	for id, qbl := range reg.byID {
		fn(id, qbl.keyExpr, qbl.complete)
	}
}

// UnregisterQueryable removes a queryable from the registry.
func (s *Session) UnregisterQueryable(id uint32) {
	reg := s.regQueryables()
	reg.mu.Lock()
	delete(reg.byID, id)
	reg.mu.Unlock()
}

// dispatchQuery routes an inbound REQUEST to every queryable whose key
// expression intersects, arming the shared QueryDone counter so the public
// layer can emit exactly one RESPONSE_FINAL after the last handler
// finishes. Returns the number of queryables the query was delivered to;
// the caller must finalise the request itself when that is zero, or the
// remote querier hangs until its timeout.
func (s *Session) dispatchQuery(queryKE keyexpr.KeyExpr, q QueryReceived) int {
	reg := s.regQueryables()
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	var matched []*queryableEntry
	for _, qbl := range reg.byID {
		if qbl.keyExpr.Intersects(queryKE) {
			matched = append(matched, qbl)
		}
	}
	if len(matched) == 0 {
		return 0
	}
	q.remaining = &atomic.Int32{}
	q.remaining.Store(int32(len(matched)))
	for _, qbl := range matched {
		qbl.deliver(q)
	}
	return len(matched)
}
