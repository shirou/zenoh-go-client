// Package scout implements the zenoh SCOUT/HELLO discovery protocol.
//
// A scouter broadcasts SCOUT messages (typically over UDP multicast) and
// listens for HELLO replies. Each reachable peer whose role matches the
// scouter's WhatAmIMatcher answers with a HELLO that advertises its ZenohID
// and locators.
//
// This package is session-independent: it only speaks the SCOUT/HELLO wire
// format from internal/wire and plain UDP sockets. Higher layers wrap Run in
// a handler-based API (see zenoh.Scout).
package scout

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"sync"
	"time"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/locator"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// DefaultMulticastAddressV4 is the IPv4 multicast endpoint for zenoh SCOUT,
// matching zenoh-rust's DEFAULT_MULTICAST_IPV4_ADDRESS.
const DefaultMulticastAddressV4 = "224.0.0.224:7446"

// DefaultMulticastAddressV6 is the IPv6 equivalent (link-local scope).
const DefaultMulticastAddressV6 = "[ff02::224]:7446"

// DefaultInterval is the SCOUT retransmission period.
const DefaultInterval = time.Second

// DefaultTimeout is the overall scouting timeout. Zero-valued Options.Timeout
// falls back to this.
const DefaultTimeout = 3 * time.Second

const readBufSize = 4096

// Hello is a decoded HELLO reply enriched with a synthesised locator when the
// peer omitted L==1.
type Hello struct {
	WhatAmI wire.WhatAmI
	// ZID owns its backing slice (copied from the receive buffer), so it is
	// safe to retain past the next read.
	ZID wire.ZenohID
	// Locators is never nil. When the HELLO carried L==0, a single
	// "udp/<src-addr>" entry synthesised from the datagram source is returned
	// (per hello.adoc §Behaviour).
	Locators []string
}

// Options configure a Run invocation.
type Options struct {
	// MulticastEnabled gates the multicast target. When true, MulticastAddress
	// (or DefaultMulticastAddressV4) is added to the send list.
	MulticastEnabled bool

	// MulticastAddress is the "host:port" UDP target. For IPv6 use the
	// bracketed form, e.g. "[ff02::224]:7446". Empty means
	// DefaultMulticastAddressV4. Ignored when MulticastEnabled is false.
	MulticastAddress string

	// MulticastInterface is the outgoing interface for multicast SCOUT and
	// the interface on which the multicast listen socket (if enabled) joins
	// the group. nil = OS default.
	MulticastInterface *net.Interface

	// MulticastTTL sets the IP multicast hop limit. 0 = OS default (1 on
	// most systems — link-local only).
	MulticastTTL int

	// MulticastListen, when true, binds an additional socket to the
	// multicast group + port and joins the group so periodic multicast
	// HELLO advertisements from other peers are observed (in addition to
	// unicast replies on the sender socket).
	MulticastListen bool

	// UnicastAddresses is a list of "host:port" UDP targets to SCOUT
	// directly, in addition to (or instead of) the multicast target.
	UnicastAddresses []string

	// Matcher is the WhatAmIMatcher bitmap of roles to discover. Zero maps
	// to wire.MatcherAny.
	Matcher wire.WhatAmIMatcher

	// ZID is the scouter's own ZenohID, enabling the SCOUT I flag and
	// self-suppression of our own HELLOs. Zero-value disables both.
	ZID wire.ZenohID

	// Interval is the SCOUT retransmit period. Zero maps to DefaultInterval.
	Interval time.Duration

	// Timeout is the overall scouting duration. Zero maps to DefaultTimeout.
	// Negative disables the implicit timeout — callers drive shutdown via ctx.
	Timeout time.Duration
}

// Run sends SCOUT messages and delivers each unique HELLO (deduplicated by
// ZID) via deliver. It returns when ctx is cancelled, the timeout elapses, or
// socket setup fails.
//
// deliver is serialised across all receive sockets and may be called for any
// HELLO whose ZID has not yet been seen. Callers that perform heavy work
// inside deliver should push into a channel to avoid stalling the receivers.
func Run(ctx context.Context, opts Options, deliver func(Hello)) error {
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	matcher := opts.Matcher
	if matcher == 0 {
		matcher = wire.MatcherAny
	}

	plan, err := planSockets(opts)
	if err != nil {
		return err
	}
	hasSend := len(plan.targetsV4)+len(plan.targetsV6) > 0
	hasListen := opts.MulticastListen && (plan.mcastV4 != nil || plan.mcastV6 != nil)
	if !hasSend && !hasListen {
		return errors.New("scout: no multicast/unicast targets and no multicast listener configured")
	}

	body, err := encodeScout(opts.ZID, matcher)
	if err != nil {
		return fmt.Errorf("scout: encode SCOUT: %w", err)
	}

	sockets, err := openSockets(plan, opts)
	if err != nil {
		return err
	}
	defer sockets.closeAll()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if timeout > 0 {
		var deadlineCancel context.CancelFunc
		runCtx, deadlineCancel = context.WithTimeout(runCtx, timeout)
		defer deadlineCancel()
	}

	dispatcher := newDispatcher(opts.ZID, deliver)

	var wg sync.WaitGroup
	if sockets.sendV4 != nil {
		wg.Go(func() { sendLoop(runCtx, sockets.sendV4, body, plan.targetsV4, interval) })
	}
	if sockets.sendV6 != nil {
		wg.Go(func() { sendLoop(runCtx, sockets.sendV6, body, plan.targetsV6, interval) })
	}
	for _, c := range sockets.conns() {
		wg.Go(func() { readLoop(c, dispatcher) })
	}
	wg.Go(func() {
		<-runCtx.Done()
		sockets.unblockReaders()
	})

	wg.Wait()
	return nil
}

