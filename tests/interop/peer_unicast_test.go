//go:build interop

package interop

import (
	"context"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/zenoh"
)

// peerInteropAddr is the host port the Go peer binds for these tests.
// The Makefile target ships a compose override that maps
// host.docker.internal:7461 → host:7461 so the Python container can
// reach it; the test sets ZENOH_PEER_INTEROP=1 once that override is
// in play.
const peerInteropAddr = "127.0.0.1:7461"

// peerInteropEnabled gates the test on the harness flag the Makefile
// sets when the matching compose override is loaded. Without it the Go
// listener is unreachable from the Python container and the test would
// hang waiting for the peer to connect back.
func peerInteropEnabled() bool {
	v := os.Getenv("ZENOH_PEER_INTEROP")
	return v != "" && v != "0"
}

// TestGoPeerPySubscribe runs the Go session in peer mode, listening on
// peerInteropAddr, while the Python peer dials it and subscribes. After
// declaration converges, the Go peer publishes; the Python peer reports
// each sample on stdout. No router is involved.
func TestGoPeerPySubscribe(t *testing.T) {
	if !peerInteropEnabled() {
		t.Skip("peer interop disabled (set ZENOH_PEER_INTEROP=1 with the matching compose override)")
	}

	const (
		key   = "interop/peer/g2p"
		count = 3
	)

	// Bring the Go peer up first so the python script's connect succeeds.
	cfg := zenoh.Config{
		Mode:            zenoh.ModePeer,
		ListenEndpoints: []string{"tcp/" + peerInteropAddr},
		Scouting:        zenoh.ScoutingConfig{MulticastMode: zenoh.MulticastOff},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := zenoh.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("zenoh.Open(peer): %v", err)
	}
	defer session.Close()

	if err := waitForTCP(peerInteropAddr, 2*time.Second); err != nil {
		t.Fatalf("listener not ready: %v", err)
	}

	sub := startPython(t, "python_peer_sub.py", "--key", key, "--count", fmt.Sprint(count))
	defer sub.close(t)
	sub.waitFor(t, readyMarker, readyTimeout)

	if err := waitForCount(session.PeersZId, 1, 3*time.Second); err != nil {
		t.Fatalf("Python peer never appeared: %v", err)
	}

	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}
	pub, err := session.DeclarePublisher(ke, nil)
	if err != nil {
		t.Fatalf("DeclarePublisher: %v", err)
	}
	defer pub.Drop()
	time.Sleep(subPropagationDelay)

	for i := 0; i < count; i++ {
		if err := pub.Put(zenoh.NewZBytesFromString(fmt.Sprintf("g2p-%d", i)), nil); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}

	for i := 0; i < count; i++ {
		line := sub.readLine(t, ioTimeout)
		if line == doneMarker {
			t.Fatalf("done before %d samples (got %d)", count, i)
		}
		_, payload := decodeRecord(t, line)
		want := fmt.Sprintf("g2p-%d", i)
		if string(payload) != want {
			t.Errorf("payload[%d] = %q, want %q", i, payload, want)
		}
	}
	sub.waitFor(t, doneMarker, ioTimeout)
}

func waitForTCP(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("tcp %s not reachable within %s", addr, timeout)
}

func waitForCount[T any](f func() []T, want int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(f()) >= want {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("count never reached %d", want)
}
