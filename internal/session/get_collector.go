package session

import (
	"sync"

	"github.com/shirou/zenoh-go-client/internal/keyexpr"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// InboundReply is the internal representation of one reply delivered to a
// Get call. Exactly one of Put / Del / Err is non-nil.
//
// ParsedKeyExpr is the canonical-form KeyExpr the dispatcher parsed once;
// the zenoh layer reuses it to avoid re-parsing per reply. KeyExpr is the
// raw suffix text for logs/debug.
type InboundReply struct {
	KeyExpr       string
	ParsedKeyExpr keyexpr.KeyExpr
	Put           *wire.PutBody
	Del           *wire.DelBody
	Err           *wire.ErrBody
}

// IsErr reports whether this is an ERR reply rather than a data reply.
func (r InboundReply) IsErr() bool { return r.Err != nil }

// getCollector tracks an in-flight Get: the channel where replies are
// pushed and a signal for final termination.
type getCollector struct {
	replies chan InboundReply
	// closeOnce guards close(replies). RESPONSE_FINAL and Session.Close can
	// both trigger it.
	closeOnce sync.Once
}

type gets struct {
	mu    sync.RWMutex
	byReq map[uint32]*getCollector
}

func (s *Session) regGets() *gets {
	s.getsOnce.Do(func() {
		s.gets = &gets{byReq: map[uint32]*getCollector{}}
	})
	return s.gets
}

// RegisterGet starts tracking a new in-flight Get. The caller ranges over
// the returned channel, which closes after RESPONSE_FINAL or
// CancelGet(requestID) is called.
func (s *Session) RegisterGet(requestID uint32, bufferSize int) <-chan InboundReply {
	if bufferSize <= 0 {
		bufferSize = 16
	}
	c := &getCollector{replies: make(chan InboundReply, bufferSize)}
	reg := s.regGets()
	reg.mu.Lock()
	reg.byReq[requestID] = c
	reg.mu.Unlock()
	return c.replies
}

// deliverReply pushes one reply onto the collector's channel. Silently drops
// if the channel is full or the Get has been finalised.
//
// The read lock is held across the send so finaliseGet (which holds the
// write lock to close the channel) can never run concurrently with this
// send. Without that, the non-blocking send would race with close(replies)
// and could panic with "send on closed channel" — racy on every cancel /
// timeout path. The send itself is non-blocking (select default), so the
// read lock is held only briefly.
func (s *Session) deliverReply(requestID uint32, reply InboundReply) {
	reg := s.regGets()
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	c, ok := reg.byReq[requestID]
	if !ok {
		return
	}
	select {
	case c.replies <- reply:
	default:
		// buffer full; drop. Budget extension will eventually stop the
		// source anyway.
	}
}

// finaliseGet closes the collector's channel and removes it from the map.
// Called on RESPONSE_FINAL and on CancelGet.
//
// close(replies) runs while the write lock is held, mutually exclusive
// with any in-flight deliverReply (which holds the read lock across its
// send). closeOnce additionally guards against double-close from racing
// finaliseGet callers (e.g. RESPONSE_FINAL + concurrent CancelGet) that
// both delete from the map but only one wins the entry lookup.
func (s *Session) finaliseGet(requestID uint32) {
	reg := s.regGets()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	c, ok := reg.byReq[requestID]
	if !ok {
		return
	}
	delete(reg.byReq, requestID)
	c.closeOnce.Do(func() { close(c.replies) })
}

// CancelGet finalises a Get from the public-API side (e.g. context cancel).
func (s *Session) CancelGet(requestID uint32) { s.finaliseGet(requestID) }

// cancelAllGets finalises every outstanding in-flight Get. Called during
// session teardown so translator goroutines on the zenoh side stop
// blocking on never-to-be-closed collector channels.
func (s *Session) cancelAllGets() {
	reg := s.regGets()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	for id, c := range reg.byReq {
		delete(reg.byReq, id)
		c.closeOnce.Do(func() { close(c.replies) })
	}
}
