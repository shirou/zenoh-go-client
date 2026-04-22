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
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// DefaultMulticastAddressV4 is the IPv4 multicast endpoint for zenoh SCOUT,
// matching zenoh-rust's DEFAULT_MULTICAST_IPV4_ADDRESS.
const DefaultMulticastAddressV4 = "224.0.0.224:7446"

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

	// MulticastAddress is the "host:port" UDP target. Empty means
	// DefaultMulticastAddressV4. Ignored when MulticastEnabled is false.
	MulticastAddress string

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
// an unrecoverable socket error occurs.
//
// deliver runs on Run's receive goroutine; to avoid stalling the receive loop,
// fan out by queuing into a channel or launching a goroutine inside deliver.
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

	targets, err := resolveTargets(opts)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return errors.New("scout: no multicast or unicast targets configured")
	}

	body, err := encodeScout(opts.ZID, matcher)
	if err != nil {
		return fmt.Errorf("scout: encode SCOUT: %w", err)
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return fmt.Errorf("scout: listen udp4: %w", err)
	}
	defer conn.Close()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if timeout > 0 {
		var deadlineCancel context.CancelFunc
		runCtx, deadlineCancel = context.WithTimeout(runCtx, timeout)
		defer deadlineCancel()
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		sendLoop(runCtx, conn, body, targets, interval)
	})

	// When the run context finishes, unblock ReadFromUDP by setting an
	// already-elapsed deadline. The sender goroutine honours ctx directly.
	wg.Go(func() {
		<-runCtx.Done()
		_ = conn.SetReadDeadline(time.Now())
	})

	defer wg.Wait()
	return receiveLoop(conn, opts.ZID, deliver)
}

func resolveTargets(opts Options) ([]*net.UDPAddr, error) {
	var targets []*net.UDPAddr
	if opts.MulticastEnabled {
		addr := opts.MulticastAddress
		if addr == "" {
			addr = DefaultMulticastAddressV4
		}
		u, err := net.ResolveUDPAddr("udp4", addr)
		if err != nil {
			return nil, fmt.Errorf("scout: resolve multicast %q: %w", addr, err)
		}
		targets = append(targets, u)
	}
	for _, s := range opts.UnicastAddresses {
		u, err := net.ResolveUDPAddr("udp4", s)
		if err != nil {
			return nil, fmt.Errorf("scout: resolve unicast %q: %w", s, err)
		}
		targets = append(targets, u)
	}
	return targets, nil
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

func receiveLoop(conn *net.UDPConn, selfZID wire.ZenohID, deliver func(Hello)) error {
	seen := make(map[string]struct{})
	buf := make([]byte, readBufSize)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				// Deadline set by the shutdown watchdog goroutine.
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("scout: read: %w", err)
		}
		h, err := decodeHello(buf[:n], src)
		if err != nil {
			slog.Default().Debug("scout: invalid HELLO",
				"src", src.String(), "err", err)
			continue
		}
		if selfZID.IsValid() && h.ZID.Equal(selfZID) {
			continue
		}
		key := string(h.ZID.Bytes)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		deliver(h)
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
