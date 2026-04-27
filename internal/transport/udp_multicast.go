package transport

import (
	"errors"
	"fmt"
	"net"

	"github.com/shirou/zenoh-go-client/internal/locator"
	"github.com/shirou/zenoh-go-client/internal/multicast"
)

// MulticastBatchSize is the default batch size for multicast (datagram)
// links. Spec join.adoc §S==0 stipulates 8192; raising this risks IP
// fragmentation, so 8192 is also the upper-bound default we ship.
const MulticastBatchSize = 8192

// UDPMulticastLink is the datagram-based Link used by the multicast
// peer transport. Unlike a stream Link, the underlying socket is shared
// across multiple remote peers — sends fan out to the multicast group,
// reads return one datagram regardless of source.
//
// Source-address demux happens at the Session layer; the JOIN message
// body carries the originating peer's ZenohID so the receiver can map
// "datagram from src 192.168.1.7:54321" to "peer ZID 0xab12...". The
// concrete type therefore exposes ReadBatchFrom in addition to the
// Link-interface ReadBatch.
type UDPMulticastLink struct {
	listenConn *net.UDPConn
	sendConn   *net.UDPConn
	group      *net.UDPAddr
	loc        locator.Locator
	batchSize  int
}

// MulticastDialOpts configures DialMulticastUDP.
type MulticastDialOpts struct {
	// Interface is the outgoing interface for multicast sends and the
	// interface used for the listen socket's group join. nil = OS default.
	Interface *net.Interface
	// TTL is the IP multicast hop limit. 0 = OS default.
	TTL int
	// LoopbackDisabled, when true, sets IP_MULTICAST_LOOP=0 so the host
	// does not receive its own emitted JOIN/FRAME on the listen socket.
	LoopbackDisabled bool
	// BatchSize overrides the default datagram-batch ceiling. 0 means
	// MulticastBatchSize.
	BatchSize int
}

// DialMulticastUDP opens a UDPMulticastLink rooted at the given
// "udp/<group>:port" locator. The link binds a separate send socket
// (random port) and a listen socket joined to the group, so JOINs we
// emit and JOINs the group sends us flow on independent file
// descriptors. Closing the link closes both.
func DialMulticastUDP(loc locator.Locator, opts MulticastDialOpts) (*UDPMulticastLink, error) {
	if loc.Scheme != locator.SchemeUDP {
		return nil, fmt.Errorf("udp_multicast: expected udp scheme, got %q", loc.Scheme)
	}
	plan := multicast.Plan{
		Group:            loc.Address,
		Interface:        opts.Interface,
		TTL:              opts.TTL,
		EnableSend:       true,
		EnableListen:     true,
		LoopbackDisabled: opts.LoopbackDisabled,
	}
	sockets, err := multicast.Open(plan)
	if err != nil {
		return nil, fmt.Errorf("udp_multicast: open sockets: %w", err)
	}
	// Pick the IPv4 pair when present; IPv6-only configurations fall back
	// to the v6 sockets. We don't dual-stack a single Link — the caller
	// names the family in the locator.
	send := sockets.SendV4
	listen := sockets.ListenV4
	group := sockets.McastV4
	if send == nil {
		send, listen, group = sockets.SendV6, sockets.ListenV6, sockets.McastV6
	}
	if send == nil || listen == nil || group == nil {
		sockets.CloseAll()
		return nil, errors.New("udp_multicast: failed to open both send and listen sockets")
	}
	// Detach the unused other-family sockets (none expected) by closing
	// them; the helpers we keep are owned by the link from now on.
	for _, c := range []*net.UDPConn{sockets.SendV4, sockets.SendV6, sockets.ListenV4, sockets.ListenV6} {
		if c != nil && c != send && c != listen {
			_ = c.Close()
		}
	}

	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = MulticastBatchSize
	}
	return &UDPMulticastLink{
		listenConn: listen,
		sendConn:   send,
		group:      group,
		loc:        loc,
		batchSize:  batchSize,
	}, nil
}

// ReadBatch reads one datagram into buf and returns its length. When
// the datagram is larger than buf it is truncated (matches net.UDPConn
// semantics). Source address is discarded; use ReadBatchFrom when the
// caller needs to identify the sending peer.
func (l *UDPMulticastLink) ReadBatch(buf []byte) (int, error) {
	n, _, err := l.listenConn.ReadFromUDP(buf)
	return n, err
}

// ReadBatchFrom reads one datagram and returns the length plus the
// source UDP address. Used by the multicast Session reader to demux
// per-peer.
func (l *UDPMulticastLink) ReadBatchFrom(buf []byte) (int, *net.UDPAddr, error) {
	return l.listenConn.ReadFromUDP(buf)
}

// WriteBatch sends one datagram to the multicast group.
func (l *UDPMulticastLink) WriteBatch(batch []byte) error {
	_, err := l.sendConn.WriteToUDP(batch, l.group)
	return err
}

// Close closes both send and listen sockets. Idempotent.
func (l *UDPMulticastLink) Close() error {
	errSend := l.sendConn.Close()
	errListen := l.listenConn.Close()
	if errSend != nil {
		return errSend
	}
	return errListen
}

// RemoteLocator returns the multicast group locator. Multicast links
// are inherently many-to-many — there is no single "remote".
func (l *UDPMulticastLink) RemoteLocator() locator.Locator { return l.loc }

// LocalAddress is empty for datagram links per the Link interface contract.
func (l *UDPMulticastLink) LocalAddress() string { return "" }

// BatchSize returns the configured datagram-batch ceiling. The
// session-level batcher uses this as the FRAME body cap.
func (l *UDPMulticastLink) BatchSize() int { return l.batchSize }
