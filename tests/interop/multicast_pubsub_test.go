//go:build interop_multicast

package interop

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/zenoh"
)

// Run with: `make interop-multicast-up && go test -tags interop_multicast ./tests/interop/...`
// Linux only: peer-multicast requires the multicast group to actually
// route between sockets, which docker bridge networking does not support.

const (
	mcGroup        = "udp/224.0.0.224:7446"
	mcReadyTimeout = 10 * time.Second
	mcIOTimeout    = 15 * time.Second
	mcReady        = "READY"
	mcDone         = "DONE"
	mcGo           = "GO"
)

// multicastComposeFile is resolved relative to this test file.
var multicastComposeFile = func() string {
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return filepath.Join(wd, "..", "docker-compose.multicast.yml")
}()

// mcRequireHarness skips when the docker compose harness for the
// multicast suite isn't running. The regular `make interop-up` brings
// up a bridge-networked zenohd that also binds tcp/127.0.0.1:7447, so
// a plain TCP probe can't tell them apart. We send a SCOUT and require
// at least one HELLO back within 1.5s — the host-networked zenohd-
// multicast responds to it; the bridge-networked one doesn't (its
// multicast scout interface isn't reachable from the host). On
// failure, the skip message points at the right Make target.
func mcRequireHarness(t *testing.T) {
	t.Helper()
	scoutCfg := zenoh.NewConfig()
	scoutCfg.Scouting.MulticastMode = zenoh.MulticastAuto
	ch, err := zenoh.Scout(scoutCfg, zenoh.NewFifoChannel[zenoh.Hello](2), &zenoh.ScoutOptions{
		TimeoutMs: 1500,
		What:      zenoh.WhatRouter,
	})
	if err != nil {
		t.Skipf("multicast harness probe failed (run `make interop-multicast-up` first): %v", err)
	}
	got := 0
	for range ch {
		got++
	}
	if got == 0 {
		t.Skip("no multicast HELLO from zenohd-multicast within 1.5s — run `make interop-multicast-up` (NOT `interop-up`); the regular zenohd is bridge-networked and can't traverse the host's multicast group")
	}
}

// mcPyProc is a small wrapper around `docker compose exec -T python`
// that streams the script's stdout line-by-line. Mirrors the larger
// pyProc in interop_test.go but is contained to this build tag.
type mcPyProc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	lines  chan string
	errs   chan error
	done   chan struct{}
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func mcStartPython(t *testing.T, script string, args ...string) *mcPyProc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	full := append([]string{
		"compose", "-f", multicastComposeFile, "exec", "-T", "python",
		"python", "/work/" + script,
	}, args...)
	cmd := exec.CommandContext(ctx, "docker", full...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("docker compose exec python: %v", err)
	}
	p := &mcPyProc{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
		lines:  make(chan string, 64),
		errs:   make(chan error, 1),
		done:   make(chan struct{}),
		cancel: cancel,
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer close(p.lines)
		for {
			line, err := p.stdout.ReadString('\n')
			if line != "" {
				select {
				case p.lines <- strings.TrimRight(line, "\n"):
				case <-p.done:
					return
				}
			}
			if err != nil {
				p.errs <- err
				return
			}
		}
	}()
	return p
}

func (p *mcPyProc) waitFor(t *testing.T, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case line, ok := <-p.lines:
			if !ok {
				t.Fatalf("python stdout closed while waiting for %q", want)
			}
			if line == want {
				return
			}
			t.Logf("python > %s (waiting for %q)", line, want)
		case err := <-p.errs:
			if err != nil && !errors.Is(err, io.EOF) {
				t.Fatalf("python stdout error: %v", err)
			}
		case <-deadline:
			t.Fatalf("timeout waiting for %q from python", want)
		}
	}
}

func (p *mcPyProc) readLine(t *testing.T, timeout time.Duration) string {
	t.Helper()
	select {
	case line, ok := <-p.lines:
		if !ok {
			t.Fatal("python stdout closed unexpectedly")
		}
		return line
	case err := <-p.errs:
		t.Fatalf("python stdout error: %v", err)
	case <-time.After(timeout):
		t.Fatalf("timeout reading next python line")
	}
	return ""
}

func (p *mcPyProc) writeGo(t *testing.T) {
	t.Helper()
	if _, err := io.WriteString(p.stdin, mcGo+"\n"); err != nil {
		t.Fatalf("write GO: %v", err)
	}
}

func (p *mcPyProc) close(t *testing.T) {
	t.Helper()
	close(p.done)
	p.cancel()
	_ = p.stdin.Close()
	p.wg.Wait()
}

// mcDecodeSampleRecord parses one stdout JSON record emitted by
// python_common.sample_to_json — same schema as the existing peer
// scripts, just delivered over the multicast harness.
func mcDecodeSampleRecord(t *testing.T, line string) (key string, payload []byte) {
	t.Helper()
	var rec struct {
		Key     string `json:"key"`
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("decode python record %q: %v", line, err)
	}
	b, err := base64.StdEncoding.DecodeString(rec.Payload)
	if err != nil {
		t.Fatalf("decode payload base64: %v", err)
	}
	return rec.Key, b
}

