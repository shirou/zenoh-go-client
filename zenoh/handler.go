package zenoh

import (
	"log/slog"
	"runtime/debug"
	"sync"
)

// Handler is the callback contract for subscriber (and, in later phases,
// queryable/get) receivers.
//
// Attach returns a deliver function that the session's inbound dispatcher
// calls for every sample, a drop function invoked when the receiver is
// undeclared, and a handle value exposed to the user (typically a
// receive-only channel; nil for pure callback handlers).
type Handler[T any] interface {
	Attach() (deliver func(T), drop func(), handle any)
}

// Closure wraps a user callback. Call is invoked for each delivered value
// in a dedicated dispatcher goroutine so a slow callback never stalls the
// session reader. Drop (optional) runs after the last Call.
type Closure[T any] struct {
	Call func(T)
	Drop func()
}

// Attach runs a dispatcher goroutine that pulls from an internal unbounded
// buffer and invokes c.Call. Panics in Call are recovered and logged.
func (c Closure[T]) Attach() (func(T), func(), any) {
	ch := make(chan T, 1024) // bounded intermediate queue; drops on overflow.
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for v := range ch {
			callSafe(c.Call, v)
		}
		if c.Drop != nil {
			callSafe(func(struct{}) { c.Drop() }, struct{}{})
		}
	}()

	deliver := func(v T) {
		// Non-blocking send: if the dispatcher is behind, drop rather
		// than stall the inbound reader (which would starve other subs).
		select {
		case ch <- v:
		default:
			slog.Default().Warn("zenoh: closure handler queue full, dropping sample")
		}
	}
	drop := func() {
		close(ch)
		<-done
	}
	return deliver, drop, nil
}

// FifoChannel is a bounded channel-backed handler. Callers read from the
// channel returned by Attach's handle. When the channel is full, further
// samples are dropped (MVP behaviour; Phase 6 will add block-on-full).
type FifoChannel[T any] struct {
	Capacity int
}

// NewFifoChannel returns a FifoChannel with the given capacity.
func NewFifoChannel[T any](capacity int) FifoChannel[T] { return FifoChannel[T]{Capacity: capacity} }

// Attach returns a deliver that does a non-blocking send into a bounded
// channel, and exposes the channel as handle (<-chan T).
func (f FifoChannel[T]) Attach() (func(T), func(), any) {
	cap := f.Capacity
	if cap <= 0 {
		cap = 256
	}
	ch := make(chan T, cap)
	deliver := func(v T) {
		select {
		case ch <- v:
		default:
			slog.Default().Warn("zenoh: fifo handler full, dropping sample")
		}
	}
	drop := func() { close(ch) }
	// Expose the channel as <-chan T so callers cannot close or send to it.
	var handle <-chan T = ch
	return deliver, drop, handle
}

// RingChannel is a ring-buffer-backed handler. When full, the oldest
// pending sample is dropped so the newest always makes it through.
type RingChannel[T any] struct {
	Capacity int
}

// NewRingChannel returns a RingChannel with the given capacity.
func NewRingChannel[T any](capacity int) RingChannel[T] { return RingChannel[T]{Capacity: capacity} }

// Attach implements the ring-buffer semantics. Callers receive from the
// channel returned as handle.
func (r RingChannel[T]) Attach() (func(T), func(), any) {
	cap := r.Capacity
	if cap <= 0 {
		cap = 256
	}
	ch := make(chan T, cap)
	var mu sync.Mutex

	deliver := func(v T) {
		mu.Lock()
		defer mu.Unlock()
		for {
			select {
			case ch <- v:
				return
			default:
				// Drop one from the head and retry.
				select {
				case <-ch:
				default:
					// Channel became empty between checks; retry send.
				}
			}
		}
	}
	drop := func() { close(ch) }
	var handle <-chan T = ch
	return deliver, drop, handle
}

func callSafe[T any](f func(T), v T) {
	defer func() {
		if r := recover(); r != nil {
			slog.Default().Error("zenoh: handler panicked",
				"panic", r, "stack", string(debug.Stack()))
		}
	}()
	if f != nil {
		f(v)
	}
}
