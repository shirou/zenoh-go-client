package transport

import (
	"fmt"
	"sync"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// BatchSink writes one fully-framed batch to the wire. Typically the
// Writer goroutine calls link.WriteBatch.
type BatchSink func(batch []byte) error

// OutboundMessage is a serialised NetworkMessage with enough metadata for
// the batcher to route it to the right QoS lane.
type OutboundMessage struct {
	Encoded  []byte // complete NetworkMessage bytes, ready to go in a FRAME body
	Priority wire.QoSPriority
	Reliable bool
	Express  bool // bypass batching: flush current batch, emit this alone
}

// Batcher collects OutboundMessage into FRAME bodies, one per QoS lane,
// and emits them to the underlying BatchSink.
//
// All 16 lanes are tracked regardless of QoS negotiation. When QoS was not
// accepted, the writer loop routes everything to the Data lane instead of
// attaching a QoS extension — the batcher itself doesn't special-case that.
type Batcher struct {
	batchSize int
	attachQoS bool
	// laneSeeds[priority][reliable] is the starting sequence number for
	// the lane at first use. Unicast Batchers fill every entry with the
	// single negotiated initialSN; multicast Batchers carry the 16
	// independent values advertised in JOIN.
	laneSeeds [8][2]uint64

	mu    sync.Mutex
	lanes [8][2]*lane
	sink  BatchSink
}

// NewBatcher constructs a Batcher with a single starting sequence number
// shared by every lane (unicast convention).
func NewBatcher(batchSize int, attachQoS bool, initialSN uint64, sink BatchSink) *Batcher {
	b := &Batcher{
		batchSize: batchSize,
		attachQoS: attachQoS,
		sink:      sink,
	}
	for p := range b.laneSeeds {
		b.laneSeeds[p][0] = initialSN
		b.laneSeeds[p][1] = initialSN
	}
	return b
}

// NewBatcherWithLaneSeeds constructs a Batcher with per-lane starting
// SNs. seeds[priority][0] is the best-effort lane, seeds[priority][1]
// the reliable lane — matching the [8][2]*lane layout used internally.
// Used by the multicast transport, which advertises 16 independent
// initial SNs in its JOIN.
func NewBatcherWithLaneSeeds(batchSize int, attachQoS bool, seeds [8][2]uint64, sink BatchSink) *Batcher {
	return &Batcher{
		batchSize: batchSize,
		attachQoS: attachQoS,
		laneSeeds: seeds,
		sink:      sink,
	}
}

type lane struct {
	priority wire.QoSPriority
	reliable bool
	seqNum   uint64
	// batch holds the FRAME header + seq_num + extensions + body-so-far.
	// Non-empty iff a FRAME is currently being built.
	batch     []byte
	bodyStart int // offset where NetworkMessage bodies begin
}

// Enqueue routes a message to its QoS lane and either appends it to the
// current batch or flushes and starts a new one.
//
// Express messages flush the existing lane batch, then send the message
// alone in a fresh FRAME and flush again.
//
// Messages too large to fit in a single batch are split into FRAGMENT
// transport messages on the same lane (sharing its seq_num space). Any
// FRAME currently building on the lane is flushed first to preserve order.
func (b *Batcher) Enqueue(msg *OutboundMessage) error {
	if !msg.Priority.IsValid() {
		return fmt.Errorf("batcher: invalid priority %d", msg.Priority)
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	l := b.laneFor(msg.Priority, msg.Reliable)

	if len(msg.Encoded) > b.maxFrameBodySize() {
		if err := b.flushLocked(l); err != nil {
			return err
		}
		return b.fragmentLocked(l, msg.Encoded)
	}

	if msg.Express {
		// Flush anything queued on this lane, then emit the express message
		// as a standalone FRAME and flush again.
		if err := b.flushLocked(l); err != nil {
			return err
		}
		if err := b.appendToLane(l, msg.Encoded); err != nil {
			return err
		}
		return b.flushLocked(l)
	}

	// Normal path: if the message doesn't fit in the current batch, flush first.
	if len(l.batch) > 0 && len(l.batch)+len(msg.Encoded) > b.batchSize {
		if err := b.flushLocked(l); err != nil {
			return err
		}
	}
	return b.appendToLane(l, msg.Encoded)
}

// maxFrameBodySize is the largest single NetworkMessage we still try to
// pack into a FRAME. Anything strictly larger is fragmented instead.
// Subtracts the FRAME header + seq_num + optional QoS-ext overhead so a
// message at or below this size is guaranteed to fit in a fresh FRAME on
// its own; multi-message FRAMEs still go through the normal flush-on-
// overflow path inside Enqueue.
func (b *Batcher) maxFrameBodySize() int { return b.batchSize - fragmentOverhead }

// fragmentLocked splits msgBytes into FRAGMENT transport messages on the
// lane and writes each as its own stream batch via b.sink. The lane's
// seq_num is advanced by the number of fragments emitted so subsequent
// FRAMEs on the lane keep monotonic seq_nums. Must be called with b.mu held.
//
// Fragmenter.MaxBodySize takes the full batchSize because the fragmenter
// internally subtracts its own per-fragment overhead — passing
// maxFrameBodySize() here would deduct it twice.
func (b *Batcher) fragmentLocked(l *lane, msgBytes []byte) error {
	var exts []codec.Extension
	if b.attachQoS {
		exts = []codec.Extension{
			codec.NewZ64Ext(wire.ExtIDQoS, true, wire.QoS{Priority: l.priority}.EncodeZ64()),
		}
	}
	// Single buffer reused across every fragment of this Enqueue. The sink
	// consumes the bytes synchronously so we can reset between iterations.
	scratch := make([]byte, 0, b.batchSize)
	f := Fragmenter{MaxBodySize: b.batchSize}
	nextSN, err := f.Fragment(msgBytes, l.reliable, l.seqNum, exts, func(frag *wire.Fragment) error {
		w := codec.WriterFromSlice(scratch)
		if err := frag.EncodeTo(w); err != nil {
			return err
		}
		batch := w.Bytes()
		scratch = batch[:0]
		return b.sink(batch)
	})
	l.seqNum = nextSN
	return err
}

// FlushAll flushes every non-empty lane to the sink.
func (b *Batcher) FlushAll() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for p := range b.lanes {
		for r := range b.lanes[p] {
			l := b.lanes[p][r]
			if l == nil {
				continue
			}
			if err := b.flushLocked(l); err != nil {
				return err
			}
		}
	}
	return nil
}

// appendToLane opens a FRAME (header + seq_num + ext) if none is in flight,
// then appends the NetworkMessage bytes to the body section.
func (b *Batcher) appendToLane(l *lane, body []byte) error {
	if len(l.batch) == 0 {
		if err := b.openFrame(l); err != nil {
			return err
		}
	}
	l.batch = append(l.batch, body...)
	return nil
}

// openFrame writes a fresh FRAME header + seq_num + optional QoS ext into
// l.batch. After this, bodies can be appended directly.
//
// Uses l.batch as its own scratch buffer (reset to zero length first) to
// avoid allocating a fresh codec.Writer every flush.
func (b *Batcher) openFrame(l *lane) error {
	l.batch = l.batch[:0]
	w := codec.WriterFromSlice(l.batch)
	h := codec.Header{
		ID: wire.IDTransportFrame,
		F1: l.reliable,
		Z:  b.attachQoS,
	}
	w.EncodeHeader(h)
	w.EncodeZ64(l.seqNum)
	if b.attachQoS {
		ext := codec.NewZ64Ext(wire.ExtIDQoS, true, wire.QoS{Priority: l.priority}.EncodeZ64())
		if err := w.EncodeExtension(ext); err != nil {
			return err
		}
	}
	l.batch = w.Bytes()
	l.bodyStart = len(l.batch)
	return nil
}

// flushLocked writes l.batch to the sink if it has a non-empty body, then
// resets. Must be called with b.mu held.
func (b *Batcher) flushLocked(l *lane) error {
	if len(l.batch) == l.bodyStart {
		// No FRAME open, or opened with no body bytes appended.
		l.batch = l.batch[:0]
		return nil
	}
	err := b.sink(l.batch)
	l.seqNum++
	l.batch = l.batch[:0]
	return err
}

func (b *Batcher) laneFor(p wire.QoSPriority, reliable bool) *lane {
	pi := int(p) & 0x07
	ri := 0
	if reliable {
		ri = 1
	}
	if b.lanes[pi][ri] == nil {
		b.lanes[pi][ri] = &lane{
			priority: wire.QoSPriority(pi),
			reliable: reliable,
			seqNum:   b.laneSeeds[pi][ri],
			batch:    make([]byte, 0, b.batchSize),
		}
	}
	return b.lanes[pi][ri]
}
