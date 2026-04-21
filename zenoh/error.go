package zenoh

import "fmt"

// ZError is the error type returned by zenoh operations.
//
// Code uses these ranges:
//   - positive values are reserved for zenoh-go compatible application errors
//   - negative values are this package's sentinels (see the vars below)
//   - -100..-107 are [CLOSE reason codes] received from a peer
//     (see [CloseReasonError])
//   - -200 and below are internal decode / wire integrity errors
//
// Each sentinel has a distinct Code so [errors.Is] can discriminate them.
type ZError struct {
	Code int
	Msg  string
}

func (e ZError) Error() string {
	return fmt.Sprintf("zenoh error [%d]: %s", e.Code, e.Msg)
}

// Is enables errors.Is(err, ErrX) by comparing Code. Each sentinel uses a
// distinct Code value so that different sentinels do not alias each other.
func (e ZError) Is(target error) bool {
	t, ok := target.(ZError)
	if !ok {
		return false
	}
	return e.Code == t.Code
}

var (
	ErrNotImplemented     = ZError{Code: -1, Msg: "not implemented"}
	ErrAlreadyDropped     = ZError{Code: -2, Msg: "entity already dropped"}
	ErrConnectionLost     = ZError{Code: -3, Msg: "connection lost"}
	ErrPayloadTooLarge    = ZError{Code: -4, Msg: "payload too large"}
	ErrAllEndpointsFailed = ZError{Code: -5, Msg: "all endpoints failed"}
	ErrSessionClosed      = ZError{Code: -6, Msg: "session is closed"}
	ErrInvalidKeyExpr     = ZError{Code: -7, Msg: "invalid or zero-value KeyExpr"}
	ErrSessionNotReady    = ZError{Code: -10, Msg: "session not ready"}
)

// CLOSE reason codes from the Zenoh 1.0 spec.
const (
	CloseReasonGeneric          uint8 = 0x00
	CloseReasonUnsupported      uint8 = 0x01
	CloseReasonInvalid          uint8 = 0x02
	CloseReasonMaxSessions      uint8 = 0x03
	CloseReasonMaxLinks         uint8 = 0x04
	CloseReasonExpired          uint8 = 0x05
	CloseReasonUnresponsive     uint8 = 0x06
	CloseReasonConnectionToSelf uint8 = 0x07
)

// CloseReasonError converts a received CLOSE reason code into a ZError.
func CloseReasonError(reason uint8) ZError {
	msg := closeReasonText(reason)
	return ZError{Code: -100 - int(reason), Msg: "peer closed session: " + msg}
}

func closeReasonText(reason uint8) string {
	switch reason {
	case CloseReasonGeneric:
		return "generic"
	case CloseReasonUnsupported:
		return "unsupported version or extension"
	case CloseReasonInvalid:
		return "invalid protocol state"
	case CloseReasonMaxSessions:
		return "max sessions reached"
	case CloseReasonMaxLinks:
		return "max links reached"
	case CloseReasonExpired:
		return "session lease expired"
	case CloseReasonUnresponsive:
		return "peer unresponsive"
	case CloseReasonConnectionToSelf:
		return "connection to self"
	default:
		return fmt.Sprintf("unknown reason 0x%02x", reason)
	}
}
