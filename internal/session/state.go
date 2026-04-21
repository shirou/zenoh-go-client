// Package session implements the Zenoh session state machine, entity
// registry, ID allocators, and the inbound/outbound goroutine topology.
package session

import (
	"fmt"
	"sync/atomic"
)

// State is the lifecycle state of a Session. Transitions are protected via
// CompareAndSwap on the embedded atomic so concurrent callers cannot drive
// the state machine into an invalid configuration.
type State int32

const (
	StateInit State = iota
	StateDialing
	StateHandshaking
	StateActive
	StateReconnecting
	StateClosed
)

func (s State) String() string {
	switch s {
	case StateInit:
		return "Init"
	case StateDialing:
		return "Dialing"
	case StateHandshaking:
		return "Handshaking"
	case StateActive:
		return "Active"
	case StateReconnecting:
		return "Reconnecting"
	case StateClosed:
		return "Closed"
	default:
		return fmt.Sprintf("State(%d)", int32(s))
	}
}

// stateAtom wraps atomic.Int32 with State-typed Load/Store/CAS helpers.
type stateAtom struct{ v atomic.Int32 }

func (a *stateAtom) Load() State   { return State(a.v.Load()) }
func (a *stateAtom) Store(s State) { a.v.Store(int32(s)) }
func (a *stateAtom) CAS(old, new State) bool {
	return a.v.CompareAndSwap(int32(old), int32(new))
}
