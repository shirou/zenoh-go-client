package session

import (
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/transport"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// peerRunLoop reads transport messages from the server-side of a pipe link
// until the link closes. It records KEEPALIVE counts so tests can assert
// that the client's keepalive goroutine is emitting.
type peerRunLoop struct {
	link        transport.Link
	keepalives  chan struct{} // buffered, signals keepalive received
	done        chan struct{}
	closeReason *uint8 // captured if peer gets a CLOSE from client
}

func newPeerRunLoop(link transport.Link) *peerRunLoop {
	return &peerRunLoop{
		link:       link,
		keepalives: make(chan struct{}, 16),
		done:       make(chan struct{}),
	}
}

func (p *peerRunLoop) run() {
	defer close(p.done)
	buf := make([]byte, codec.MaxBatchSize)
	for {
		n, err := p.link.ReadBatch(buf)
		if err != nil {
			return
		}
		r := codec.NewReader(buf[:n])
		h, err := r.DecodeHeader()
		if err != nil {
			return
		}
		switch h.ID {
		case wire.IDTransportKeepAlive:
			select {
			case p.keepalives <- struct{}{}:
			default:
			}
		case wire.IDTransportClose:
			if cl, err := wire.DecodeClose(r, h); err == nil {
				reason := cl.Reason
				p.closeReason = &reason
			}
			return
		case wire.IDTransportFrame:
			// Ignore network messages in this basic test fixture.
		}
	}
}

// TestSessionRunHandshakeAndKeepalive:
//   1. Client does DoHandshake against a mockPeer.
//   2. Client runs Session.Run, which spins up reader/writer/keepalive.
//   3. We verify at least one KEEPALIVE reaches the peer.
//   4. Client.Close cleanly shuts everything down; goleak asserts no leaks.
func TestSessionRunHandshakeAndKeepalive(t *testing.T) {
	clientLink, serverLink := newPipeLinks()
	defer clientLink.Close()

	peer := &mockPeer{
		ZID:     wire.ZenohID{Bytes: []byte{1, 2, 3, 4}},
		WhatAmI: wire.WhatAmIRouter,
		Lease:   400, // 400 ms lease → client keepalive every ~100ms
		EchoQoS: true,
	}
	// Run handshake + post-handshake loop on the server side in one goroutine.
	serverDone := make(chan error, 1)
	prl := newPeerRunLoop(serverLink)
	go func() {
		if err := peer.RunOnce(serverLink); err != nil {
			serverDone <- err
			return
		}
		prl.run()
		serverDone <- nil
	}()

	s := New()
	if err := s.BeginHandshake(); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultHandshakeConfig()
	cfg.ZID = GenerateZID()
	cfg.WhatAmI = wire.WhatAmIClient
	cfg.LeaseMillis = 400
	res, err := DoHandshake(clientLink, cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := s.Run(RunConfig{
		Link:      clientLink,
		Result:    res,
		KeepAlive: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = rt

	// Wait for at least one keepalive.
	select {
	case <-prl.keepalives:
	case <-time.After(2 * time.Second):
		t.Fatal("no KEEPALIVE received within 2s")
	}

	// Client close → reader exits on link close, writer drains, all goroutines join.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if !s.IsClosed() {
		t.Error("IsClosed should be true after Close")
	}
	<-prl.done
	if err := <-serverDone; err != nil {
		t.Errorf("server: %v", err)
	}
}

// TestSessionRunPeerCloseTriggersShutdown: peer sends CLOSE → reader
// observes it, triggers link close, client-side shuts down cleanly.
func TestSessionRunPeerCloseTriggersShutdown(t *testing.T) {
	clientLink, serverLink := newPipeLinks()
	defer clientLink.Close()

	peer := &mockPeer{
		ZID:     wire.ZenohID{Bytes: []byte{9, 9}},
		WhatAmI: wire.WhatAmIRouter,
		Lease:   10_000,
		EchoQoS: true,
	}
	go func() {
		if err := peer.RunOnce(serverLink); err != nil {
			return
		}
		// After handshake, immediately send CLOSE with EXPIRED.
		closeBytes, _ := EncodeCloseMessage(CloseReasonExpired)
		_ = serverLink.WriteBatch(closeBytes)
		serverLink.Close()
	}()

	s := New()
	_ = s.BeginHandshake()
	cfg := DefaultHandshakeConfig()
	cfg.ZID = GenerateZID()
	cfg.WhatAmI = wire.WhatAmIClient
	res, err := DoHandshake(clientLink, cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := s.Run(RunConfig{Link: clientLink, Result: res})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case reason := <-rt.PeerClose:
		if reason != CloseReasonExpired {
			t.Errorf("peer close reason = %#x, want %#x", reason, CloseReasonExpired)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("peer close not observed within 2s")
	}

	// Link should be closed by triggerShutdown; the runtime's LinkClosed
	// channel signals reader exit.
	select {
	case <-rt.LinkClosed:
	case <-time.After(time.Second):
		t.Fatal("LinkClosed did not close after peer CLOSE")
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestSessionRunLinkDropTriggersShutdown: peer closes TCP abruptly (no
// CLOSE); reader gets EOF and the session cleans up without the client
// needing to call Close first.
func TestSessionRunLinkDropTriggersShutdown(t *testing.T) {
	clientLink, serverLink := newPipeLinks()

	peer := &mockPeer{
		ZID:     wire.ZenohID{Bytes: []byte{7, 7}},
		WhatAmI: wire.WhatAmIRouter,
		Lease:   10_000,
	}
	go func() {
		_ = peer.RunOnce(serverLink)
		serverLink.Close() // abrupt drop
	}()

	s := New()
	_ = s.BeginHandshake()
	cfg := DefaultHandshakeConfig()
	cfg.ZID = GenerateZID()
	cfg.WhatAmI = wire.WhatAmIClient
	res, err := DoHandshake(clientLink, cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := s.Run(RunConfig{Link: clientLink, Result: res})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-rt.LinkClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("LinkClosed did not close after peer drop")
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

