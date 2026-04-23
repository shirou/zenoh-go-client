//go:build interop && stress

package stress

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/zenoh"
)

// TestScaleDeclarePublishers declares 100 / 1000 publishers on the same
// session and records declare latency + heap delta. No hard threshold —
// the test's value is catching regressions that break the operation
// entirely (timeouts, OOM) or by orders of magnitude.
func TestScaleDeclarePublishers(t *testing.T) {
	requireZenohd(t)
	benchDeclare(t, "pub",
		func(session *zenoh.Session, i int) (dropper, error) {
			ke, err := zenoh.NewKeyExpr(fmt.Sprintf("scale/pub/%d", i))
			if err != nil {
				return nil, err
			}
			p, err := session.DeclarePublisher(ke, nil)
			if err != nil {
				return nil, err
			}
			return p, nil
		})
}

// TestScaleDeclareSubscribers mirrors TestScaleDeclarePublishers for
// Subscribers. Handler is a no-op closure; we're measuring declare, not
// delivery.
func TestScaleDeclareSubscribers(t *testing.T) {
	requireZenohd(t)
	benchDeclare(t, "sub",
		func(session *zenoh.Session, i int) (dropper, error) {
			ke, err := zenoh.NewKeyExpr(fmt.Sprintf("scale/sub/%d", i))
			if err != nil {
				return nil, err
			}
			s, err := session.DeclareSubscriber(ke, zenoh.Closure[zenoh.Sample]{
				Call: func(zenoh.Sample) {},
			})
			if err != nil {
				return nil, err
			}
			return s, nil
		})
}

// dropper is the shared suffix of the Publisher / Subscriber (and other
// declare-return) API — just enough for benchDeclare to clean up without
// caring about the concrete type.
type dropper interface{ Drop() }

// benchDeclare declares n=100 and n=1000 entities of the same kind and
// records declare latency + heap delta per entity. Assertions are
// intentionally loose (log-only); the intent is to catch >order-of-
// magnitude regressions, not enforce a SLA.
func benchDeclare(t *testing.T, noun string, decl func(*zenoh.Session, int) (dropper, error)) {
	t.Helper()
	for _, n := range []int{100, 1000} {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			session := openSession(t)
			defer session.Close()

			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)

			entities := make([]dropper, n)
			start := time.Now()
			for i := 0; i < n; i++ {
				d, err := decl(session, i)
				if err != nil {
					t.Fatalf("declare[%d]: %v", i, err)
				}
				entities[i] = d
			}
			declareDur := time.Since(start)

			runtime.GC()
			runtime.ReadMemStats(&after)
			delta := int64(after.HeapAlloc) - int64(before.HeapAlloc)

			t.Logf("%d %ss: declare total=%v, avg=%v/%s, heap delta=%d KiB (%d B/%s)",
				n, noun, declareDur, declareDur/time.Duration(n), noun,
				delta/1024, delta/int64(n), noun)

			for _, d := range entities {
				d.Drop()
			}
		})
	}
}

