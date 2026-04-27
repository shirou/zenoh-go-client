package zenoh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shirou/zenoh-go-client/internal/locator"
	"github.com/shirou/zenoh-go-client/internal/scout"
	"github.com/shirou/zenoh-go-client/internal/session"
	"github.com/shirou/zenoh-go-client/internal/transport"
)

// openPeer brings up listeners and outbound dial loops in peer mode.
// Listeners are bound synchronously so the returned session is
// guaranteed to be reachable when Open returns; outbound dials retry in
// the background.
//
// Endpoints are parse-validated up front so a malformed entry fails Open
// immediately rather than feeding an infinite log-spam retry loop.
func (s *Session) openPeer(ctx context.Context) error {
	udpEndpoints, tcpEndpoints, err := splitMulticastEndpoints(s.cfg.Endpoints)
	if err != nil {
		return fmt.Errorf("connect endpoint: %w", err)
	}
	if err := validateEndpointsParse(s.cfg.ListenEndpoints); err != nil {
		return fmt.Errorf("listen endpoint: %w", err)
	}

	for _, ep := range s.cfg.ListenEndpoints {
		ln, err := s.bindListener(ctx, ep)
		if err != nil {
			s.closeAllListeners()
			return fmt.Errorf("listen %q: %w", ep, err)
		}
		s.listenersMu.Lock()
		s.listeners = append(s.listeners, ln)
		s.listenersMu.Unlock()
		s.peerWG.Go(func() { s.runAcceptLoop(ln) })
	}
	for _, ep := range tcpEndpoints {
		s.peerWG.Go(func() { s.runOutboundLoop(ep) })
	}
	for _, ep := range udpEndpoints {
		if err := s.startMulticastEndpoint(ep); err != nil {
			s.inner.Logger().Warn("multicast peer endpoint failed", "endpoint", ep, "err", err)
		}
	}
	if s.cfg.Scouting.MulticastMode != MulticastOff {
		s.peerWG.Go(s.runScoutDiscovery)
	}
	return nil
}

// splitMulticastEndpoints partitions Endpoints into UDP (multicast peer
// transports) and everything else (TCP today, TLS/QUIC eventually). A
// parse failure is surfaced as an error so the caller can fail Open
// instead of letting the bad endpoint loop forever in runOutboundLoop.
func splitMulticastEndpoints(eps []string) (udp, other []string, err error) {
	for _, ep := range eps {
		loc, perr := locator.Parse(ep)
		if perr != nil {
			return nil, nil, fmt.Errorf("invalid endpoint %q: %w", ep, perr)
		}
		if loc.Scheme == locator.SchemeUDP {
			udp = append(udp, ep)
		} else {
			other = append(other, ep)
		}
	}
	return udp, other, nil
}

// validateEndpointsParse checks every entry parses as a locator. Used for
// listen endpoints, where the dial-side equivalents are already covered
// by splitMulticastEndpoints.
func validateEndpointsParse(eps []string) error {
	for _, ep := range eps {
		if _, err := locator.Parse(ep); err != nil {
			return fmt.Errorf("invalid endpoint %q: %w", ep, err)
		}
	}
	return nil
}

// startMulticastEndpoint dials the given multicast group locator and
// starts a JOIN-driven MulticastRuntime against it. Pub/sub propagation
// is not yet wired — the runtime is discovery-only at present, so
// declarations on this Session still need a unicast peer or router for
// data delivery.
func (s *Session) startMulticastEndpoint(endpoint string) error {
	loc, err := locator.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("parse %q: %w", endpoint, err)
	}
	link, err := transport.DialMulticastUDP(loc, transport.MulticastDialOpts{
		Interface:        nil,
		TTL:              s.cfg.Scouting.MulticastTTL,
		LoopbackDisabled: true, // do not register our own JOIN as a peer
	})
	if err != nil {
		return fmt.Errorf("dial multicast %q: %w", endpoint, err)
	}
	rt, err := s.inner.StartMulticastPeer(session.MulticastConfig{
		Link:        link,
		ZID:         s.zid.ToWireID(),
		WhatAmI:     sessionModeToWire(s.cfg.Mode),
		LeaseMillis: 10000,
		BatchSize:   uint16(transport.MulticastBatchSize),
		Logger:      s.inner.Logger(),
	})
	if err != nil {
		_ = link.Close()
		return err
	}
	s.multicastMu.Lock()
	s.multicastRuntimes = append(s.multicastRuntimes, rt)
	s.multicastMu.Unlock()
	// When the user closes the session, master ctx → cancel → runtime
	// observes via its own ctx. Tie that here so peerWG.Wait blocks until
	// the runtime exits.
	closeCtx := s.userCloseCtx()
	s.peerWG.Go(func() {
		<-closeCtx.Done()
		_ = rt.Close()
	})
	return nil
}

