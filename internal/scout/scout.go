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

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/locator"
	"github.com/shirou/zenoh-go-client/internal/multicast"
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

	plan := multicast.Plan{
		Group:        scoutMulticastAddress(opts.MulticastAddress),
		Interface:    opts.MulticastInterface,
		TTL:          opts.MulticastTTL,
		EnableSend:   opts.MulticastEnabled,
		EnableListen: opts.MulticastListen,
		Unicast:      opts.UnicastAddresses,
	}
	if !opts.MulticastEnabled && !opts.MulticastListen {
		// Disable the multicast group entirely for unicast-only scouting.
		plan.Group = ""
	}
	sockets, err := multicast.Open(plan)
	if err != nil {
		return err
	}
	hasSend := len(sockets.TargetsV4)+len(sockets.TargetsV6) > 0
	hasListen := opts.MulticastListen && (sockets.McastV4 != nil || sockets.McastV6 != nil)
	if !hasSend && !hasListen {
		sockets.CloseAll()
		return errors.New("scout: no multicast/unicast targets and no multicast listener configured")
	}

	body, err := encodeScout(opts.ZID, matcher)
	if err != nil {
		sockets.CloseAll()
		return fmt.Errorf("scout: encode SCOUT: %w", err)
	}

	defer sockets.CloseAll()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if timeout > 0 {
		var deadlineCancel context.CancelFunc
		runCtx, deadlineCancel = context.WithTimeout(runCtx, timeout)
		defer deadlineCancel()
	}

	dispatcher := newDispatcher(opts.ZID, deliver)

	var wg sync.WaitGroup
	if sockets.SendV4 != nil {
		targets := sockets.TargetsV4
		wg.Go(func() { sendLoop(runCtx, sockets.SendV4, body, targets, interval) })
	}
	if sockets.SendV6 != nil {
		targets := sockets.TargetsV6
		wg.Go(func() { sendLoop(runCtx, sockets.SendV6, body, targets, interval) })
	}
	for _, c := range sockets.Conns() {
		wg.Go(func() { readLoop(c, dispatcher) })
	}
	wg.Go(func() {
		<-runCtx.Done()
		sockets.UnblockReaders()
	})

	wg.Wait()
	return nil
}

// scoutMulticastAddress returns the configured multicast group address
// for SCOUT, falling back to DefaultMulticastAddressV4 when empty.
func scoutMulticastAddress(addr string) string {
	if addr == "" {
		return DefaultMulticastAddressV4
	}
	return addr
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
