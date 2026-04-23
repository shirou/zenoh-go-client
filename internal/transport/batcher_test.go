package transport

import (
	"bytes"
	"testing"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

type capturedBatch struct {
	bytes []byte
}

func newCapturingBatcher(t *testing.T, batchSize int, attachQoS bool) (*Batcher, *[]capturedBatch) {
	t.Helper()
	var captured []capturedBatch
	b := NewBatcher(batchSize, attachQoS, 100, func(batch []byte) error {
		captured = append(captured, capturedBatch{bytes: bytes.Clone(batch)})
		return nil
	})
	return b, &captured
}

// TestBatcherGroupsMessagesPerLane: three messages on the same lane should
// be flushed in one FRAME.
func TestBatcherGroupsMessagesPerLane(t *testing.T) {
	b, out := newCapturingBatcher(t, 1024, true)

	for range 3 {
		if err := b.Enqueue(&OutboundMessage{
			Encoded:  []byte{0xAA, 0xBB, 0xCC},
			Priority: wire.QoSPriorityData,
			Reliable: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.FlushAll(); err != nil {
		t.Fatal(err)
	}

	if len(*out) != 1 {
		t.Fatalf("expected 1 flush, got %d", len(*out))
	}
	// FRAME header + seq_num + QoS ext + 3 × 3 bytes body.
	// We don't assert exact length; instead verify the header ID is FRAME
	// and seq_num decodes correctly.
	r := codec.NewReader((*out)[0].bytes)
	h, _ := r.DecodeHeader()
	if h.ID != wire.IDTransportFrame {
		t.Errorf("header id = %#x, want FRAME", h.ID)
	}
	if !h.F1 {
		t.Error("R flag should be set on reliable lane")
	}
	if !h.Z {
		t.Error("Z flag should be set when QoS ext attached")
	}
	sn, err := r.DecodeZ64()
	if err != nil {
		t.Fatal(err)
	}
	if sn != 100 {
		t.Errorf("seq_num = %d, want initial 100", sn)
	}
}

// TestBatcherSeqNumIncrements: flushing twice advances seq_num by 1.
func TestBatcherSeqNumIncrements(t *testing.T) {
	b, out := newCapturingBatcher(t, 256, true)

	body := []byte{0x01}
	_ = b.Enqueue(&OutboundMessage{Encoded: body, Priority: wire.QoSPriorityData, Reliable: true})
	_ = b.FlushAll()
	_ = b.Enqueue(&OutboundMessage{Encoded: body, Priority: wire.QoSPriorityData, Reliable: true})
	_ = b.FlushAll()

	if len(*out) != 2 {
		t.Fatalf("expected 2 flushes, got %d", len(*out))
	}

	sn1 := mustReadFrameSN(t, (*out)[0].bytes)
	sn2 := mustReadFrameSN(t, (*out)[1].bytes)
	if sn2 != sn1+1 {
		t.Errorf("seq_num: %d then %d, want increment", sn1, sn2)
	}
}

// TestBatcherLanesIndependent: two priorities on the same reliability
// produce two separate FRAMEs, each with its own seq_num.
func TestBatcherLanesIndependent(t *testing.T) {
	b, out := newCapturingBatcher(t, 1024, true)

	_ = b.Enqueue(&OutboundMessage{Encoded: []byte{1}, Priority: wire.QoSPriorityDataHigh, Reliable: true})
	_ = b.Enqueue(&OutboundMessage{Encoded: []byte{2}, Priority: wire.QoSPriorityDataLow, Reliable: true})
	_ = b.FlushAll()

	if len(*out) != 2 {
		t.Fatalf("expected 2 FRAMEs (one per lane), got %d", len(*out))
	}
	// Both lanes use the same initial SN (100) because each lane is
	// independent; they don't share a counter.
	sn0 := mustReadFrameSN(t, (*out)[0].bytes)
	sn1 := mustReadFrameSN(t, (*out)[1].bytes)
	if sn0 != 100 || sn1 != 100 {
		t.Errorf("per-lane initial SN: got %d %d, want both 100", sn0, sn1)
	}
}

// TestBatcherOverflowSplits: a message that doesn't fit in the open FRAME
// triggers a flush and starts a new batch.
//
// Sizes are tuned so each 40-byte message fits alone (≤ batchSize-fragmentOverhead
// so it doesn't trigger fragmentation) but two together overflow.
func TestBatcherOverflowSplits(t *testing.T) {
	b, out := newCapturingBatcher(t, 64, true)

	body := make([]byte, 40)
	for range 3 {
		if err := b.Enqueue(&OutboundMessage{Encoded: body, Priority: wire.QoSPriorityData, Reliable: true}); err != nil {
			t.Fatal(err)
		}
	}
	_ = b.FlushAll()

	if len(*out) != 3 {
		t.Errorf("expected 3 flushes, got %d", len(*out))
	}
}

// TestBatcherExpressImmediate: an express message flushes prior content
// and emits the express message in its own FRAME.
func TestBatcherExpressImmediate(t *testing.T) {
	b, out := newCapturingBatcher(t, 1024, true)

	_ = b.Enqueue(&OutboundMessage{Encoded: []byte{0x10}, Priority: wire.QoSPriorityData, Reliable: true})
	// Next message is express → two flushes: the pending one, then itself.
	_ = b.Enqueue(&OutboundMessage{Encoded: []byte{0x20}, Priority: wire.QoSPriorityData, Reliable: true, Express: true})

	if len(*out) != 2 {
		t.Fatalf("expected 2 flushes after express, got %d", len(*out))
	}
}

// TestBatcherNoQoSExtension: when AttachQoS=false, the FRAME header has Z=0
// and the batch lacks the QoS extension bytes.
func TestBatcherNoQoSExtension(t *testing.T) {
	b, out := newCapturingBatcher(t, 256, false)

	_ = b.Enqueue(&OutboundMessage{Encoded: []byte{0xFF}, Priority: wire.QoSPriorityData, Reliable: true})
	_ = b.FlushAll()

	if len(*out) != 1 {
		t.Fatalf("expected 1 flush, got %d", len(*out))
	}
	r := codec.NewReader((*out)[0].bytes)
	h, _ := r.DecodeHeader()
	if h.Z {
		t.Error("Z should be 0 when QoS ext disabled")
	}
}

func mustReadFrameSN(t *testing.T, batch []byte) uint64 {
	t.Helper()
	r := codec.NewReader(batch)
	if _, err := r.DecodeHeader(); err != nil {
		t.Fatal(err)
	}
	sn, err := r.DecodeZ64()
	if err != nil {
		t.Fatal(err)
	}
	return sn
}