// runScoutDiscovery runs scout.Run in the background, auto-dialling
// peers whose WhatAmI matches AutoconnectMask. The mask defaults to
// WhatDefault (router|peer) when unset.
func (s *Session) runScoutDiscovery() {
	mask := s.cfg.AutoconnectMask
	if mask == 0 {
		mask = WhatDefault
	}
	opts, err := buildScoutOptions(s.cfg, &ScoutOptions{What: mask})
	if err != nil {
		s.inner.Logger().Warn("scout setup failed", "err", err)
		return
	}
	// Disable the implicit timeout — peer-mode scouting runs until Close.
	opts.Timeout = -1
	opts.ZID = s.zid.ToWireID()
	closeCtx := s.userCloseCtx()
	if err := scout.Run(closeCtx, opts, s.onHello(mask)); err != nil && !errors.Is(err, context.Canceled) {
		s.inner.Logger().Debug("scout discovery exited", "err", err)
	}
}

// onHello returns the deliver callback for scout.Run. It filters the
// reply against AutoconnectMask, skips peers we already have a Runtime
// for, and triggers a one-shot dial to each candidate locator.
func (s *Session) onHello(mask What) func(scout.Hello) {
	return func(h scout.Hello) {
		if What(1<<h.WhatAmI)&mask == 0 {
			return
		}
		// Skip self and any peer we already have an established runtime for.
		myZID := s.zid.ToWireID().Bytes
		if bytes.Equal(h.ZID.Bytes, myZID) {
			return
		}
		if s.inner.RuntimeFor(h.ZID.Bytes) != nil {
			return
		}
		s.peerWG.Go(func() { s.dialDiscoveredPeer(h) })
	}
}

// dialDiscoveredPeer tries each advertised locator in order, stopping on
// the first successful handshake. Failures are logged at Debug since
// scouting can produce noisy "down peer" results.
func (s *Session) dialDiscoveredPeer(h scout.Hello) {
	closeCtx := s.userCloseCtx()
	for _, ep := range h.Locators {
		select {
		case <-closeCtx.Done():
			return
		default:
		}
		// Bail out if a parallel direction (incoming connection or another
		// HELLO) already established the runtime.
		if s.inner.RuntimeFor(h.ZID.Bytes) != nil {
			return
		}
		ctx, cancel := s.dialContext(time.Duration(s.backoff.MaxMs) * time.Millisecond)
		_, err := s.dialOneAndRun(ctx, ep)
		cancel()
		if err == nil {
			return
		}
		s.inner.Logger().Debug("scout-driven dial failed", "endpoint", ep, "err", err)
	}
}

// runPeerSupervisor waits for every spawned goroutine to exit and closes
// s.done. Mirrors runReconnectLoop's role of fronting Close().
func (s *Session) runPeerSupervisor() {
	defer close(s.done)
	s.peerWG.Wait()
}

// bindListener resolves an endpoint string into a transport.Listener.
// Errors out with a clear message if the scheme has no listener factory.
func (s *Session) bindListener(ctx context.Context, endpoint string) (transport.Listener, error) {
	loc, err := locator.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	factory := transport.ListenerFactoryFor(loc.Scheme)
	if factory == nil {
		return nil, fmt.Errorf("no listener factory for scheme %q", loc.Scheme)
	}
	return factory.Listen(ctx, loc)
}

// runAcceptLoop accepts incoming connections until the listener closes
// or the user closes the session. Each successful accept is handed off
// to a goroutine that runs the responder handshake.
func (s *Session) runAcceptLoop(ln transport.Listener) {
	logger := s.inner.Logger()
	closeCtx := s.userCloseCtx()
	for {
		link, err := ln.Accept(closeCtx)
		if err != nil {
			if errors.Is(err, transport.ErrListenerClosed) || errors.Is(err, context.Canceled) {
				return
			}
			// Transient accept errors: log and back off briefly.
			logger.Debug("accept failed", "err", err)
			select {
			case <-time.After(50 * time.Millisecond):
			case <-closeCtx.Done():
				return
			}
			continue
		}
		s.peerWG.Go(func() { s.handleAccepted(link) })
	}
}

// handleAccepted runs the responder handshake on link and installs the
// resulting Runtime. On any failure the link is closed and we return so
// the accept loop can take the next one.
func (s *Session) handleAccepted(link transport.Link) {
	cfg := session.DefaultAcceptConfig()
	cfg.ZID = s.zid.ToWireID()
	cfg.WhatAmI = sessionModeToWire(s.cfg.Mode)
	if err := s.inner.BeginHandshake(); err != nil {
		_ = link.Close()
		return
	}
	result, err := session.DoHandshakeResponder(link, cfg)
	if err != nil {
		s.inner.Logger().Debug("responder handshake failed", "err", err)
		_ = link.Close()
		return
	}
	s.startRuntimeForPeer(link, result, false)
}

