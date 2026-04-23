package zenoh

// LinkInfo is a snapshot of the currently-active link.
//
// The value is copied at the moment Session.LinkInfo is called; it does not
// track subsequent reconnects or CLOSE events. Session.LinkInfo returns nil
// when no link is active (session closed, or reconnect in progress).
type LinkInfo struct {
	// Scheme is the transport scheme (e.g. "tcp"). See internal/locator for
	// the full list of parseable schemes; "tcp" is the only one that can
	// actually carry traffic in the current release.
	Scheme string

	// LocalAddress is the local endpoint of the connection (e.g.
	// "192.0.2.1:54321"). Empty when the transport has no meaningful local
	// address.
	LocalAddress string

	// RemoteAddress is the peer address portion of the locator
	// (e.g. "192.0.2.9:7447").
	RemoteAddress string

	// RemoteLocator is the full locator string the link was dialed with
	// (e.g. "tcp/192.0.2.9:7447").
	RemoteLocator string

	// PeerZID is the remote node's ZenohID as exchanged during INIT/OPEN.
	PeerZID Id

	// PeerWhatAmI is the role advertised by the peer (Router / Peer / Client).
	PeerWhatAmI WhatAmI

	// NegotiatedBatchSize is the maximum framed batch size both sides agreed
	// on during INIT (lower of the two proposals).
	NegotiatedBatchSize uint16

	// NegotiatedResolution is the raw Resolution byte negotiated during INIT,
	// carrying the FrameSN and RequestID VLE byte-budget codes. Callers that
	// need the decoded field widths should go through an internal/wire helper
	// when that becomes necessary.
	NegotiatedResolution uint8

	// LocalLeaseMillis is the lease we announced; the peer must KEEPALIVE
	// within this window or we tear the link down.
	LocalLeaseMillis uint64

	// PeerLeaseMillis is the lease the peer announced; we must KEEPALIVE
	// within this window to keep the link up.
	PeerLeaseMillis uint64

	// QoSEnabled reports whether 16-lane QoS was negotiated. False means the
	// session falls back to a single Data-priority lane.
	QoSEnabled bool
}

// LinkInfo returns a snapshot of the currently-active link, or nil if the
// session has no link (closed, or currently reconnecting).
func (s *Session) LinkInfo() *LinkInfo {
	if s.closed.Load() {
		return nil
	}
	rt := s.snapshotRuntime()
	if rt == nil || rt.Result == nil {
		return nil
	}
	loc := rt.LinkRemoteLocator()
	return &LinkInfo{
		Scheme:               string(loc.Scheme),
		LocalAddress:         rt.LinkLocalAddress(),
		RemoteAddress:        loc.Address,
		RemoteLocator:        loc.String(),
		PeerZID:              IdFromWireID(rt.Result.PeerZID),
		PeerWhatAmI:          WhatAmI(rt.Result.PeerWhatAmI),
		NegotiatedBatchSize:  rt.Result.NegotiatedBatchSize,
		NegotiatedResolution: uint8(rt.Result.NegotiatedResolution),
		LocalLeaseMillis:     rt.Result.MyLeaseMillis,
		PeerLeaseMillis:      rt.Result.PeerLeaseMillis,
		QoSEnabled:           rt.Result.QoSEnabled,
	}
}
