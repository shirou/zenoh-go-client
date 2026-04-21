package zenoh

import (
	"context"
	"fmt"
	"runtime"
	"sync/atomic"

	intkeyexpr "github.com/shirou/zenoh-go-client/internal/keyexpr"
	"github.com/shirou/zenoh-go-client/internal/session"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// Session is the client's connection to a zenoh router or peer. Session
// values are safe for concurrent use.
type Session struct {
	inner   *session.Session
	runtime *session.Runtime
	zid     Id
	closed  atomic.Bool
}

// Open dials the first reachable endpoint in cfg.Endpoints, completes the
// INIT/OPEN handshake, and starts the session's internal goroutines.
func Open(ctx context.Context, cfg Config) (*Session, error) {
	if len(cfg.Endpoints) == 0 {
		return nil, fmt.Errorf("zenoh.Open: Config.Endpoints is empty")
	}

	zid, err := resolveZID(cfg.ZID)
	if err != nil {
		return nil, err
	}

	link, err := session.DialFirst(ctx, cfg.Endpoints)
	if err != nil {
		return nil, fmt.Errorf("zenoh.Open dial: %w", err)
	}

	inner := session.New()
	if err := inner.BeginHandshake(); err != nil {
		link.Close()
		return nil, err
	}

	hcfg := session.DefaultHandshakeConfig()
	hcfg.ZID = zid.ToWireID()
	hcfg.WhatAmI = wire.WhatAmIClient
	result, err := session.DoHandshake(link, hcfg)
	if err != nil {
		link.Close()
		_ = inner.Close()
		return nil, fmt.Errorf("zenoh.Open handshake: %w", err)
	}

	s := &Session{inner: inner, zid: IdFromWireID(hcfg.ZID)}

	rt, err := inner.Run(session.RunConfig{
		Link:     link,
		Result:   result,
		Dispatch: inner.NetworkDispatcher(),
	})
	if err != nil {
		link.Close()
		_ = inner.Close()
		return nil, fmt.Errorf("zenoh.Open run: %w", err)
	}
	s.runtime = rt
	return s, nil
}

// Close terminates the session, flushing pending messages, sending a CLOSE
// to the peer, and joining every per-session goroutine.
func (s *Session) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	s.sendFarewellClose()
	return s.inner.Close()
}

// sendFarewellClose enqueues a best-effort CLOSE frame. The outbound queue
// may race with the shutdown orchestrator (which closes it once the link
// goroutines exit). LinkClosed closes strictly before OutQ is closed, so
// observing LinkClosed as open means the send is safe; the recover is a
// narrow safety net for the "send on closed channel" race that only the
// Go runtime can surface here — it leaves every other panic intact.
func (s *Session) sendFarewellClose() {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(runtime.Error); !ok {
				panic(r)
			}
		}
	}()
	select {
	case <-s.runtime.LinkClosed:
		return
	default:
	}
	closeBytes, err := session.EncodeCloseMessage(session.CloseReasonGeneric)
	if err != nil {
		return
	}
	select {
	case s.runtime.OutQ <- session.OutboundItem{RawBatch: closeBytes}:
	case <-s.runtime.LinkClosed:
	default:
	}
}

// IsClosed reports whether Close has been called.
func (s *Session) IsClosed() bool { return s.closed.Load() }

// ZId returns the session's local ZenohID.
func (s *Session) ZId() Id { return s.zid }

// resolveZID parses hex or falls back to a fresh random ID.
func resolveZID(hexStr string) (Id, error) {
	if hexStr == "" {
		return IdFromWireID(session.GenerateZID()), nil
	}
	return NewIdFromHex(hexStr)
}

// internalKeyExpr returns the internal/keyexpr value for wire encoding / matching.
func (k KeyExpr) internalKeyExpr() intkeyexpr.KeyExpr { return k.inner }
