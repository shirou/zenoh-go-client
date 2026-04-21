package session

import "sync/atomic"

// idAllocators holds the six per-session entity ID allocators described in
// docs/internal-design.md §7. All are monotonically increasing 32-bit.
// expr_id reserves 0 for global scope (starts at 1); other spaces may start
// at 0 or 1 depending on convention (we use 1 uniformly for debug friendliness).
type idAllocators struct {
	nextExprID     atomic.Uint32
	nextSubsID     atomic.Uint32
	nextQblsID     atomic.Uint32
	nextTokenID    atomic.Uint32
	nextRequestID  atomic.Uint32
	nextInterestID atomic.Uint32
}

// AllocExprID returns the next D_KEYEXPR alias ID. 0 is reserved as the
// global scope, so values start at 1.
func (a *idAllocators) AllocExprID() uint32 { return a.nextExprID.Add(1) }

// AllocSubsID returns the next D_SUBSCRIBER entity ID (starts at 1).
func (a *idAllocators) AllocSubsID() uint32 { return a.nextSubsID.Add(1) }

// AllocQblsID returns the next D_QUERYABLE entity ID (starts at 1).
func (a *idAllocators) AllocQblsID() uint32 { return a.nextQblsID.Add(1) }

// AllocTokenID returns the next D_TOKEN entity ID (starts at 1).
func (a *idAllocators) AllocTokenID() uint32 { return a.nextTokenID.Add(1) }

// AllocRequestID returns the next REQUEST correlation ID (starts at 1).
func (a *idAllocators) AllocRequestID() uint32 { return a.nextRequestID.Add(1) }

// AllocInterestID returns the next INTEREST ID (starts at 1).
func (a *idAllocators) AllocInterestID() uint32 { return a.nextInterestID.Add(1) }
