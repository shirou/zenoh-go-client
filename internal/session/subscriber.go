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
//
// SourceZID is populated when the PUSH was received over a multicast
// transport (so the originating peer's ZID is meaningful and recoverable
// from the datagram source). For unicast PUSHes it is the zero value —
// the unicast Runtime is already keyed by peer ZID, so there is no
// useful additional source information at this level. The field is not
// yet surfaced through the public Sample API; it exists as a hook for
// future per-peer matching-registry accounting and for tests.
type PushSample struct {
	KeyExpr       string
	ParsedKeyExpr keyexpr.KeyExpr
	Kind          byte // IDDataPut or IDDataDel
	Payload       []byte
	Encoding      *wire.Encoding
	Timestamp     *wire.Timestamp
	SourceZID     wire.ZenohID
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

// DispatchLocalPush is the public entry the zenoh-layer Put / Delete
// path calls so a session's own published samples reach its own
// matching subscribers without a wire round-trip. Mirrors zenoh-rust's
// default Locality::Any: the source session dispatches locally before
// sending to the wire, and zenohd's egress_filter prevents the wire
// echo from producing a second delivery.
//
// SourceZID identifies the publishing session as the source for any
// downstream per-peer accounting; callers pass their own ZID.
func (s *Session) DispatchLocalPush(pushKE keyexpr.KeyExpr, sample PushSample) {
	s.dispatchPush(pushKE, sample)
}

// dispatchPush routes an inbound PUSH to every subscriber whose key
// expression intersects the push's key. The registry is walked linearly;
// a trie can replace it once the subscriber count justifies the cost.
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