type socketPlan struct {
	targetsV4 []*net.UDPAddr
	targetsV6 []*net.UDPAddr
	mcastV4   *net.UDPAddr // non-nil when a v4 multicast group is in play (send or listen)
	mcastV6   *net.UDPAddr
}

func planSockets(opts Options) (*socketPlan, error) {
	p := &socketPlan{}
	// A multicast address is resolved if we'll either send to it or listen
	// on the group. MulticastListen without MulticastEnabled yields a
	// passive observer — no SCOUT is transmitted, but multicast HELLO
	// advertisements still reach the read loop.
	if opts.MulticastEnabled || opts.MulticastListen {
		addr := opts.MulticastAddress
		if addr == "" {
			addr = DefaultMulticastAddressV4
		}
		u, err := net.ResolveUDPAddr("udp", addr)
		if err != nil {
			return nil, fmt.Errorf("scout: resolve multicast %q: %w", addr, err)
		}
		if u.IP.To4() != nil {
			p.mcastV4 = u
			if opts.MulticastEnabled {
				p.targetsV4 = append(p.targetsV4, u)
			}
		} else {
			p.mcastV6 = u
			if opts.MulticastEnabled {
				p.targetsV6 = append(p.targetsV6, u)
			}
		}
	}
	for _, s := range opts.UnicastAddresses {
		u, err := net.ResolveUDPAddr("udp", s)
		if err != nil {
			return nil, fmt.Errorf("scout: resolve unicast %q: %w", s, err)
		}
		if u.IP.To4() != nil {
			p.targetsV4 = append(p.targetsV4, u)
		} else {
			p.targetsV6 = append(p.targetsV6, u)
		}
	}
	return p, nil
}

type openedSockets struct {
	sendV4   *net.UDPConn
	sendV6   *net.UDPConn
	listenV4 *net.UDPConn
	listenV6 *net.UDPConn
}

func (s *openedSockets) conns() []*net.UDPConn {
	out := make([]*net.UDPConn, 0, 4)
	for _, c := range []*net.UDPConn{s.sendV4, s.sendV6, s.listenV4, s.listenV6} {
		if c != nil {
			out = append(out, c)
		}
	}
	return out
}

func (s *openedSockets) closeAll() {
	for _, c := range s.conns() {
		_ = c.Close()
	}
}

// unblockReaders nudges every read loop out of ReadFromUDP by setting an
// already-elapsed deadline. Readers treat the resulting timeout as a clean
// shutdown signal, so this runs before closeAll to avoid racing a close
// with an in-flight Read.
func (s *openedSockets) unblockReaders() {
	now := time.Now()
	for _, c := range s.conns() {
		_ = c.SetReadDeadline(now)
	}
}

func openSockets(plan *socketPlan, opts Options) (*openedSockets, error) {
	out := &openedSockets{}
	var err error
	defer func() {
		if err != nil {
			out.closeAll()
		}
	}()

	if len(plan.targetsV4) > 0 {
		out.sendV4, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
		if err != nil {
			return nil, fmt.Errorf("scout: listen udp4: %w", err)
		}
		if err = applyMulticastOpts(out.sendV4, false, opts); err != nil {
			return nil, err
		}
	}
	if len(plan.targetsV6) > 0 {
		out.sendV6, err = net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6unspecified, Port: 0})
		if err != nil {
			return nil, fmt.Errorf("scout: listen udp6: %w", err)
		}
		if err = applyMulticastOpts(out.sendV6, true, opts); err != nil {
			return nil, err
		}
	}
	if opts.MulticastListen && plan.mcastV4 != nil {
		out.listenV4, err = net.ListenMulticastUDP("udp4", opts.MulticastInterface, plan.mcastV4)
		if err != nil {
			return nil, fmt.Errorf("scout: listen multicast v4: %w", err)
		}
	}
	if opts.MulticastListen && plan.mcastV6 != nil {
		out.listenV6, err = net.ListenMulticastUDP("udp6", opts.MulticastInterface, plan.mcastV6)
		if err != nil {
			return nil, fmt.Errorf("scout: listen multicast v6: %w", err)
		}
	}
	return out, nil
}

