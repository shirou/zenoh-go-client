package session

// EntityReplayTarget is the subset of a runtime that the entity-replay
// path needs: a way to enqueue a single OutboundItem. Both *Runtime and
// *MulticastRuntime implement it.
//
// The replay loop only needs to know whether each individual send made
// it onto the runtime's queue. A returned `false` means the runtime has
// torn down (unicast Link drop) or refused the item (multicast OutQ
// full); the loop records it as a per-entity error and keeps going.
//
// This is deliberately narrow. It does not expose LinkClosed channels
// or per-runtime hot-path details — those belong to the unicast Put /
// Get hot path through deliverEncoded. Replay does not need them
// because the unicast Runtime.Enqueue waits on its own LinkClosed
// internally.
type EntityReplayTarget interface {
	Enqueue(item OutboundItem) bool
}

// Enqueue blocks until the OutboundItem is accepted onto the runtime's
// outbound queue, or the link tears down. Returns true on accept,
// false on link drop. Used by entity-replay code paths so unicast and
// multicast runtimes share one replay engine.
//
// The hot Put / Get paths still use deliverEncoded directly because
// they need caller-context cancellation as well.
func (rt *Runtime) Enqueue(item OutboundItem) bool {
	select {
	case rt.OutQ <- item:
		return true
	case <-rt.LinkClosed:
		return false
	}
}
