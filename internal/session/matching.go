package session

import (
	"sync"

	"github.com/shirou/zenoh-go-client/internal/keyexpr"
)

// MatchingKind distinguishes which kind of remote declaration a matching
// listener observes.
type MatchingKind uint8

const (
	// MatchingSubscribers counts remote D_SUBSCRIBER / U_SUBSCRIBER whose
	// key expression intersects a Publisher's key. Used for
	// Publisher.MatchingListener.
	MatchingSubscribers MatchingKind = iota

	// MatchingQueryables counts remote D_QUERYABLE / U_QUERYABLE whose key
	// expression intersects a Querier's key. Used for
	// Querier.MatchingListener.
	MatchingQueryables
)

// MatchingStatus is the per-delivery payload fed to listener callbacks.
// Matching is true when at least one matching remote entity exists.
type MatchingStatus struct {
	Matching bool
}

// MatchingDeliverFn is the listener callback.
//
// Contract: MUST NOT block and MUST NOT call back into Session.
// The registry invokes deliver under its own mutex so downstream work
// has to be handed off to a channel or goroutine (the zenoh-layer
// handlers Closure/Fifo/Ring all perform non-blocking sends).
type MatchingDeliverFn func(MatchingStatus)

// MatchingListenerID identifies one attached listener within a matching
// entry; used to support listener removal without tearing down the
// underlying INTEREST.
type MatchingListenerID uint32

// matchingEntry is the per-INTEREST tracking state. One entry per
// outstanding Publisher / Querier INTEREST.
type matchingEntry struct {
	keyExpr keyexpr.KeyExpr
	kind    MatchingKind

	// initialDone flips to true when DeclareFinal for this interest_id is
	// observed. Until then we update matchCount but do not deliver, so the
	// first notification reflects the post-snapshot count.
	initialDone bool

	// matchCount is the live count of matching remote entities (>=0).
	// Transitions 0→1 and 1→0 trigger delivery.
	matchCount int

	// nextListenerID is the allocator for attached listeners. Never reused.
	nextListenerID MatchingListenerID
	listeners      map[MatchingListenerID]matchingListener
}

// matchingListener pairs a delivery callback with an optional teardown
// hook. onEntryRemoved runs when UnregisterMatching drops the parent
// entry, giving the zenoh-layer listener a chance to close its handler
// channel so callers ranging over it don't block forever.
type matchingListener struct {
	deliver        MatchingDeliverFn
	onEntryRemoved func()
}

// matchingRegistry is the session-wide matching state. Keyed by
// interest_id. A single mutex serialises inbound declare events and
// listener attach/detach so deliveries observe a consistent snapshot.
type matchingRegistry struct {
	mu   sync.Mutex
	byID map[uint32]*matchingEntry
}

func (s *Session) regMatching() *matchingRegistry {
	s.matchingOnce.Do(func() {
		s.matching = &matchingRegistry{byID: map[uint32]*matchingEntry{}}
	})
	return s.matching
}

// RegisterMatching creates a tracking entry for an INTEREST that the
// caller has already (or will shortly) send. The entry starts empty
// (matchCount=0, initialDone=false) so the first delivery — at
// DeclareFinal time — reflects the snapshot the router returns.
func (s *Session) RegisterMatching(interestID uint32, ke keyexpr.KeyExpr, kind MatchingKind) {
	reg := s.regMatching()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.byID[interestID] = &matchingEntry{
		keyExpr:   ke,
		kind:      kind,
		listeners: map[MatchingListenerID]matchingListener{},
	}
}

// UnregisterMatching drops the entry for interestID and invokes every
// attached listener's onEntryRemoved hook so the zenoh layer can tear
// down any handler-side resources (channels, dispatcher goroutines).
func (s *Session) UnregisterMatching(interestID uint32) {
	reg := s.regMatching()
	reg.mu.Lock()
	entry, ok := reg.byID[interestID]
	if ok {
		delete(reg.byID, interestID)
	}
	reg.mu.Unlock()
	if !ok {
		return
	}
	// Invoke cleanup hooks outside the lock — they may call back into the
	// session (e.g. DetachMatchingListener during idempotent listener Drop).
	for _, l := range entry.listeners {
		if l.onEntryRemoved != nil {
			l.onEntryRemoved()
		}
	}
}

