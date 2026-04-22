package zenoh

import (
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Handler variants — Closure / FifoChannel / RingChannel.
//
// Tests call Attach() directly so the semantics are deterministic and do
// not depend on the session reader or network timing. Each test covers:
//   - back-pressure behaviour (drop newest / drop oldest / none)
//   - drop() terminates the handler cleanly (no goroutine leak, no panic)

// defaultHandlerCap mirrors the fallback capacity used by FifoChannel and
// RingChannel when Capacity is <= 0. Keep in sync with handler.go.
const defaultHandlerCap = 256

// TestClosureHandlerOrderAndDrop verifies the closure dispatcher preserves
// delivery order and runs Drop after the last value is processed.
func TestClosureHandlerOrderAndDrop(t *testing.T) {
	const n = 100
	var mu sync.Mutex
	got := make([]int, 0, n)
	dropped := make(chan struct{})

	h := Closure[int]{
		Call: func(v int) {
			mu.Lock()
			got = append(got, v)
			mu.Unlock()
		},
		Drop: func() { close(dropped) },
	}
	deliver, drop, handle := h.Attach()
	if handle != nil {
		t.Errorf("Closure.Attach handle = %v, want nil", handle)
	}

	for i := 0; i < n; i++ {
		deliver(i)
	}
	drop()
	select {
	case <-dropped:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Closure.Drop did not run")
	}

	mu.Lock()
	defer mu.Unlock()
	want := make([]int, n)
	for i := range want {
		want[i] = i
	}
	if !slices.Equal(got, want) {
		t.Errorf("order not preserved: got %v, want %v", got, want)
	}
}

// TestChannelHandlerBackpressure covers both bounded-channel handlers:
// FifoChannel keeps the first `cap` values (drops newest),
// RingChannel keeps the last `cap` values (drops oldest).
//
// The common shape — fill past capacity, close, drain — keeps the table
// small; each variant declares only what survives.
func TestChannelHandlerBackpressure(t *testing.T) {
	const size = 4

	cases := []struct {
		name     string
		handler  Handler[int]
		survives []int // surviving values in channel read order
	}{
		{
			name:     "fifo_drops_newest",
			handler:  NewFifoChannel[int](size),
			survives: []int{0, 1, 2, 3},
		},
		{
			name:     "ring_drops_oldest",
			handler:  NewRingChannel[int](size),
			survives: []int{size, size + 1, size + 2, size + 3},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			deliver, drop, handle := c.handler.Attach()
			ch, ok := handle.(<-chan int)
			if !ok {
				t.Fatalf("handle type = %T, want <-chan int", handle)
			}
			for i := 0; i < 2*size; i++ {
				deliver(i)
			}
			drop()

			got := make([]int, 0, size)
			for v := range ch {
				got = append(got, v)
			}
			if !slices.Equal(got, c.survives) {
				t.Errorf("got %v, want %v", got, c.survives)
			}
		})
	}
}

// TestClosureHandlerPanicRecovered asserts the dispatcher keeps going
// when Call panics on one value. drop() blocks until the dispatcher has
// drained, so by the time it returns we know every send was consumed.
func TestClosureHandlerPanicRecovered(t *testing.T) {
	var calls atomic.Int32
	h := Closure[int]{
		Call: func(v int) {
			calls.Add(1)
			if v == 1 {
				panic("boom")
			}
		},
	}
	deliver, drop, _ := h.Attach()
	deliver(0)
	deliver(1) // should panic internally, be recovered
	deliver(2)
	drop()
	if got := calls.Load(); got != 3 {
		t.Fatalf("calls = %d, want 3", got)
	}
}

// TestFifoChannelZeroCapacityDefaults: Capacity<=0 uses the defaultHandlerCap
// so a zero-valued handler is still usable.
func TestFifoChannelZeroCapacityDefaults(t *testing.T) {
	h := FifoChannel[int]{}
	deliver, drop, handle := h.Attach()
	ch := handle.(<-chan int)
	for i := 0; i < defaultHandlerCap; i++ {
		deliver(i)
	}
	drop()
	n := 0
	for range ch {
		n++
	}
	if n != defaultHandlerCap {
		t.Errorf("Fifo default cap buffered %d, want %d", n, defaultHandlerCap)
	}
}
