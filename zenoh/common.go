package zenoh

import (
	"bytes"
	"encoding/hex"
	"fmt"

	"github.com/shirou/zenoh-go-client/internal/wire"
)

// Priority is the message priority class used by QoS transport lanes.
// Wire values match the zenoh-spec: 0=Control .. 5=Data (default) .. 7=Background.
type Priority uint8

const (
	PriorityControl         Priority = 0
	PriorityRealTime        Priority = 1
	PriorityInteractiveHigh Priority = 2
	PriorityInteractiveLow  Priority = 3
	PriorityDataHigh        Priority = 4
	PriorityData            Priority = 5
	PriorityDataLow         Priority = 6
	PriorityBackground      Priority = 7

	PriorityDefault = PriorityData
)

func (p Priority) String() string {
	switch p {
	case PriorityControl:
		return "Control"
	case PriorityRealTime:
		return "RealTime"
	case PriorityInteractiveHigh:
		return "InteractiveHigh"
	case PriorityInteractiveLow:
		return "InteractiveLow"
	case PriorityDataHigh:
		return "DataHigh"
	case PriorityData:
		return "Data"
	case PriorityDataLow:
		return "DataLow"
	case PriorityBackground:
		return "Background"
	default:
		return fmt.Sprintf("Priority(%d)", uint8(p))
	}
}

// IsValid reports whether the priority value fits in the 3-bit wire field (0..7).
func (p Priority) IsValid() bool { return p <= PriorityBackground }

// CongestionControl maps to the D bit of the QoS extension.
type CongestionControl uint8

const (
	CongestionControlDrop  CongestionControl = iota // D=0
	CongestionControlBlock                          // D=1

	CongestionControlDefault = CongestionControlDrop
)

func (c CongestionControl) String() string {
	switch c {
	case CongestionControlDrop:
		return "Drop"
	case CongestionControlBlock:
		return "Block"
	default:
		return fmt.Sprintf("CongestionControl(%d)", uint8(c))
	}
}

// WhatAmI is the node role. Wire values: 0b00=Router, 0b01=Peer, 0b10=Client.
// Only Client is fully supported in the initial implementation, but all three
// values are defined so types are ready for future work.
type WhatAmI uint8

const (
	WhatAmIRouter WhatAmI = 0b00
	WhatAmIPeer   WhatAmI = 0b01
	WhatAmIClient WhatAmI = 0b10
)

func (w WhatAmI) String() string {
	switch w {
	case WhatAmIRouter:
		return "Router"
	case WhatAmIPeer:
		return "Peer"
	case WhatAmIClient:
		return "Client"
	default:
		return fmt.Sprintf("WhatAmI(%d)", uint8(w))
	}
}

// IsValid reports whether the WhatAmI value is one of the three defined roles.
func (w WhatAmI) IsValid() bool { return w <= WhatAmIClient }

// Id is a ZenohID: between 1 and 16 bytes.
// The zero-value is an empty (invalid) ID.
type Id struct {
	bytes []byte // 1..16 bytes; stored in little-endian wire order
}

// NewIdFromBytes constructs an Id from a slice of 1..16 bytes.
// The input is copied.
func NewIdFromBytes(b []byte) (Id, error) {
	if len(b) == 0 || len(b) > 16 {
		return Id{}, fmt.Errorf("zenoh: Id length must be 1..16, got %d", len(b))
	}
	return Id{bytes: bytes.Clone(b)}, nil
}

// NewIdFromHex parses a lowercase hex string (no separators, no 0x prefix).
func NewIdFromHex(s string) (Id, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return Id{}, fmt.Errorf("zenoh: Id hex decode: %w", err)
	}
	return NewIdFromBytes(b)
}

// String returns the lowercase hex representation.
func (id Id) String() string { return hex.EncodeToString(id.bytes) }

// Bytes returns a copy of the Id bytes.
func (id Id) Bytes() []byte { return bytes.Clone(id.bytes) }

// Len returns the number of bytes in the Id (1..16). Returns 0 for the zero-value.
func (id Id) Len() int { return len(id.bytes) }

// IsZero reports whether the Id is the zero-value (invalid).
func (id Id) IsZero() bool { return len(id.bytes) == 0 }

// Equal reports whether two Ids have the same bytes.
func (id Id) Equal(other Id) bool { return bytes.Equal(id.bytes, other.bytes) }

// ToWireID converts the public Id to the internal wire representation.
// Returns the zero wire.ZenohID for an empty Id. The returned ZenohID owns
// its Bytes slice (copied from the source).
//
// This is an internal-leaning helper exposed so the zenoh package itself
// (which bridges between the public API and internal/wire) does not need to
// reach into the private byte field. User code should not need it.
func (id Id) ToWireID() wire.ZenohID {
	return wire.ZenohID{Bytes: bytes.Clone(id.bytes)}
}

// IdFromWireID builds a public Id from a wire ZenohID. A zero-length wire
// ZenohID maps to the zero Id.
func IdFromWireID(z wire.ZenohID) Id {
	return Id{bytes: bytes.Clone(z.Bytes)}
}

// TimeStamp is an HLC timestamp: NTP64 time + originating ZenohID.
type TimeStamp struct {
	ntp64 uint64
	zid   Id
}

// NewTimeStamp constructs a TimeStamp from its wire fields.
func NewTimeStamp(ntp64 uint64, zid Id) TimeStamp {
	return TimeStamp{ntp64: ntp64, zid: zid}
}

// Time returns the raw NTP64 time (upper 32 bits seconds since 1900, lower 32 fractional).
func (t TimeStamp) Time() uint64 { return t.ntp64 }

// Id returns the originating ZenohID.
func (t TimeStamp) Id() Id { return t.zid }
