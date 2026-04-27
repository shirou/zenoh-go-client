// Package multicast provides shared UDP multicast socket plumbing used
// by both internal/scout (SCOUT/HELLO) and internal/transport's UDP
// multicast Link (JOIN/FRAME). The two callers need the same IPv4 / IPv6
// resolution, group join, TTL, interface, and loopback control logic;
// keeping it in one place avoids drift.
package multicast

import (
	"fmt"
	"net"
	"time"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// Plan describes which UDP sockets to open for a multicast operation.
//
// The same Plan can request: a send socket targeting the multicast group
// (EnableSend), a listen socket joined to the group (EnableListen),
// additional unicast send targets (Unicast), or a combination. Open
// returns Sockets with the relevant fields populated and zeros the rest.
type Plan struct {
	// Group is the multicast group as "host:port" (e.g. "224.0.0.224:7446"
	// or "[ff02::224]:7446"). Empty means no multicast — only Unicast
	// targets will be opened.
	Group string

	// Interface, when non-nil, is the outgoing interface for multicast
	// sends and the interface used for the listen socket's group join.
	Interface *net.Interface

	// TTL is the IP multicast hop limit. 0 = OS default (1 on most
	// systems — link-local only).
	TTL int

	// EnableSend, when true, adds the multicast Group to the send target
	// list. Combine with empty Group to disable.
	EnableSend bool

	// EnableListen, when true, opens a separate UDP socket bound to the
	// Group and joins it. Used to receive periodic multicast traffic
	// (HELLO advertisements, JOIN beacons, FRAME bodies) emitted by other
	// peers in addition to whatever lands on the send socket.
	EnableListen bool

	// LoopbackDisabled, when true, sets IP_MULTICAST_LOOP=0 / IPV6
	// equivalent on the send socket so locally-emitted datagrams are not
	// delivered back to the listen socket on the same host. JOIN-based
	// multicast peer transports want this off; SCOUT (which is fine with
	// loopback for local same-host discovery) leaves it on.
	LoopbackDisabled bool

	// Unicast is the list of additional "host:port" UDP send targets,
	// used by SCOUT to hit explicitly-configured peers.
	Unicast []string
}

// Sockets is the set of UDP sockets opened for a Plan. Any field may be
// nil — for example, a listen-only Plan leaves SendV4/SendV6 nil.
type Sockets struct {
	SendV4   *net.UDPConn
	SendV6   *net.UDPConn
	ListenV4 *net.UDPConn
	ListenV6 *net.UDPConn

	TargetsV4 []*net.UDPAddr
	TargetsV6 []*net.UDPAddr

	McastV4 *net.UDPAddr
	McastV6 *net.UDPAddr
}

// Conns returns every non-nil socket as a flat slice for iteration.
func (s *Sockets) Conns() []*net.UDPConn {
	out := make([]*net.UDPConn, 0, 4)
	for _, c := range []*net.UDPConn{s.SendV4, s.SendV6, s.ListenV4, s.ListenV6} {
		if c != nil {
			out = append(out, c)
		}
	}
	return out
}

// CloseAll closes every non-nil socket. Idempotent — closing an already-
// closed UDPConn is a no-op for our purposes (the returned error is
// silently dropped).
func (s *Sockets) CloseAll() {
	for _, c := range s.Conns() {
		_ = c.Close()
	}
}

// UnblockReaders nudges every read loop out of ReadFromUDP by setting an
// already-elapsed deadline. Readers translate the resulting timeout into
// a clean shutdown signal, so this should run before CloseAll to avoid
// racing a close with an in-flight Read.
func (s *Sockets) UnblockReaders() {
	now := time.Now()
	for _, c := range s.Conns() {
		_ = c.SetReadDeadline(now)
	}
}

// resolved is the internal expansion of a Plan: parsed multicast group
// + parsed unicast targets, split by IP family.
type resolved struct {
	targetsV4 []*net.UDPAddr
	targetsV6 []*net.UDPAddr
	mcastV4   *net.UDPAddr
	mcastV6   *net.UDPAddr
}

func resolvePlan(plan Plan) (*resolved, error) {
	r := &resolved{}
	if plan.Group != "" && (plan.EnableSend || plan.EnableListen) {
		u, err := net.ResolveUDPAddr("udp", plan.Group)
		if err != nil {
			return nil, fmt.Errorf("multicast: resolve group %q: %w", plan.Group, err)
		}
		if u.IP.To4() != nil {
			r.mcastV4 = u
			if plan.EnableSend {
				r.targetsV4 = append(r.targetsV4, u)
			}
		} else {
			r.mcastV6 = u
			if plan.EnableSend {
				r.targetsV6 = append(r.targetsV6, u)
			}
		}
	}
	for _, s := range plan.Unicast {
		u, err := net.ResolveUDPAddr("udp", s)
		if err != nil {
			return nil, fmt.Errorf("multicast: resolve unicast %q: %w", s, err)
		}
		if u.IP.To4() != nil {
			r.targetsV4 = append(r.targetsV4, u)
		} else {
			r.targetsV6 = append(r.targetsV6, u)
		}
	}
	return r, nil
}

// Open resolves the Plan's targets and opens the requested UDP sockets.
// Returns the Sockets bundle on success; a partially-opened bundle is
// closed before any error is returned.
func Open(plan Plan) (*Sockets, error) {
	r, err := resolvePlan(plan)
	if err != nil {
		return nil, err
	}
	out := &Sockets{
		TargetsV4: r.targetsV4,
		TargetsV6: r.targetsV6,
		McastV4:   r.mcastV4,
		McastV6:   r.mcastV6,
	}
	defer func() {
		if err != nil {
			out.CloseAll()
		}
	}()
	if len(r.targetsV4) > 0 {
		out.SendV4, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
		if err != nil {
			return nil, fmt.Errorf("multicast: listen udp4: %w", err)
		}
		if err = applySendOpts(out.SendV4, false, plan); err != nil {
			return nil, err
		}
	}
	if len(r.targetsV6) > 0 {
		out.SendV6, err = net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6unspecified, Port: 0})
		if err != nil {
			return nil, fmt.Errorf("multicast: listen udp6: %w", err)
		}
		if err = applySendOpts(out.SendV6, true, plan); err != nil {
			return nil, err
		}
	}
	if plan.EnableListen && r.mcastV4 != nil {
		out.ListenV4, err = net.ListenMulticastUDP("udp4", plan.Interface, r.mcastV4)
		if err != nil {
			return nil, fmt.Errorf("multicast: listen multicast v4: %w", err)
		}
	}
	if plan.EnableListen && r.mcastV6 != nil {
		out.ListenV6, err = net.ListenMulticastUDP("udp6", plan.Interface, r.mcastV6)
		if err != nil {
			return nil, fmt.Errorf("multicast: listen multicast v6: %w", err)
		}
	}
	return out, nil
}

