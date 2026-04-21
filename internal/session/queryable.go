package session

import (
	"sync"

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
}

// QueryDeliverFn is the callback a queryable registers to handle inbound
// queries.
type QueryDeliverFn func(QueryReceived)

type queryableEntry struct {
	id      uint32
	keyExpr keyexpr.KeyExpr
	deliver QueryDeliverFn
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
// REQUEST that intersects ke.
func (s *Session) RegisterQueryable(id uint32, ke keyexpr.KeyExpr, deliver QueryDeliverFn) {
	reg := s.regQueryables()
	reg.mu.Lock()
	reg.byID[id] = &queryableEntry{id: id, keyExpr: ke, deliver: deliver}
	reg.mu.Unlock()
}

// UnregisterQueryable removes a queryable from the registry.
func (s *Session) UnregisterQueryable(id uint32) {
	reg := s.regQueryables()
	reg.mu.Lock()
	delete(reg.byID, id)
	reg.mu.Unlock()
}

// dispatchQuery routes an inbound REQUEST to every queryable whose key
// expression intersects.
func (s *Session) dispatchQuery(queryKE keyexpr.KeyExpr, q QueryReceived) {
	reg := s.regQueryables()
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	for _, qbl := range reg.byID {
		if qbl.keyExpr.Intersects(queryKE) {
			qbl.deliver(q)
		}
	}
}
