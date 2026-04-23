//go:build interop && fault

package interop

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/internal/testutil"
	"github.com/shirou/zenoh-go-client/zenoh"
)

// Fault-injection interop suite. Uses tests/docker-compose.fault.yml:
//
//   Go test --tcp/127.0.0.1:17447--> toxiproxy --tcp/zenohd:7447--> zenohd
//
// The Go side talks exclusively through the toxiproxy listener; each test
// creates and tears down its own named proxy so toxics do not leak
// between tests.

const (
	toxiproxyAPI  = "http://127.0.0.1:8474"
	faultEndpoint = "tcp/127.0.0.1:17447"
	// faultProxyListen is the toxiproxy-side bind spec; must match the
	// exposed host port in docker-compose.fault.yml.
	faultProxyListen   = "0.0.0.0:17447"
	faultProxyUpstream = "zenohd:7447"
	faultProxyName     = "zenohd_fault"
)

// requireFaultInfra skips the test unless both toxiproxy and the proxied
// zenohd path are reachable. Keeping this a Skipf rather than Fatalf lets
// developers run `go test ./...` without booting the fault compose.
func requireFaultInfra(t *testing.T) *testutil.Toxiproxy {
	t.Helper()
	tp := testutil.NewToxiproxy(toxiproxyAPI)
	if err := tp.Ping(); err != nil {
		t.Skipf("toxiproxy not reachable at %s (run `make interop-fault-up` first): %v", toxiproxyAPI, err)
	}
	return tp
}

// setupFaultProxy creates a fresh proxy named faultProxyName and registers
// a cleanup that deletes it. The caller gets the toxiproxy client for
// adding/removing toxics during the test.
func setupFaultProxy(t *testing.T) *testutil.Toxiproxy {
	t.Helper()
	tp := requireFaultInfra(t)
	if err := tp.CreateProxy(faultProxyName, faultProxyListen, faultProxyUpstream); err != nil {
		t.Fatalf("toxiproxy CreateProxy: %v", err)
	}
	t.Cleanup(func() {
		_ = tp.DeleteProxy(faultProxyName)
	})
	// Allow a brief moment for the listener socket to bind before the
	// Go dial lands. toxiproxy's own API returns before the listen is
	// accepting on some platforms.
	waitUntilReachable(t, "127.0.0.1:17447", 2*time.Second)
	return tp
}

// waitUntilReachable polls a TCP endpoint until it accepts a dial or the
// timeout fires. Used after SetEnabled(true) / CreateProxy to avoid a
// spurious connection-refused on the first session dial.
func waitUntilReachable(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("endpoint %s not reachable within %v", addr, timeout)
}

// openFaultSession opens a session against the toxiproxy listener with
// the fastReconnect tweak so tests observing a link drop don't have to
// wait for the default 1 s / 4 s backoff.
func openFaultSession(t *testing.T, extra ...func(*zenoh.Config)) *zenoh.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := zenoh.NewConfig().WithEndpoint(faultEndpoint)
	fastReconnect(&cfg)
	for _, f := range extra {
		f(&cfg)
	}
	s, err := zenoh.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("zenoh.Open(%s): %v", faultEndpoint, err)
	}
	return s
}

