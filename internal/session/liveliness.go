package session

import (
	"sync"

	"github.com/shirou/zenoh-go-client/internal/keyexpr"
)

// LivelinessEvent is one D_TOKEN / U_TOKEN delivery handed to a
// registered liveliness subscriber. IsAlive=true for D_TOKEN, false for
// U_TOKEN. Tokens have no payload so only the key is carried.
type LivelinessEvent struct {
	KeyExpr       string
	ParsedKeyExpr keyexpr.KeyExpr
	IsAlive       bool
}

// LivelinessDeliverFn is the callback a liveliness subscriber registers.
type LivelinessDeliverFn func(LivelinessEvent)

type livelinessSubEntry struct {
	id      uint32
	keyExpr keyexpr.KeyExpr
	deliver LivelinessDeliverFn
}

type livelinessSubs struct {
	mu   sync.RWMutex
	byID map[uint32]*livelinessSubEntry
}

func (s *Session) regLivelinessSubs() *livelinessSubs {
	s.livelinessSubsOnce.Do(func() {
		s.livelinessSubs = &livelinessSubs{byID: map[uint32]*livelinessSubEntry{}}
	})
	return s.livelinessSubs
}

// RegisterLivelinessSubscriber adds a liveliness subscriber. The caller
// has already emitted the INTEREST.
func (s *Session) RegisterLivelinessSubscriber(id uint32, ke keyexpr.KeyExpr, deliver LivelinessDeliverFn) {
	reg := s.regLivelinessSubs()
	reg.mu.Lock()
	reg.byID[id] = &livelinessSubEntry{id: id, keyExpr: ke, deliver: deliver}
	reg.mu.Unlock()
}

// UnregisterLivelinessSubscriber removes a liveliness subscriber.
func (s *Session) UnregisterLivelinessSubscriber(id uint32) {
	reg := s.regLivelinessSubs()
	reg.mu.Lock()
	delete(reg.byID, id)
	reg.mu.Unlock()
}

// ForEachLivelinessSubscriber lets the reconnect orchestrator replay
// INTEREST on a fresh link.
func (s *Session) ForEachLivelinessSubscriber(fn func(id uint32, ke keyexpr.KeyExpr)) {
	reg := s.regLivelinessSubs()
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	for id, sub := range reg.byID {
		fn(id, sub.keyExpr)
	}
}

// dispatchLiveliness fans a D_TOKEN / U_TOKEN out to every subscriber
// whose key expression intersects the token's key.
func (s *Session) dispatchLiveliness(tokenKE keyexpr.KeyExpr, ev LivelinessEvent) {
	reg := s.regLivelinessSubs()
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	for _, sub := range reg.byID {
		if sub.keyExpr.Intersects(tokenKE) {
			sub.deliver(ev)
		}
	}
}

// ---- Inbound token tracking ----

// inboundTokens maps router-assigned token entity_id → the full key the
// client resolved at D_TOKEN time. Needed because zenohd's U_TOKEN
// typically carries an empty WireExpr (the client is expected to
// remember which key the entity_id denoted).
//
// Known limitation: in some deployments zenohd forwards multiple foreign
// tokens to a single subscriber with the same (EntityID=0) — in which
// case a later D_TOKEN overwrites the earlier entry, and the
// corresponding U_TOKENs resolve to whichever key was most recently
// written. The live-interop tests exercise the single-token path that
// this handles correctly; multi-token-per-id fidelity is a Phase 7 item.
type inboundTokens struct {
	mu   sync.RWMutex
	byID map[uint32]string
}

func (s *Session) regInboundTokens() *inboundTokens {
	s.inboundTokensOnce.Do(func() {
		s.inboundTokens = &inboundTokens{byID: map[uint32]string{}}
	})
	return s.inboundTokens
}

