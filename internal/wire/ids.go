package wire

// Wire-level message IDs. These are the low 5 bits of the header byte
// (docs/internal-design.md §1, spec wire/message-format.adoc).
//
// Transport layer: IDs occupy 0x00..0x07.
// Network layer:  IDs occupy 0x19..0x1F.
// Declaration sub-messages (inside DECLARE): IDs 0x00..0x07 + 0x1A.
// Data sub-messages (inside PUSH / REQUEST / RESPONSE): IDs 0x01..0x05.

// Transport message IDs.
const (
	IDTransportOAM       byte = 0x00
	IDTransportInit      byte = 0x01
	IDTransportOpen      byte = 0x02
	IDTransportClose     byte = 0x03
	IDTransportKeepAlive byte = 0x04
	IDTransportFrame     byte = 0x05
	IDTransportFragment  byte = 0x06
	IDTransportJoin      byte = 0x07
)

// Network message IDs.
const (
	IDNetworkInterest      byte = 0x19
	IDNetworkResponseFinal byte = 0x1A
	IDNetworkResponse      byte = 0x1B
	IDNetworkRequest       byte = 0x1C
	IDNetworkPush          byte = 0x1D
	IDNetworkDeclare       byte = 0x1E
	IDNetworkOAM           byte = 0x1F
)

// Declaration sub-message IDs (inside DECLARE body).
const (
	IDDeclareKeyExpr        byte = 0x00
	IDUndeclareKeyExpr      byte = 0x01
	IDDeclareSubscriber     byte = 0x02
	IDUndeclareSubscriber   byte = 0x03
	IDDeclareQueryable      byte = 0x04
	IDUndeclareQueryable    byte = 0x05
	IDDeclareToken          byte = 0x06
	IDUndeclareToken        byte = 0x07
	IDDeclareFinal          byte = 0x1A
)

// Data sub-message IDs (inside PUSH / REQUEST / RESPONSE body).
const (
	IDDataPut   byte = 0x01
	IDDataDel   byte = 0x02
	IDDataQuery byte = 0x03
	IDDataReply byte = 0x04
	IDDataErr   byte = 0x05
)

// Scouting message IDs (scouting.adoc). These live in their own ID space
// because scouting runs outside the transport/network layers.
const (
	IDScoutScout byte = 0x01
	IDScoutHello byte = 0x02
)

// ProtoVersion is the current Zenoh wire protocol version byte.
// Spec: commons/zenoh-protocol/src/lib.rs:31 → 0x09.
const ProtoVersion byte = 0x09