// runOutboundLoop continuously dials endpoint, runs the initiator
// handshake, and installs the resulting Runtime. On link drop or any
// transient error it backs off and retries until the user closes the
// session.
func (s *Session) runOutboundLoop(endpoint string) {
	closeCtx := s.userCloseCtx()
	b := session.NewBackoff(s.backoff)
	for {
		select {
		case <-closeCtx.Done():
			return
		default:
		}
		attemptCtx, cancel := s.dialContext(time.Duration(s.backoff.MaxMs) * time.Millisecond)
		rt, err := s.dialOneAndRun(attemptCtx, endpoint)
		cancel()
		if err != nil {
			s.inner.Logger().Debug("outbound dial failed", "endpoint", endpoint, "err", err)
			wait := b.Next()
			select {
			case <-time.After(wait):
				continue
			case <-closeCtx.Done():
				return
			}
		}
		b = session.NewBackoff(s.backoff) // reset backoff after success
		// Wait until the runtime tears down, then loop and re-dial. The
		// teardown watcher spawned in startRuntimeForPeer owns the actual
		// uninstallRuntime call; this loop only observes Done() to know
		// when to retry the dial.
		<-rt.Done()
		select {
		case <-closeCtx.Done():
			return
		default:
		}
	}
}

// dialOneAndRun dials a single endpoint, runs the initiator handshake,
// and installs the runtime via the tiebreak-aware path.
func (s *Session) dialOneAndRun(ctx context.Context, endpoint string) (*session.Runtime, error) {
	link, err := session.DialOne(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	if err := s.inner.BeginHandshake(); err != nil {
		_ = link.Close()
		return nil, err
	}
	hcfg := session.DefaultHandshakeConfig()
	hcfg.ZID = s.zid.ToWireID()
	hcfg.WhatAmI = sessionModeToWire(s.cfg.Mode)
	result, err := session.DoHandshake(link, hcfg)
	if err != nil {
		_ = link.Close()
		return nil, fmt.Errorf("handshake: %w", err)
	}
	rt, err := s.startRuntimeForPeer(link, result, true)
	if err != nil {
		return nil, err
	}
	return rt, nil
}

// startRuntimeForPeer wires up the Runtime, applies the duplicate-
// connection tiebreak, registers the survivor, and replays existing
// declarations so the freshly-connected peer learns about them.
//
// isInitiator is true when we dialled the link, false when we accepted
// it. The tiebreak rule keeps the link whose initiator has the
// lexicographically-smaller ZID — both sides reach the same decision
// because each evaluates the same predicate on the same pair.
func (s *Session) startRuntimeForPeer(link transport.Link, result *session.HandshakeResult, isInitiator bool) (*session.Runtime, error) {
	myZID := s.zid.ToWireID().Bytes
	peerZID := result.PeerZID.Bytes

	// Serialise the canonical-link decision so two concurrent handshakes
	// for the same peer cannot both observe an empty registry and both
	// register. The lock spans Run as well so the loser's link is closed
	// before any rt.Done observer can register an unregister.
	s.peerInstallMu.Lock()
	defer s.peerInstallMu.Unlock()

	if existing := s.inner.RuntimeFor(peerZID); existing != nil {
		newCanonical := isCanonicalLink(myZID, peerZID, isInitiator)
		if !newCanonical {
			// Existing wins. Close our link without ever spinning up a
			// runtime — that way the remote side, which kept this link's
			// far end, doesn't see it drop.
			_ = link.Close()
			return nil, nil
		}
		// New connection wins — drop the old one before installing.
		existing.Shutdown()
	}

	rt, err := s.inner.Run(session.RunConfig{
		Link:     link,
		Result:   result,
		Dispatch: s.inner.NetworkDispatcher(),
	})
	if err != nil {
		_ = link.Close()
		return nil, fmt.Errorf("run: %w", err)
	}

	s.inner.RegisterRuntime(peerZID, rt)
	s.curRuntime.Store(rt)
	if err := s.replayToRuntime(rt); err != nil {
		s.inner.Logger().Warn("peer-mode replay failed", "err", err)
	}
	// Watch for runtime teardown so accepted / scout-triggered runtimes
	// (which have no surrounding loop to call uninstallRuntime) leave
	// the registry when their Link drops. The outbound dial loop also
	// relies on this; it observes rt.Done() separately to know when to
	// re-dial, but the unregister itself is owned here.
	s.peerWG.Go(func() {
		<-rt.Done()
		s.uninstallRuntime(rt)
	})
	return rt, nil
}

// isCanonicalLink reports whether the link with the given local
// orientation is the survivor when both sides connect simultaneously.
// The rule is: keep the link whose initiator ZID is lexicographically
// smaller. Both ends reach the same answer.
func isCanonicalLink(myZID, peerZID []byte, isInitiator bool) bool {
	myLower := bytes.Compare(myZID, peerZID) < 0
	if isInitiator {
		return myLower
	}
	return !myLower
}