// applySendOpts applies TTL / outgoing-interface / loopback overrides to
// a send socket. IPv4 and IPv6 use separate x/net PacketConn types that
// share the same method shape, so one helper covers both.
func applySendOpts(c *net.UDPConn, isV6 bool, plan Plan) error {
	if isV6 {
		p := ipv6.NewPacketConn(c)
		if plan.TTL > 0 {
			if err := p.SetMulticastHopLimit(plan.TTL); err != nil {
				return fmt.Errorf("multicast: set v6 multicast hop limit: %w", err)
			}
		}
		if plan.Interface != nil {
			if err := p.SetMulticastInterface(plan.Interface); err != nil {
				return fmt.Errorf("multicast: set v6 multicast interface: %w", err)
			}
		}
		if plan.LoopbackDisabled {
			if err := p.SetMulticastLoopback(false); err != nil {
				return fmt.Errorf("multicast: set v6 multicast loopback off: %w", err)
			}
		}
		return nil
	}
	p := ipv4.NewPacketConn(c)
	if plan.TTL > 0 {
		if err := p.SetMulticastTTL(plan.TTL); err != nil {
			return fmt.Errorf("multicast: set v4 multicast TTL: %w", err)
		}
	}
	if plan.Interface != nil {
		if err := p.SetMulticastInterface(plan.Interface); err != nil {
			return fmt.Errorf("multicast: set v4 multicast interface: %w", err)
		}
	}
	if plan.LoopbackDisabled {
		if err := p.SetMulticastLoopback(false); err != nil {
			return fmt.Errorf("multicast: set v4 multicast loopback off: %w", err)
		}
	}
	return nil
}