// TestScaleCrossPublish spins up 100 publishers on distinct keys and 100
// subscribers, each subscribing to the common wildcard. Every publish
// should fan out to all subscribers, so expected deliveries =
// nPublishers × nMessagesPerPub × nSubscribers. Loss rate is logged and
// a regression-catching upper bound (10 %) is enforced — local zenohd
// under CongestionControl=Block is expected to deliver all samples, but
// the loose bound accommodates GitHub runner variance.
func TestScaleCrossPublish(t *testing.T) {
	requireZenohd(t)

	const (
		nPublishers     = 100
		nSubscribers    = 100
		nMessagesPerPub = 10
	)

	subSession := openSession(t)
	defer subSession.Close()
	pubSession := openSession(t)
	defer pubSession.Close()

	wildKE, err := zenoh.NewKeyExpr("scale/cross/**")
	if err != nil {
		t.Fatalf("NewKeyExpr wild: %v", err)
	}

	expected := int64(nPublishers * nSubscribers * nMessagesPerPub)
	var received int64
	done := make(chan struct{})
	var doneOnce sync.Once
	subs := make([]*zenoh.Subscriber, nSubscribers)
	for i := 0; i < nSubscribers; i++ {
		sub, err := subSession.DeclareSubscriber(wildKE, zenoh.Closure[zenoh.Sample]{
			Call: func(zenoh.Sample) {
				if atomic.AddInt64(&received, 1) == expected {
					doneOnce.Do(func() { close(done) })
				}
			},
		})
		if err != nil {
			t.Fatalf("DeclareSubscriber[%d]: %v", i, err)
		}
		subs[i] = sub
	}
	t.Cleanup(func() {
		for _, s := range subs {
			s.Drop()
		}
	})

	// 100 D_SUBSCRIBER messages need a longer breather than the single-sub case.
	time.Sleep(subPropagationDelay * 5)

	pubs := make([]*zenoh.Publisher, nPublishers)
	for i := 0; i < nPublishers; i++ {
		ke, err := zenoh.NewKeyExpr(fmt.Sprintf("scale/cross/%d", i))
		if err != nil {
			t.Fatalf("NewKeyExpr[%d]: %v", i, err)
		}
		p, err := pubSession.DeclarePublisher(ke, nil)
		if err != nil {
			t.Fatalf("DeclarePublisher[%d]: %v", i, err)
		}
		pubs[i] = p
	}
	t.Cleanup(func() {
		for _, p := range pubs {
			p.Drop()
		}
	})

	time.Sleep(subPropagationDelay)

	block := &zenoh.PutOptions{
		CongestionControl: zenoh.CongestionControlBlock,
		HasCongestion:     true,
	}
	payload := zenoh.NewZBytesFromString("cross")

	var putErrs atomic.Int64
	var wg sync.WaitGroup
	wg.Add(nPublishers)
	start := time.Now()
	for _, p := range pubs {
		go func(p *zenoh.Publisher) {
			defer wg.Done()
			for m := 0; m < nMessagesPerPub; m++ {
				if err := p.Put(payload, block); err != nil {
					putErrs.Add(1)
					return
				}
			}
		}(p)
	}
	wg.Wait()
	sendDur := time.Since(start)
	if n := putErrs.Load(); n > 0 {
		t.Errorf("%d publisher Put errors (see warn log)", n)
	}

	select {
	case <-done:
	case <-time.After(60 * time.Second):
	}
	got := atomic.LoadInt64(&received)
	loss := float64(expected-got) / float64(expected)
	t.Logf("cross publish: sent %d msgs in %v, received %d/%d deliveries, loss=%.4f",
		nPublishers*nMessagesPerPub, sendDur, got, expected, loss)
	if loss > 0.10 {
		t.Errorf("loss rate %.4f exceeds 10%% (missing %d of %d)", loss, expected-got, expected)
	}
}