// applyMulticastOpts applies TTL / outgoing-interface overrides to a send
// socket. IPv4 and IPv6 use separate x/net PacketConn types that share the
// same method shape, so one helper covers both.
func applyMulticastOpts(c *net.UDPConn, isV6 bool, opts Options) error {
	if opts.MulticastTTL == 0 && opts.MulticastInterface == nil {
		return nil
	}
	if isV6 {
		p := ipv6.NewPacketConn(c)
		if opts.MulticastTTL > 0 {
			if err := p.SetMulticastHopLimit(opts.MulticastTTL); err != nil {
				return fmt.Errorf("scout: set v6 multicast hop limit: %w", err)
			}
		}
		if opts.MulticastInterface != nil {
			if err := p.SetMulticastInterface(opts.MulticastInterface); err != nil {
				return fmt.Errorf("scout: set v6 multicast interface: %w", err)
			}
		}
		return nil
	}
	p := ipv4.NewPacketConn(c)
	if opts.MulticastTTL > 0 {
		if err := p.SetMulticastTTL(opts.MulticastTTL); err != nil {
			return fmt.Errorf("scout: set v4 multicast TTL: %w", err)
		}
	}
	if opts.MulticastInterface != nil {
		if err := p.SetMulticastInterface(opts.MulticastInterface); err != nil {
			return fmt.Errorf("scout: set v4 multicast interface: %w", err)
		}
	}
	return nil
}

func sendLoop(ctx context.Context, conn *net.UDPConn, body []byte, targets []*net.UDPAddr, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		for _, target := range targets {
			if _, err := conn.WriteToUDP(body, target); err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				slog.Default().Debug("scout: send SCOUT failed",
					"target", target.String(), "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// dispatcher serialises HELLO delivery across multiple receive sockets and
// drops HELLOs whose ZIDs have already been reported.
type dispatcher struct {
	mu      sync.Mutex
	seen    map[string]struct{}
	selfZID wire.ZenohID
	deliver func(Hello)
}

func newDispatcher(selfZID wire.ZenohID, deliver func(Hello)) *dispatcher {
	return &dispatcher{
		seen:    make(map[string]struct{}),
		selfZID: selfZID,
		deliver: deliver,
	}
}

// tryDeliver deduplicates by ZID and invokes deliver under the dispatcher's
// mutex so user callbacks observe a well-defined order even when multiple
// sockets receive HELLOs concurrently.
func (d *dispatcher) tryDeliver(h Hello) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.selfZID.IsValid() && h.ZID.Equal(d.selfZID) {
		return
	}
	key := string(h.ZID.Bytes)
	if _, dup := d.seen[key]; dup {
		return
	}
	d.seen[key] = struct{}{}
	d.deliver(h)
}

func readLoop(conn *net.UDPConn, d *dispatcher) {
	buf := make([]byte, readBufSize)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Default().Debug("scout: read failed", "err", err)
			return
		}
		h, err := decodeHello(buf[:n], src)
		if err != nil {
			slog.Default().Debug("scout: invalid HELLO",
				"src", src.String(), "err", err)
			continue
		}
		d.tryDeliver(h)
	}
}

func encodeScout(zid wire.ZenohID, matcher wire.WhatAmIMatcher) ([]byte, error) {
	msg := &wire.Scout{
		Version: wire.ProtoVersion,
		Matcher: matcher,
		ZID:     zid,
	}
	w := codec.NewWriter(32)
	if err := msg.EncodeTo(w); err != nil {
		return nil, err
	}
	return bytes.Clone(w.Bytes()), nil
}

func decodeHello(buf []byte, src *net.UDPAddr) (Hello, error) {
	r := codec.NewReader(buf)
	h, err := r.DecodeHeader()
	if err != nil {
		return Hello{}, fmt.Errorf("decode header: %w", err)
	}
	if h.ID != wire.IDScoutHello {
		return Hello{}, fmt.Errorf("expected HELLO id=%#x, got %#x", wire.IDScoutHello, h.ID)
	}
	msg, err := wire.DecodeHello(r, h)
	if err != nil {
		return Hello{}, err
	}
	// DecodeZIDBytes aliases the receive buffer; clone so callers can retain.
	zid := wire.ZenohID{Bytes: bytes.Clone(msg.ZID.Bytes)}
	out := Hello{WhatAmI: msg.WhatAmI, ZID: zid}
	if len(msg.Locators) > 0 {
		out.Locators = slices.Clone(msg.Locators)
	} else {
		// hello.adoc §Behaviour: with L==0 the packet source is the locator.
		out.Locators = []string{locator.Locator{Scheme: locator.SchemeUDP, Address: src.String()}.String()}
	}
	return out, nil
}