// TestMulticastPubGoSubPython: Go peer publishes on the multicast
// group; a Python peer in the same group subscribes and reports each
// sample.
//
// Skipped pending follow-up: the rust multicast transport (used by
// eclipse-zenoh Python) accepts our JOIN, decodes our FRAME + 3
// PUSHes, and traces "recv Push ... payload: g2p-N" three times at
// zenoh::api::session level — but the user-side subscriber callback
// never fires. The reverse direction (Python publishes, Go receives,
// see TestMulticastPubPythonSubGo below) works, which is the test
// that proves our wire format is rust-readable both ways. The
// not-firing path appears to be inside the Python binding's
// callback-dispatch layer; revisit when zenoh-python ships an updated
// p2p_peer multicast routing path.
func TestMulticastPubGoSubPython(t *testing.T) {
	t.Skip("zenoh-python p2p_peer multicast: Push reaches session callback but not user-side subscriber; follow-up")
	mcRequireHarness(t)

	const (
		key   = "interop/mcast/g2p"
		count = 3
	)

	cfg := zenoh.Config{
		Mode:      zenoh.ModePeer,
		Endpoints: []string{mcGroup},
		// Disable SCOUT so it doesn't share the multicast group with
		// our peer transport. Rust's multicast transport at the same
		// group port treats every datagram as a transport message
		// (INIT/JOIN/FRAME); a SCOUT datagram (header 0x01) trips
		// rust's INIT decoder, fails, and closes the multicast
		// transport entirely. JOIN-driven discovery is sufficient for
		// peer-multicast pub/sub interop.
		Scouting: zenoh.ScoutingConfig{MulticastMode: zenoh.MulticastOff},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := zenoh.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("zenoh.Open(peer-multicast): %v", err)
	}
	defer session.Close()

	sub := mcStartPython(t, "python_multicast_peer_sub.py",
		"--key", key, "--count", fmt.Sprint(count))
	defer sub.close(t)
	sub.waitFor(t, mcReady, mcReadyTimeout)

	// Wait for the Python peer to appear in our multicast peer table —
	// JOIN convergence is what triggers our DECLARE re-flood.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(session.MulticastPeers()) >= 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(session.MulticastPeers()) == 0 {
		t.Fatal("Python multicast peer never appeared in our peer table")
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
	// Give D_SUBSCRIBER from Python and D_PUBLISHER-INTEREST from us
	// a moment to propagate through the multicast group.
	time.Sleep(300 * time.Millisecond)

	for i := 0; i < count; i++ {
		if err := pub.Put(zenoh.NewZBytesFromString(fmt.Sprintf("g2p-%d", i)), nil); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}

	for i := 0; i < count; i++ {
		line := sub.readLine(t, mcIOTimeout)
		if line == mcDone {
			t.Fatalf("done before %d samples (got %d)", count, i)
		}
		gotKey, payload := mcDecodeSampleRecord(t, line)
		if gotKey != key {
			t.Errorf("sample[%d] key = %q, want %q", i, gotKey, key)
		}
		want := fmt.Sprintf("g2p-%d", i)
		if string(payload) != want {
			t.Errorf("sample[%d] payload = %q, want %q", i, payload, want)
		}
	}
	sub.waitFor(t, mcDone, mcIOTimeout)
}

// TestMulticastPubPythonSubGo: a Python peer publishes on the
// multicast group; the Go peer subscribes and counts deliveries.
// Verifies our inbound dispatch is wire-compatible with rust output.
func TestMulticastPubPythonSubGo(t *testing.T) {
	mcRequireHarness(t)

	const (
		key   = "interop/mcast/p2g"
		count = 3
	)

	cfg := zenoh.Config{
		Mode:      zenoh.ModePeer,
		Endpoints: []string{mcGroup},
		// Disable SCOUT so it doesn't share the multicast group with
		// our peer transport. Rust's multicast transport at the same
		// group port treats every datagram as a transport message
		// (INIT/JOIN/FRAME); a SCOUT datagram (header 0x01) trips
		// rust's INIT decoder, fails, and closes the multicast
		// transport entirely. JOIN-driven discovery is sufficient for
		// peer-multicast pub/sub interop.
		Scouting: zenoh.ScoutingConfig{MulticastMode: zenoh.MulticastOff},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := zenoh.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("zenoh.Open(peer-multicast): %v", err)
	}
	defer session.Close()

	keGo, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}
	type recv struct {
		key string
		val string
	}
	ch := make(chan recv, count*2)
	subGo, err := session.DeclareSubscriber(keGo, zenoh.Closure[zenoh.Sample]{
		Call: func(s zenoh.Sample) {
			ch <- recv{key: s.KeyExpr().String(), val: string(s.Payload().Bytes())}
		},
	})
	if err != nil {
		t.Fatalf("DeclareSubscriber: %v", err)
	}
	defer subGo.Drop()

	pub := mcStartPython(t, "python_multicast_peer_pub.py",
		"--key", key, "--count", fmt.Sprint(count))
	defer pub.close(t)
	pub.waitFor(t, mcReady, mcReadyTimeout)

	// Wait for JOIN to propagate; the Python peer's view of our
	// D_SUBSCRIBER drives whether it actually publishes through.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(session.MulticastPeers()) >= 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(session.MulticastPeers()) == 0 {
		t.Fatal("Python multicast peer never appeared in our peer table")
	}
	time.Sleep(300 * time.Millisecond)

	pub.writeGo(t)

	got := 0
	deadline = time.Now().Add(mcIOTimeout)
	for got < count {
		select {
		case r := <-ch:
			if r.key != key {
				t.Errorf("sample key = %q, want %q", r.key, key)
			}
			want := fmt.Sprintf("mpeer-%d", got)
			if r.val != want {
				t.Errorf("sample[%d] = %q, want %q", got, r.val, want)
			}
			got++
		case <-time.After(time.Until(deadline)):
			t.Fatalf("only %d/%d samples received from Python publisher", got, count)
		}
	}
	pub.waitFor(t, mcDone, mcIOTimeout)
}
