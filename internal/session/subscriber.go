package session

import (
	"sync"

	"github.com/shirou/zenoh-go-client/internal/keyexpr"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// PushSample is the internal representation of an inbound PUSH delivery,
// handed to each matching subscriber's DeliverFn. The zenoh package wraps
// it into the public Sample type before reaching user code.
//
// ParsedKeyExpr is the canonical KeyExpr the dispatcher parsed once; the
// zenoh layer reuses it to avoid re-parsing per delivery. KeyExpr is the
// raw suffix text.
//
// Payload aliases the reader buffer unless the subscriber callback copies.
type PushSample struct {
	KeyExpr       string
	ParsedKeyExpr keyexpr.KeyExpr
	Kind          byte // IDDataPut or IDDataDel
	Payload       []byte
	Encoding      *wire.Encoding
	Timestamp     *wire.Timestamp
}

// DeliverFn is what Session.RegisterSubscriber stores for inbound delivery.
type DeliverFn func(PushSample)

// subscriberEntry tracks one registered subscriber.
type subscriberEntry struct {
	id      uint32
	keyExpr keyexpr.KeyExpr
	deliver DeliverFn
}

// subscribers is the per-session subscriber registry. Access is serialised
// by mu because inbound dispatch, DeclareSubscriber, and Undeclare touch
// this map concurrently.
type subscribers struct {
	mu   sync.RWMutex
	byID map[uint32]*subscriberEntry
}

func (s *Session) regSubscribers() *subscribers {
	s.subsOnce.Do(func() {
		s.subs = &subscribers{byID: map[uint32]*subscriberEntry{}}
	})
	return s.subs
}

// RegisterSubscriber adds a subscriber to the session's registry. It does
// NOT send a D_SUBSCRIBER message — the public zenoh.DeclareSubscriber
// layer orchestrates the wire side before/after calling this.
func (s *Session) RegisterSubscriber(id uint32, ke keyexpr.KeyExpr, deliver DeliverFn) {
	reg := s.regSubscribers()
	reg.mu.Lock()
	reg.byID[id] = &subscriberEntry{id: id, keyExpr: ke, deliver: deliver}
	reg.mu.Unlock()
}

// UnregisterSubscriber removes a subscriber from the registry. Returns
// the previously stored entry (nil if none).
func (s *Session) UnregisterSubscriber(id uint32) {
	reg := s.regSubscribers()
	reg.mu.Lock()
	delete(reg.byID, id)
	reg.mu.Unlock()
}

// ForEachSubscriber invokes fn for every registered subscriber. fn MUST
// NOT call RegisterSubscriber / UnregisterSubscriber on this session — the
// registry RLock is held. Used by the reconnect orchestrator to replay
// D_SUBSCRIBER messages on a fresh Runtime.
func (s *Session) ForEachSubscriber(fn func(id uint32, ke keyexpr.KeyExpr)) {
	reg := s.regSubscribers()
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	for id, sub := range reg.byID {
		fn(id, sub.keyExpr)
	}
}

// dispatchPush routes an inbound PUSH to every subscriber whose key
// expression intersects the push's key. For MVP we walk the registry
// linearly; a trie is Phase 6+.
func (s *Session) dispatchPush(pushKE keyexpr.KeyExpr, sample PushSample) {
	reg := s.regSubscribers()
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	for _, sub := range reg.byID {
		if sub.keyExpr.Intersects(pushKE) {
			sub.deliver(sample)
		}
	}
}