// TestFaultLatencyDelivery asserts that an added round-trip latency does
// not drop samples — a reliable publish burst lands on the subscriber
// within a window that scales with the configured delay.
func TestFaultLatencyDelivery(t *testing.T) {
	cases := []struct {
		label      string
		latencyMs  int
		count      int
		recvWindow time.Duration
	}{
		{"100ms", 100, 10, 5 * time.Second},
		{"1s", 1000, 5, 12 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			tp := setupFaultProxy(t)
			// Apply latency on both streams so SUBSCRIBER declarations as
			// well as PUSH samples see the delay.
			if err := tp.AddToxic(faultProxyName, testutil.Toxic{
				Name:       "latency_down",
				Type:       "latency",
				Stream:     "downstream",
				Toxicity:   1.0,
				Attributes: map[string]any{"latency": tc.latencyMs},
			}); err != nil {
				t.Fatalf("add downstream latency: %v", err)
			}
			if err := tp.AddToxic(faultProxyName, testutil.Toxic{
				Name:       "latency_up",
				Type:       "latency",
				Stream:     "upstream",
				Toxicity:   1.0,
				Attributes: map[string]any{"latency": tc.latencyMs},
			}); err != nil {
				t.Fatalf("add upstream latency: %v", err)
			}

			// Two independent sessions through the same proxy so both
			// publisher and subscriber share the impairment.
			pubSess := openFaultSession(t)
			defer pubSess.Close()
			subSess := openFaultSession(t)
			defer subSess.Close()

			key := fmt.Sprintf("interop/fault/latency/%s", tc.label)
			ke, _ := zenoh.NewKeyExpr(key)

			var (
				mu      sync.Mutex
				samples []string
			)
			sub, err := subSess.DeclareSubscriber(ke, zenoh.Closure[zenoh.Sample]{
				Call: func(s zenoh.Sample) {
					mu.Lock()
					samples = append(samples, string(s.Payload().Bytes()))
					mu.Unlock()
				},
			})
			if err != nil {
				t.Fatalf("DeclareSubscriber: %v", err)
			}
			defer sub.Drop()

			// Wait at least one RTT + router propagation before PUSH.
			time.Sleep(subPropagationDelay + time.Duration(tc.latencyMs*3)*time.Millisecond)

			pub, err := pubSess.DeclarePublisher(ke, nil)
			if err != nil {
				t.Fatalf("DeclarePublisher: %v", err)
			}
			defer pub.Drop()

			for i := 0; i < tc.count; i++ {
				if err := pub.Put(zenoh.NewZBytesFromString(fmt.Sprintf("m-%d", i)), nil); err != nil {
					t.Fatalf("pub.Put[%d]: %v", i, err)
				}
			}

			deadline := time.Now().Add(tc.recvWindow)
			for time.Now().Before(deadline) {
				mu.Lock()
				n := len(samples)
				mu.Unlock()
				if n >= tc.count {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			mu.Lock()
			got := append([]string(nil), samples...)
			mu.Unlock()
			if len(got) < tc.count {
				t.Errorf("received %d/%d samples within %v under %s latency (samples=%v)",
					len(got), tc.count, tc.recvWindow, tc.label, got)
			}
		})
	}
}

// TestFaultBandwidthLimit bounds the link to 1 Mbps (= 125 KB/s) and
// publishes a payload several times that cap. The assertion is that the
// whole payload arrives — not that it arrives in exactly one RTT — so
// the test proves no bytes are dropped under backpressure.
func TestFaultBandwidthLimit(t *testing.T) {
	tp := setupFaultProxy(t)
	// 128 KB/s is close enough to 1 Mbps that a 256 KB payload completes
	// under the 15 s ioTimeout even when router / encoding overhead is
	// added on top.
	if err := tp.AddToxic(faultProxyName, testutil.Toxic{
		Name:       "bw_up",
		Type:       "bandwidth",
		Stream:     "upstream",
		Toxicity:   1.0,
		Attributes: map[string]any{"rate": 128},
	}); err != nil {
		t.Fatalf("add upstream bandwidth: %v", err)
	}
	if err := tp.AddToxic(faultProxyName, testutil.Toxic{
		Name:       "bw_down",
		Type:       "bandwidth",
		Stream:     "downstream",
		Toxicity:   1.0,
		Attributes: map[string]any{"rate": 128},
	}); err != nil {
		t.Fatalf("add downstream bandwidth: %v", err)
	}

	pubSess := openFaultSession(t)
	defer pubSess.Close()
	subSess := openFaultSession(t)
	defer subSess.Close()

	const (
		key         = "interop/fault/bandwidth"
		payloadSize = 256 * 1024
	)
	ke, _ := zenoh.NewKeyExpr(key)

	payload := make([]byte, payloadSize)
	for i := range payload {
		payload[i] = byte(i)
	}
	done := make(chan int, 1)
	sub, err := subSess.DeclareSubscriber(ke, zenoh.Closure[zenoh.Sample]{
		Call: func(s zenoh.Sample) {
			done <- len(s.Payload().Bytes())
		},
	})
	if err != nil {
		t.Fatalf("DeclareSubscriber: %v", err)
	}
	defer sub.Drop()

	time.Sleep(subPropagationDelay)

	pub, err := pubSess.DeclarePublisher(ke, nil)
	if err != nil {
		t.Fatalf("DeclarePublisher: %v", err)
	}
	defer pub.Drop()

	start := time.Now()
	if err := pub.Put(zenoh.NewZBytes(payload), nil); err != nil {
		t.Fatalf("pub.Put: %v", err)
	}

	select {
	case got := <-done:
		if got != payloadSize {
			t.Errorf("payload size = %d, want %d", got, payloadSize)
		}
		// Lower-bound sanity: 256 KB at 128 KB/s is at least ~1 s each
		// way through the proxy. The upper bound is the 15 s timeout.
		if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
			t.Logf("note: received in %v, bandwidth cap may not have applied", elapsed)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no sample within 15s under 1 Mbps cap")
	}
}

// TestFaultHalfOpenSocketTriggersReconnect exercises the half-open
// contract: once the peer has stopped feeding bytes while the socket
// stays formally open, the lease watchdog must notice and hand
// control to the reconnect orchestrator.
//
// toxiproxy's `timeout` toxic with `timeout=0` stops data in both
// directions and keeps the underlying TCP connection open — which is
// exactly the half-open shape the watchdog is supposed to detect.
func TestFaultHalfOpenSocketTriggersReconnect(t *testing.T) {
	tp := setupFaultProxy(t)

	session := openFaultSession(t)
	defer session.Close()

	// Prime the connection with one successful Put so the reader has
	// at least one lastRecv timestamp before the gag arrives.
	ke, _ := zenoh.NewKeyExpr("interop/fault/halfopen/warmup")
	if err := session.Put(ke, zenoh.NewZBytesFromString("warmup"), nil); err != nil {
		t.Fatalf("warmup Put: %v", err)
	}

	// Gag the connection: data stops flowing, socket stays half-open.
	if err := tp.AddToxic(faultProxyName, testutil.Toxic{
		Name:       "halfopen_down",
		Type:       "timeout",
		Stream:     "downstream",
		Toxicity:   1.0,
		Attributes: map[string]any{"timeout": 0},
	}); err != nil {
		t.Fatalf("add halfopen downstream: %v", err)
	}
	if err := tp.AddToxic(faultProxyName, testutil.Toxic{
		Name:       "halfopen_up",
		Type:       "timeout",
		Stream:     "upstream",
		Toxicity:   1.0,
		Attributes: map[string]any{"timeout": 0},
	}); err != nil {
		t.Fatalf("add halfopen upstream: %v", err)
	}

	// The session's negotiated lease is 10 s; the watchdog fires at
	// (lease + a check tick). Leave a generous margin before clearing
	// the toxic so we know the reconnect path actually triggered.
	time.Sleep(12 * time.Second)
	if err := tp.RemoveToxic(faultProxyName, "halfopen_down"); err != nil {
		t.Fatalf("remove halfopen downstream: %v", err)
	}
	if err := tp.RemoveToxic(faultProxyName, "halfopen_up"); err != nil {
		t.Fatalf("remove halfopen upstream: %v", err)
	}

	// Post-recovery: the reconnect orchestrator should have produced a
	// fresh Runtime that accepts Puts again. waitForSessionReady
	// probes until it succeeds.
	waitForSessionReady(t, session, 10*time.Second)
	if err := session.Put(ke, zenoh.NewZBytesFromString("after"), nil); err != nil {
		t.Fatalf("post-recovery Put: %v", err)
	}
}

// TestFaultShortDisconnectRecovers pushes the router off the wire
// briefly (1 s) via toxiproxy's enabled flag, then verifies that the
// session reconnects and continues to publish.
func TestFaultShortDisconnectRecovers(t *testing.T) {
	tp := setupFaultProxy(t)

	session := openFaultSession(t)
	defer session.Close()

	ke, _ := zenoh.NewKeyExpr("interop/fault/short_unreachable")
	if err := session.Put(ke, zenoh.NewZBytesFromString("before"), nil); err != nil {
		t.Fatalf("warmup Put: %v", err)
	}

	if err := tp.SetEnabled(faultProxyName, false); err != nil {
		t.Fatalf("disable proxy: %v", err)
	}
	time.Sleep(1 * time.Second)
	if err := tp.SetEnabled(faultProxyName, true); err != nil {
		t.Fatalf("re-enable proxy: %v", err)
	}
	waitUntilReachable(t, "127.0.0.1:17447", 2*time.Second)

	waitForSessionReady(t, session, 10*time.Second)
	if err := session.Put(ke, zenoh.NewZBytesFromString("after"), nil); err != nil {
		t.Fatalf("post-reconnect Put: %v", err)
	}
}

// TestFaultLongDisconnectRecovers keeps the proxy down long enough for
// the reconnect orchestrator's backoff to saturate at its cap. The test
// confirms that the orchestrator keeps trying past the cap and recovers
// once the path comes back.
func TestFaultLongDisconnectRecovers(t *testing.T) {
	tp := setupFaultProxy(t)

	// Pin the backoff cap to 500 ms so a handful of attempts fit in the
	// downtime window without dragging the test out.
	session := openFaultSession(t)
	defer session.Close()

	ke, _ := zenoh.NewKeyExpr("interop/fault/long_unreachable")
	if err := session.Put(ke, zenoh.NewZBytesFromString("before"), nil); err != nil {
		t.Fatalf("warmup Put: %v", err)
	}

	if err := tp.SetEnabled(faultProxyName, false); err != nil {
		t.Fatalf("disable proxy: %v", err)
	}
	// 8 s is several multiples of the 500 ms backoff cap — long enough
	// that the orchestrator has stopped ramping and is just retrying at
	// the cap interval.
	time.Sleep(8 * time.Second)
	if err := tp.SetEnabled(faultProxyName, true); err != nil {
		t.Fatalf("re-enable proxy: %v", err)
	}
	waitUntilReachable(t, "127.0.0.1:17447", 2*time.Second)

	waitForSessionReady(t, session, 10*time.Second)
	if err := session.Put(ke, zenoh.NewZBytesFromString("after"), nil); err != nil {
		t.Fatalf("post-long-outage Put: %v", err)
	}
}
