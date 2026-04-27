package zenoh

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// peerListenAddr binds a tcp/127.0.0.1:0 socket, reads the resolved port
// out of it, and closes the socket so the test peer can re-bind it. Used
// to discover a free port before bringing up two co-operating peer
// sessions.
func peerListenAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// TestPeerModeMutualConnect spins up two peer-mode sessions, each
// listening on a free port and dialling the other. After a short stabili-
// sation window each side must have one Runtime registered for the other,
// matching the duplicate-connection tiebreak rule (canonical link only).
func TestPeerModeMutualConnect(t *testing.T) {
	addrA := peerListenAddr(t)
	addrB := peerListenAddr(t)

	cfgA := Config{
		Mode:            ModePeer,
		ZID:             "0a0a0a0a0a0a0a0a",
		ListenEndpoints: []string{"tcp/" + addrA},
		Endpoints:       []string{"tcp/" + addrB},
	}
	cfgB := Config{
		Mode:            ModePeer,
		ZID:             "0b0b0b0b0b0b0b0b",
		ListenEndpoints: []string{"tcp/" + addrB},
		Endpoints:       []string{"tcp/" + addrA},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var (
		sA, sB *Session
		errA   error
		errB   error
		wg     sync.WaitGroup
	)
	wg.Go(func() { sA, errA = Open(ctx, cfgA) })
	wg.Go(func() { sB, errB = Open(ctx, cfgB) })
	wg.Wait()
	if errA != nil {
		t.Fatalf("Open(A): %v", errA)
	}
	if errB != nil {
		t.Fatalf("Open(B): %v", errB)
	}
	defer sA.Close()
	defer sB.Close()

	// Wait for both sides to converge to exactly one runtime entry per
	// peer. Mutual dial races can briefly install both directions.
	if err := waitForCondition(2*time.Second, func() bool {
		return countRuntimes(sA) == 1 && countRuntimes(sB) == 1
	}); err != nil {
		t.Fatalf("convergence: A=%d B=%d", countRuntimes(sA), countRuntimes(sB))
	}

	// Each session knows the other's ZID via the registry.
	if !hasPeerZID(sA, sB.ZId()) {
		t.Errorf("session A has no runtime for B (zid=%s)", sB.ZId())
	}
	if !hasPeerZID(sB, sA.ZId()) {
		t.Errorf("session B has no runtime for A (zid=%s)", sA.ZId())
	}
}

// TestPeerModePeersZId asserts that PeersZId returns the partner peer
// after both sessions have converged.
func TestPeerModePeersZId(t *testing.T) {
	addrA := peerListenAddr(t)
	addrB := peerListenAddr(t)
	cfgA := Config{
		Mode:            ModePeer,
		ZID:             "a1a1a1a1a1a1a1a1",
		ListenEndpoints: []string{"tcp/" + addrA},
		Endpoints:       []string{"tcp/" + addrB},
	}
	cfgB := Config{
		Mode:            ModePeer,
		ZID:             "b2b2b2b2b2b2b2b2",
		ListenEndpoints: []string{"tcp/" + addrB},
		Endpoints:       []string{"tcp/" + addrA},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var (
		sA, sB *Session
		errA   error
		errB   error
		wg     sync.WaitGroup
	)
	wg.Go(func() { sA, errA = Open(ctx, cfgA) })
	wg.Go(func() { sB, errB = Open(ctx, cfgB) })
	wg.Wait()
	if errA != nil || errB != nil {
		t.Fatalf("Open: A=%v B=%v", errA, errB)
	}
	defer sA.Close()
	defer sB.Close()

	if err := waitForCondition(2*time.Second, func() bool {
		return len(sA.PeersZId()) == 1 && len(sB.PeersZId()) == 1
	}); err != nil {
		t.Fatalf("PeersZId did not converge: A=%v B=%v", sA.PeersZId(), sB.PeersZId())
	}
	if got := sA.PeersZId()[0]; got.String() != sB.ZId().String() {
		t.Errorf("A.PeersZId = %v, want %v", got, sB.ZId())
	}
	if got := sB.PeersZId()[0]; got.String() != sA.ZId().String() {
		t.Errorf("B.PeersZId = %v, want %v", got, sA.ZId())
	}
	if len(sA.RoutersZId()) != 0 {
		t.Errorf("A.RoutersZId = %v, want empty", sA.RoutersZId())
	}
}

// TestPeerModeListenOnly verifies that a peer-mode session with only
// ListenEndpoints (no Endpoints, no scouting) opens without error and
// remains usable until Close.
func TestPeerModeListenOnly(t *testing.T) {
	addr := peerListenAddr(t)
	cfg := Config{
		Mode:            ModePeer,
		ListenEndpoints: []string{"tcp/" + addr},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestPeerModeRejectsRouterMode ensures the parser-accepted "router"
// mode still surfaces as a runtime error since the routing logic isn't
// implemented yet.
func TestPeerModeRejectsRouterMode(t *testing.T) {
	cfg := Config{Mode: ModeRouter}
	_, err := Open(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected router-mode error")
	}
}

// TestPeerModeRequiresAtLeastOneEndpoint asserts the friendly error when
// peer mode is requested with no listen / connect / scouting endpoints.
func TestPeerModeRequiresAtLeastOneEndpoint(t *testing.T) {
	cfg := Config{Mode: ModePeer}
	_, err := Open(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error from empty peer config")
	}
}

// TestPeerModeCloseLeaks asserts that a peer-mode session's Close
// cascade (listeners + dial loops + scouting + per-runtime watchers)
// joins every goroutine it spawns. Local goleak scope avoids the rest
// of the zenoh package's pre-existing test goroutines (handler fixtures
// etc.) interfering with the assertion.
func TestPeerModeCloseLeaks(t *testing.T) {
	defer goleak.VerifyNone(t,
		// FifoChannel-based handlers retain a closer goroutine across
		// other tests in this package; ignore those.
		goleak.IgnoreTopFunction("github.com/shirou/zenoh-go-client/zenoh.Closure[...].Attach.func1"),
	)
	addrA := peerListenAddr(t)
	addrB := peerListenAddr(t)
	cfgA := Config{
		Mode:            ModePeer,
		ListenEndpoints: []string{"tcp/" + addrA},
		Endpoints:       []string{"tcp/" + addrB},
		Scouting: ScoutingConfig{
			MulticastMode: MulticastOff, // keep test focused; scouting separately covered
		},
	}
	cfgB := Config{
		Mode:            ModePeer,
		ListenEndpoints: []string{"tcp/" + addrB},
		Endpoints:       []string{"tcp/" + addrA},
		Scouting: ScoutingConfig{
			MulticastMode: MulticastOff,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var (
		sA, sB *Session
		errA   error
		errB   error
		wg     sync.WaitGroup
	)
	wg.Go(func() { sA, errA = Open(ctx, cfgA) })
	wg.Go(func() { sB, errB = Open(ctx, cfgB) })
	wg.Wait()
	if errA != nil || errB != nil {
		t.Fatalf("Open: A=%v B=%v", errA, errB)
	}

	if err := waitForCondition(2*time.Second, func() bool {
		return countRuntimes(sA) == 1 && countRuntimes(sB) == 1
	}); err != nil {
		t.Fatalf("convergence failed")
	}

	if err := sA.Close(); err != nil {
		t.Errorf("Close(A): %v", err)
	}
	if err := sB.Close(); err != nil {
		t.Errorf("Close(B): %v", err)
	}
}

func countRuntimes(s *Session) int {
	return len(s.inner.SnapshotRuntimes())
}

func hasPeerZID(s *Session, peer Id) bool {
	wireID := peer.ToWireID()
	for _, rt := range s.inner.SnapshotRuntimes() {
		if got := rt.PeerZIDBytes(); fmt.Sprintf("%x", got) == fmt.Sprintf("%x", wireID.Bytes) {
			return true
		}
	}
	return false
}

func waitForCondition(timeout time.Duration, cond func() bool) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	if cond() {
		return nil
	}
	return fmt.Errorf("condition not met within %s", timeout)
}