// TestScaleSparseSubscription verifies that the router filters
// non-matching pushes before they reach the Go session. A narrow
// subscriber on a single key must never see samples published on
// unrelated keys, even under a sustained noise load.
//
// Ordering matters: we send the target batch first so the assertion
// signal is captured on a healthy link. The noise burst follows; noise
// Puts tolerate ErrConnectionLost because zenohd 1.0.0's routing-thread
// panic on certain teardown patterns can drop the link mid-burst. The
// filter-correctness check (noiseHits==0) still holds even if the
// router survives only long enough to process part of the noise.
func TestScaleSparseSubscription(t *testing.T) {
	requireZenohd(t)

	const (
		targetKey    = "scale/sparse/target"
		noiseKeyFmt  = "scale/sparse/noise/%d"
		nNoisePubs   = 10
		nNoisePerPub = 100
		nTargetPuts  = 5
		targetWindow = 5 * time.Second
		noiseSettle  = 500 * time.Millisecond
	)

	subSession := openSession(t)
	defer subSession.Close()
	pubSession := openSession(t)
	defer pubSession.Close()

	targetKE, err := zenoh.NewKeyExpr(targetKey)
	if err != nil {
		t.Fatalf("NewKeyExpr target: %v", err)
	}

	var targetHits, noiseHits int64
	sub, err := subSession.DeclareSubscriber(targetKE, zenoh.Closure[zenoh.Sample]{
		Call: func(s zenoh.Sample) {
			if s.KeyExpr().String() == targetKey {
				atomic.AddInt64(&targetHits, 1)
			} else {
				atomic.AddInt64(&noiseHits, 1)
			}
		},
	})
	if err != nil {
		t.Fatalf("DeclareSubscriber: %v", err)
	}
	defer sub.Drop()

	time.Sleep(subPropagationDelay)

	block := &zenoh.PutOptions{
		CongestionControl: zenoh.CongestionControlBlock,
		HasCongestion:     true,
	}
	payload := zenoh.NewZBytesFromString("x")

	for m := 0; m < nTargetPuts; m++ {
		if err := pubSession.Put(targetKE, payload, block); err != nil {
			t.Fatalf("target Put[%d] on healthy link: %v", m, err)
		}
	}

	deadline := time.Now().Add(targetWindow)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&targetHits) >= nTargetPuts {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	var noiseSent int64
	var wg sync.WaitGroup
	wg.Add(nNoisePubs)
	for i := 0; i < nNoisePubs; i++ {
		go func(i int) {
			defer wg.Done()
			ke, err := zenoh.NewKeyExpr(fmt.Sprintf(noiseKeyFmt, i))
			if err != nil {
				return
			}
			for m := 0; m < nNoisePerPub; m++ {
				if err := pubSession.Put(ke, payload, block); err == nil {
					atomic.AddInt64(&noiseSent, 1)
				}
			}
		}(i)
	}
	wg.Wait()

	// Let any in-flight noise sample finish its routing round-trip before
	// we freeze noiseHits.
	time.Sleep(noiseSettle)

	tHits := atomic.LoadInt64(&targetHits)
	nHits := atomic.LoadInt64(&noiseHits)
	sent := atomic.LoadInt64(&noiseSent)
	t.Logf("sparse: target hits=%d (expected %d), noise leaks=%d (noise accepted=%d of %d)",
		tHits, nTargetPuts, nHits, sent, nNoisePubs*nNoisePerPub)
	if nHits != 0 {
		t.Errorf("router delivered %d non-matching samples to narrow subscriber", nHits)
	}
	if tHits < nTargetPuts {
		t.Errorf("received %d/%d target samples", tHits, nTargetPuts)
	}
}

// TestScaleHighFrequencyThroughput drives a tight single-pub / single-sub
// loop with 10k messages to log the observed send + end-to-end throughput.
// Baseline measurement only — no pass/fail threshold beyond "all messages
// arrive within the grace period".
func TestScaleHighFrequencyThroughput(t *testing.T) {
	requireZenohd(t)

	const (
		key       = "scale/highfreq"
		nMessages = 10_000
	)

	subSession := openSession(t)
	defer subSession.Close()
	pubSession := openSession(t)
	defer pubSession.Close()

	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}

	var received int64
	done := make(chan struct{})
	var once sync.Once
	sub, err := subSession.DeclareSubscriber(ke, zenoh.Closure[zenoh.Sample]{
		Call: func(zenoh.Sample) {
			if atomic.AddInt64(&received, 1) == nMessages {
				once.Do(func() { close(done) })
			}
		},
	})
	if err != nil {
		t.Fatalf("DeclareSubscriber: %v", err)
	}
	defer sub.Drop()

	pub, err := pubSession.DeclarePublisher(ke, nil)
	if err != nil {
		t.Fatalf("DeclarePublisher: %v", err)
	}
	defer pub.Drop()

	time.Sleep(subPropagationDelay)

	block := &zenoh.PutOptions{
		CongestionControl: zenoh.CongestionControlBlock,
		HasCongestion:     true,
	}
	payload := zenoh.NewZBytesFromString("msg")

	start := time.Now()
	for i := 0; i < nMessages; i++ {
		if err := pub.Put(payload, block); err != nil {
			t.Fatalf("Put[%d]: %v", i, err)
		}
	}
	sendDur := time.Since(start)

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("received %d/%d within 30s", atomic.LoadInt64(&received), nMessages)
	}
	e2eDur := time.Since(start)

	t.Logf("high-frequency: n=%d send=%v (%.0f msg/s) e2e=%v (%.0f msg/s)",
		nMessages,
		sendDur, float64(nMessages)/sendDur.Seconds(),
		e2eDur, float64(nMessages)/e2eDur.Seconds())
}
