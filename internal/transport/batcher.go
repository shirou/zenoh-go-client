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
	batchSize  int
	attachQoS  bool
	initialSN  uint64

	mu    sync.Mutex
	lanes [8][2]*lane
	sink  BatchSink
}

// NewBatcher constructs a Batcher with the given parameters. initialSN is
// used for every lane as the starting sequence number.
func NewBatcher(batchSize int, attachQoS bool, initialSN uint64, sink BatchSink) *Batcher {
	return &Batcher{
		batchSize: batchSize,
		attachQoS: attachQoS,
		initialSN: initialSN,
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
func (b *Batcher) Enqueue(msg *OutboundMessage) error {
	if !msg.Priority.IsValid() {
		return fmt.Errorf("batcher: invalid priority %d", msg.Priority)
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	l := b.laneFor(msg.Priority, msg.Reliable)

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
			seqNum:   b.initialSN,
			batch:    make([]byte, 0, b.batchSize),
		}
	}
	return b.lanes[pi][ri]
}
