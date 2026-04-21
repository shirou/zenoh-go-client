package zenoh

// SampleKind distinguishes a Put (new value) from a Delete (removal).
type SampleKind uint8

const (
	SampleKindPut SampleKind = iota
	SampleKindDelete
)

func (k SampleKind) String() string {
	switch k {
	case SampleKindPut:
		return "Put"
	case SampleKindDelete:
		return "Delete"
	default:
		return "SampleKind(?)"
	}
}

// Sample is what a subscriber receives on the inbound stream. It carries
// the key expression the publisher used, the payload, and minimal metadata.
//
// Optional fields (timestamp, encoding) are exposed via accessors that
// report whether they were present on the wire.
type Sample struct {
	keyExpr  KeyExpr
	payload  ZBytes
	kind     SampleKind
	encoding Encoding
	hasEnc   bool
	ts       TimeStamp
	hasTS    bool
}

// KeyExpr returns the key expression that addressed this sample.
func (s Sample) KeyExpr() KeyExpr { return s.keyExpr }

// Payload returns the sample payload.
func (s Sample) Payload() ZBytes { return s.payload }

// Kind returns whether this sample is a Put or a Delete.
func (s Sample) Kind() SampleKind { return s.kind }

// Encoding returns the payload encoding and a bool indicating presence.
func (s Sample) Encoding() (Encoding, bool) { return s.encoding, s.hasEnc }

// TimeStamp returns the sample timestamp and a bool indicating presence.
func (s Sample) TimeStamp() (TimeStamp, bool) { return s.ts, s.hasTS }
