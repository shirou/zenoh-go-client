package transport

import (
	"bytes"
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// TestReassemblerInterleavedLanes feeds the reassembler fragments from
// multiple lanes in a randomised (but per-lane monotonic) order. Every
// lane's payload must be recovered byte-for-byte. Exercises the invariant
// that per-lane state is independent and cross-lane interleaving does not
// corrupt in-flight reassembly.
func TestReassemblerInterleavedLanes(t *testing.T) {
	const (
		lanes        = 8
		payloadSize  = 32 * 1024
		fragSize     = 1024
		startSeqNum  = uint64(1000)
		reasmCapMult = 2
	)

	type lanePlan struct {
		key   LaneKey
		msg   []byte
		frags [][]byte
	}
	plans := make([]*lanePlan, lanes)
	for i := range plans {
		msg := make([]byte, payloadSize)
		for j := range msg {
			msg[j] = byte(i*7 + j)
		}
		p := &lanePlan{
			key: LaneKey{Priority: uint8(i), Reliable: i%2 == 0},
			msg: msg,
		}
		for off := 0; off < len(msg); off += fragSize {
			end := min(off+fragSize, len(msg))
			p.frags = append(p.frags, msg[off:end])
		}
		plans[i] = p
	}

	// Build a global event order: random lane pick each step, always the
	// next unemitted fragment on that lane.
	type evt struct {
		plan *lanePlan
		idx  int
	}
	var events []evt
	rng := rand.New(rand.NewPCG(42, 99))
	indices := make([]int, lanes)
	remaining := make([]int, lanes)
	total := 0
	for i, p := range plans {
		remaining[i] = len(p.frags)
		total += len(p.frags)
	}
	for range total {
		for {
			li := rng.IntN(lanes)
			if remaining[li] == 0 {
				continue
			}
			events = append(events, evt{plans[li], indices[li]})
			indices[li]++
			remaining[li]--
			break
		}
	}

	r := NewReassembler(payloadSize * reasmCapMult)
	got := make(map[uint8][]byte)
	for _, e := range events {
		more := e.idx < len(e.plan.frags)-1
		sn := startSeqNum + uint64(e.idx)
		out, err := r.Push(e.plan.key, sn, more, e.plan.frags[e.idx])
		if err != nil {
			t.Fatalf("Push lane=%d idx=%d: %v", e.plan.key.Priority, e.idx, err)
		}
		if out != nil {
			got[e.plan.key.Priority] = out
		}
	}

	if len(got) != lanes {
		t.Fatalf("completed %d lanes, want %d", len(got), lanes)
	}
	for _, p := range plans {
		if !bytes.Equal(got[p.key.Priority], p.msg) {
			t.Errorf("lane %d payload mismatch (len got=%d want=%d)",
				p.key.Priority, len(got[p.key.Priority]), len(p.msg))
		}
	}
}

// TestBatcherConcurrentLargePayloads: N goroutines simultaneously enqueue
// oversize messages on distinct priorities. Each enqueue must fragment
// atomically so the per-lane fragment stream stays contiguous on the
// wire; reassembling the captured batches must yield each original
// payload.
//
// The batcher mutex serialises Enqueue calls, so this primarily guards
// against a regression that would split one Enqueue's fragment stream
// across another's — the contract readers rely on for lane seq_num
// monotonicity.
func TestBatcherConcurrentLargePayloads(t *testing.T) {
	const (
		lanes       = 4
		payloadSize = 16 * 1024
		batchSize   = 1024
	)

	priorities := []wire.QoSPriority{
		wire.QoSPriorityData,
		wire.QoSPriorityDataHigh,
		wire.QoSPriorityDataLow,
		wire.QoSPriorityInteractiveHigh,
	}

	payloads := make([][]byte, lanes)
	for i := range payloads {
		p := make([]byte, payloadSize)
		for j := range p {
			p[j] = byte(i*37 + j)
		}
		payloads[i] = p
	}

	var mu sync.Mutex
	var batches [][]byte
	b := NewBatcher(batchSize, true, 0, func(batch []byte) error {
		mu.Lock()
		defer mu.Unlock()
		batches = append(batches, bytes.Clone(batch))
		return nil
	})

	var wg sync.WaitGroup
	for i := range lanes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := b.Enqueue(&OutboundMessage{
				Encoded:  payloads[i],
				Priority: priorities[i],
				Reliable: true,
			}); err != nil {
				t.Errorf("Enqueue lane %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	r := NewReassembler(payloadSize * 2)
	completed := make(map[uint8][]byte)
	for _, batch := range batches {
		rd := codec.NewReader(batch)
		h, err := rd.DecodeHeader()
		if err != nil {
			t.Fatalf("decode batch header: %v", err)
		}
		if h.ID != wire.IDTransportFragment {
			t.Fatalf("expected FRAGMENT, got id=%#x", h.ID)
		}
		frag, err := wire.DecodeFragment(rd, h)
		if err != nil {
			t.Fatalf("decode FRAGMENT: %v", err)
		}
		lane := LaneKey{
			Priority: LanePriorityFromExts(frag.Extensions),
			Reliable: frag.Reliable,
		}
		out, err := r.Push(lane, frag.SeqNum, frag.More, frag.Body)
		if err != nil {
			t.Fatalf("reassemble priority=%d sn=%d: %v", lane.Priority, frag.SeqNum, err)
		}
		if out != nil {
			completed[lane.Priority] = out
		}
	}

	if len(completed) != lanes {
		t.Fatalf("completed %d lanes (%v), want %d",
			len(completed), completedKeys(completed), lanes)
	}
	for i, p := range priorities {
		if !bytes.Equal(completed[uint8(p)], payloads[i]) {
			t.Errorf("priority %d payload mismatch (len got=%d want=%d)",
				p, len(completed[uint8(p)]), len(payloads[i]))
		}
	}
}

func completedKeys(m map[uint8][]byte) []uint8 {
	ks := make([]uint8, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
