//go:build interop

package interop

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/zenoh"
)

// fastReconnect tunes a session config for tests that want to observe
// a zenohd restart promptly — without this the default 1s / 4s backoff
// eats the test's budget.
func fastReconnect(c *zenoh.Config) {
	c.ReconnectInitial = 100 * time.Millisecond
	c.ReconnectMax = 500 * time.Millisecond
}

// TestReconnectRedeclaresAndResumes checks the full reconnect contract
// against a real zenohd: after a router restart the orchestrator must
// dial again, re-declare the live Subscriber / Queryable / Publisher /
// LivelinessToken, and resume data delivery.
func TestReconnectRedeclaresAndResumes(t *testing.T) {
	requireZenohd(t)

	const key = "interop/reconnect/payload"

	subSession := openGoSession(t, fastReconnect)
	defer subSession.Close()

	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}

	var (
		mu      sync.Mutex
		samples []string
	)
	sub, err := subSession.DeclareSubscriber(ke, zenoh.Closure[zenoh.Sample]{
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

	const tokenKey = "interop/reconnect/token"
	tokKE, _ := zenoh.NewKeyExpr(tokenKey)
	tok, err := subSession.Liveliness().DeclareToken(tokKE, nil)
	if err != nil {
		t.Fatalf("DeclareToken: %v", err)
	}
	defer tok.Drop()

	qbl, err := subSession.DeclareQueryable(ke, func(q *zenoh.Query) {
		_ = q.Reply(ke, zenoh.NewZBytesFromString("qbl-reply"), nil)
	}, nil)
	if err != nil {
		t.Fatalf("DeclareQueryable: %v", err)
	}
	defer qbl.Drop()

	pubSession := openGoSession(t, fastReconnect)
	defer pubSession.Close()
	pub, err := pubSession.DeclarePublisher(ke, nil)
	if err != nil {
		t.Fatalf("DeclarePublisher: %v", err)
	}
	defer pub.Drop()

	// Before-restart data path.
	time.Sleep(subPropagationDelay)
	if err := pub.Put(zenoh.NewZBytesFromString("before"), nil); err != nil {
		t.Fatalf("pre-restart Put: %v", err)
	}
	waitForSample(t, &mu, &samples, "before", 3*time.Second)

	restartZenohd(t)

	// After-restart: both sessions must have re-dialed and replayed
	// entities before a fresh Put reaches the subscriber.
	waitForSessionReady(t, pubSession, 10*time.Second)
	waitForSessionReady(t, subSession, 10*time.Second)
	time.Sleep(subPropagationDelay)

	if err := pub.Put(zenoh.NewZBytesFromString("after"), nil); err != nil {
		t.Fatalf("post-restart Put: %v", err)
	}
	waitForSample(t, &mu, &samples, "after", 5*time.Second)

	// Queryable survived: a Get from the publisher session reaches the
	// re-declared Queryable on the subscriber session.
	replies, err := pubSession.Get(ke, nil)
	if err != nil {
		t.Fatalf("post-restart Get: %v", err)
	}
	got := collectGetPayloads(t, replies, 5*time.Second)
	if !slices.Contains(got, "qbl-reply") {
		t.Errorf("post-restart Queryable did not reply; got %v", got)
	}
}

// TestReconnectPutDuringDisconnectErrors asserts the contract from
// docs/api.md §1.3: a Put issued while the session has no live link
// returns a known sentinel error (ErrSessionNotReady or ErrConnectionLost)
// — it neither blocks nor silently drops.
//
// Both sentinels are accepted because which one fires depends on where
// the Put enters the send path relative to the reconnect orchestrator:
//
//   - ErrConnectionLost fires when the Put reaches enqueueNetwork with the
//     old Runtime still stored but its LinkClosed channel already signalled.
//   - ErrSessionNotReady is reserved for a Runtime slot that has been
//     cleared (future API: reconnect currently keeps the pointer populated
//     across the hole, but both sentinels are part of the published contract).
func TestReconnectPutDuringDisconnectErrors(t *testing.T) {
	requireZenohd(t)

	const key = "interop/reconnect/during"

	session := openGoSession(t, fastReconnect)
	defer session.Close()

	ke, _ := zenoh.NewKeyExpr(key)
	if err := session.Put(ke, zenoh.NewZBytesFromString("warmup"), nil); err != nil {
		t.Fatalf("warmup Put: %v", err)
	}

	var (
		errMu        sync.Mutex
		observedErrs []error
		wg           sync.WaitGroup
	)
	stop := make(chan struct{})
	var stopOnce sync.Once
	stopProber := func() {
		stopOnce.Do(func() { close(stop) })
		wg.Wait()
	}
	// Register teardown up front so a t.Fatal inside restartZenohd or
	// waitForSessionReady doesn't leave the prober spinning forever.
	t.Cleanup(stopProber)
	wg.Add(1)
	go func() {
		defer wg.Done()
		payload := zenoh.NewZBytesFromString("probe")
		for {
			select {
			case <-stop:
				return
			default:
			}
			err := session.Put(ke, payload, nil)
			if err != nil {
				errMu.Lock()
				// Cap to avoid unbounded growth on a very long hang; once
				// we have a handful of samples the assertions can't learn
				// anything new from further entries.
				if len(observedErrs) < 128 {
					observedErrs = append(observedErrs, err)
				}
				errMu.Unlock()
			}
			time.Sleep(time.Millisecond)
		}
	}()

	restartZenohd(t)
	waitForSessionReady(t, session, 10*time.Second)
	stopProber()

	errMu.Lock()
	errs := slices.Clone(observedErrs)
	errMu.Unlock()

	if len(errs) == 0 {
		t.Error("no Put returned an error across the restart window; " +
			"expected at least one ErrSessionNotReady or ErrConnectionLost")
	}
	for _, err := range errs {
		if !errors.Is(err, zenoh.ErrSessionNotReady) && !errors.Is(err, zenoh.ErrConnectionLost) {
			t.Errorf("unexpected Put error: %v (want ErrSessionNotReady or ErrConnectionLost)", err)
		}
	}

	// Session is usable again after the reconnect settles.
	if err := session.Put(ke, zenoh.NewZBytesFromString("final"), nil); err != nil {
		t.Fatalf("post-reconnect Put: %v", err)
	}
}

// waitForSessionReady spins Session.Put against a throwaway key until it
// succeeds or the deadline fires. The reconnect orchestrator surfaces
// "ready again" indirectly — no public IsReady() probe — so a cheap Put
// is the idiomatic wait.
func waitForSessionReady(t *testing.T, s *zenoh.Session, timeout time.Duration) {
	t.Helper()
	ke, _ := zenoh.NewKeyExpr(fmt.Sprintf("interop/_probe/%d", time.Now().UnixNano()))
	payload := zenoh.NewZBytesFromString("probe")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := s.Put(ke, payload, nil); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("session not ready within %v", timeout)
}

// waitForSample blocks until the guarded slice contains want, or fails
// on timeout. The subscriber handler may be called from another
// goroutine, so the slice is walked under mu.
func waitForSample(t *testing.T, mu *sync.Mutex, samples *[]string, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, s := range *samples {
			if s == want {
				mu.Unlock()
				return
			}
		}
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	got := slices.Clone(*samples)
	mu.Unlock()
	t.Fatalf("sample %q not seen within %v; got %v", want, timeout, got)
}