// RememberInboundToken records the key for a router-forwarded D_TOKEN so
// a later U_TOKEN with only the entity_id can resolve back to it.
func (s *Session) RememberInboundToken(entityID uint32, fullKey string) {
	reg := s.regInboundTokens()
	reg.mu.Lock()
	reg.byID[entityID] = fullKey
	reg.mu.Unlock()
}

// ForgetInboundToken looks up and removes an entity_id; returns the
// stored key if one was tracked.
func (s *Session) ForgetInboundToken(entityID uint32) (string, bool) {
	reg := s.regInboundTokens()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	key, ok := reg.byID[entityID]
	if ok {
		delete(reg.byID, entityID)
	}
	return key, ok
}

// ResetInboundTokens drops every inbound-token mapping. Called on
// reconnect: the router's new session reassigns entity_ids from scratch,
// so stale entries would either leak memory or (on collision) resolve a
// new U_TOKEN to the wrong key.
func (s *Session) ResetInboundTokens() {
	reg := s.regInboundTokens()
	reg.mu.Lock()
	reg.byID = map[uint32]string{}
	reg.mu.Unlock()
}

// ---- Liveliness Get (INTEREST[Current, T=1]) registry ----

type livelinessQueryEntry struct {
	replies   chan LivelinessEvent
	closeOnce sync.Once
}

type livelinessQueries struct {
	mu   sync.RWMutex
	byID map[uint32]*livelinessQueryEntry
}

func (s *Session) regLivelinessQueries() *livelinessQueries {
	s.livelinessQueriesOnce.Do(func() {
		s.livelinessQueries = &livelinessQueries{byID: map[uint32]*livelinessQueryEntry{}}
	})
	return s.livelinessQueries
}

// RegisterLivelinessQuery starts tracking an in-flight liveliness Get
// keyed on the INTEREST's id. The returned channel yields every matching
// live-token, then closes on D_FINAL / explicit cancel.
func (s *Session) RegisterLivelinessQuery(interestID uint32, bufferSize int) <-chan LivelinessEvent {
	if bufferSize <= 0 {
		bufferSize = 16
	}
	e := &livelinessQueryEntry{replies: make(chan LivelinessEvent, bufferSize)}
	reg := s.regLivelinessQueries()
	reg.mu.Lock()
	reg.byID[interestID] = e
	reg.mu.Unlock()
	return e.replies
}

func (s *Session) deliverLivelinessReply(interestID uint32, ev LivelinessEvent) {
	reg := s.regLivelinessQueries()
	reg.mu.RLock()
	e, ok := reg.byID[interestID]
	reg.mu.RUnlock()
	if !ok {
		return
	}
	select {
	case e.replies <- ev:
	default:
		// Full — drop silently (no Budget mechanism for liveliness get today).
	}
}

// finaliseLivelinessQuery closes the query's channel and removes it.
// Called on D_FINAL and on CancelLivelinessQuery.
func (s *Session) finaliseLivelinessQuery(interestID uint32) {
	reg := s.regLivelinessQueries()
	reg.mu.Lock()
	e, ok := reg.byID[interestID]
	if ok {
		delete(reg.byID, interestID)
	}
	reg.mu.Unlock()
	if e != nil {
		e.closeOnce.Do(func() { close(e.replies) })
	}
}

// CancelLivelinessQuery finalises a liveliness Get from the public side.
func (s *Session) CancelLivelinessQuery(interestID uint32) {
	s.finaliseLivelinessQuery(interestID)
}

// cancelAllLivelinessQueries finalises every in-flight liveliness Get.
// Called from the runtime orchestrator on teardown, alongside
// cancelAllGets, so translator goroutines wake up and exit.
func (s *Session) cancelAllLivelinessQueries() {
	reg := s.regLivelinessQueries()
	reg.mu.Lock()
	ids := make([]uint32, 0, len(reg.byID))
	for id := range reg.byID {
		ids = append(ids, id)
	}
	reg.mu.Unlock()
	for _, id := range ids {
		s.finaliseLivelinessQuery(id)
	}
}
