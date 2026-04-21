package wire

// QoS extension on network messages (PUSH / REQUEST / RESPONSE / DECLARE /
// INTEREST / RESPONSE_FINAL / OAM / FRAME).
//
// Wire (Z64 body, ext ID 0x1, M=0):
//
//	 7 6 5 4 3 2 1 0
//	+-+-+-+-+-+-+-+-+
//	|0|r|F|E|D|prio |   low byte of the z64 value
//	+-+-+-+-+-+-+-+-+
//
// prio: 3-bit priority (0..7); default 5 (Data)
// D:    don't-drop → Block congestion control
// E:    express → bypass batching
// F:    don't-drop-first (requires Patch ≥ 1)
// r:    reserved
//
// Higher bytes are reserved in the current spec; the encoder places the
// whole payload into the low byte and leaves upper bytes at zero.

// QoSPriority values (wire). Mirrors zenoh.Priority without creating a
// dependency on the public package.
type QoSPriority uint8

const (
	QoSPriorityControl         QoSPriority = 0
	QoSPriorityRealTime        QoSPriority = 1
	QoSPriorityInteractiveHigh QoSPriority = 2
	QoSPriorityInteractiveLow  QoSPriority = 3
	QoSPriorityDataHigh        QoSPriority = 4
	QoSPriorityData            QoSPriority = 5
	QoSPriorityDataLow         QoSPriority = 6
	QoSPriorityBackground      QoSPriority = 7
	QoSPriorityDefault                      = QoSPriorityData
)

// QoS describes the QoS extension body for network messages.
type QoS struct {
	Priority   QoSPriority
	DontDrop   bool // D: Block congestion control
	Express    bool // E: bypass batching
	DontDropFirst bool // F: BlockFirst (Patch ≥ 1)
}

// DefaultQoS returns the "no options" QoS used when a message has no
// explicit QoS extension attached.
func DefaultQoS() QoS { return QoS{Priority: QoSPriorityData} }

// IsValid reports whether the priority fits in the 3-bit wire field (0..7).
// Consistent with zenoh.Priority.IsValid.
func (p QoSPriority) IsValid() bool { return p <= QoSPriorityBackground }

// EncodeZ64 encodes the QoS payload into a z64 value (low byte, upper bytes 0).
func (q QoS) EncodeZ64() uint64 {
	b := uint64(q.Priority & 0b111)
	if q.DontDrop {
		b |= 1 << 3
	}
	if q.Express {
		b |= 1 << 4
	}
	if q.DontDropFirst {
		b |= 1 << 5
	}
	return b
}

// DecodeQoSZ64 extracts a QoS from the z64 body of a QoS extension.
func DecodeQoSZ64(v uint64) QoS {
	b := byte(v & 0xFF)
	return QoS{
		Priority:      QoSPriority(b & 0b111),
		DontDrop:      b&(1<<3) != 0,
		Express:       b&(1<<4) != 0,
		DontDropFirst: b&(1<<5) != 0,
	}
}