// AttachMatchingListener adds a listener to the entry keyed by interestID.
// Returns a per-entry handle (for later Detach) and whether attachment
// succeeded (false if interestID is unknown).
//
// If the snapshot has already completed, deliver is invoked synchronously
// with the current state before the method returns so callers never miss
// the initial state. The zenoh-layer Closure installs a dedicated
// dispatcher goroutine so that sync call doesn't block the caller beyond
// the queue send.
//
// onEntryRemoved, if non-nil, runs when UnregisterMatching drops the
// parent entry. Use it to close handler channels so consumers don't
// block on a dead subscription.
func (s *Session) AttachMatchingListener(interestID uint32, deliver MatchingDeliverFn, onEntryRemoved func()) (MatchingListenerID, bool) {
	reg := s.regMatching()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	entry, ok := reg.byID[interestID]
	if !ok {
		return 0, false
	}
	entry.nextListenerID++
	id := entry.nextListenerID
	entry.listeners[id] = matchingListener{deliver: deliver, onEntryRemoved: onEntryRemoved}
	if entry.initialDone {
		deliver(MatchingStatus{Matching: entry.matchCount > 0})
	}
	return id, true
}

// DetachMatchingListener removes one listener without affecting the
// underlying INTEREST. Safe to call on an already-torn-down entry.
func (s *Session) DetachMatchingListener(interestID uint32, listenerID MatchingListenerID) {
	reg := s.regMatching()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	entry, ok := reg.byID[interestID]
	if !ok {
		return
	}
	delete(entry.listeners, listenerID)
}

// GetMatchingStatus returns the current snapshot for interestID.
//
// Before the initial snapshot has completed, or when no matching entity
// has yet been observed, the raw Matching=false flag is reported rather
// than an explicit "not ready" signal — matching zenoh-go's C binding.
func (s *Session) GetMatchingStatus(interestID uint32) (MatchingStatus, bool) {
	reg := s.regMatching()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	entry, ok := reg.byID[interestID]
	if !ok {
		return MatchingStatus{}, false
	}
	return MatchingStatus{Matching: entry.matchCount > 0}, true
}

// onMatchingDeclare is called by the dispatch layer for every incoming
// D_SUBSCRIBER / D_QUERYABLE. It bumps matchCount on every matching
// entry of the given kind and delivers on 0→1 (only after initial
// snapshot).
func (s *Session) onMatchingDeclare(kind MatchingKind, remoteKE keyexpr.KeyExpr) {
	s.applyMatchingDelta(kind, remoteKE, +1)
}

// onMatchingUndeclare is called for every incoming U_SUBSCRIBER / U_QUERYABLE.
func (s *Session) onMatchingUndeclare(kind MatchingKind, remoteKE keyexpr.KeyExpr) {
	s.applyMatchingDelta(kind, remoteKE, -1)
}

func (s *Session) applyMatchingDelta(kind MatchingKind, remoteKE keyexpr.KeyExpr, delta int) {
	reg := s.regMatching()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	for _, entry := range reg.byID {
		if entry.kind != kind {
			continue
		}
		if !entry.keyExpr.Intersects(remoteKE) {
			continue
		}
		before := entry.matchCount
		entry.matchCount += delta
		if entry.matchCount < 0 {
			// Protect against double-undeclare from a misbehaving router.
			entry.matchCount = 0
		}
		if !entry.initialDone {
			continue
		}
		transitioned := (before == 0 && entry.matchCount > 0) ||
			(before > 0 && entry.matchCount == 0)
		if !transitioned {
			continue
		}
		status := MatchingStatus{Matching: entry.matchCount > 0}
		for _, l := range entry.listeners {
			l.deliver(status)
		}
	}
}

// onMatchingSnapshotDone marks the initial snapshot complete for a given
// INTEREST and emits the first delivery to every attached listener.
// Returns true if the id corresponded to a matching entry (so the caller
// knows whether to treat the DeclareFinal as matching-related).
func (s *Session) onMatchingSnapshotDone(interestID uint32) bool {
	reg := s.regMatching()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	entry, ok := reg.byID[interestID]
	if !ok {
		return false
	}
	if entry.initialDone {
		// Idempotent: router re-delivering DeclareFinal after a transient
		// state update shouldn't double-notify.
		return true
	}
	entry.initialDone = true
	status := MatchingStatus{Matching: entry.matchCount > 0}
	for _, l := range entry.listeners {
		l.deliver(status)
	}
	return true
}

// ResetMatching clears all counts and snapshot flags. Called on reconnect
// before INTEREST replay so the new session's snapshot starts from a
// clean count. Listeners are preserved and will be re-notified once the
// fresh DeclareFinal arrives.
func (s *Session) ResetMatching() {
	reg := s.regMatching()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	for _, entry := range reg.byID {
		entry.matchCount = 0
		entry.initialDone = false
	}
}
