//go:build interop

package interop

import (
	"slices"
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/zenoh"
)

// TestLivelinessHistoryReplay declares a Python token BEFORE the Go
// subscriber exists, then declares the subscriber with History=true.
// The router must replay the alive token as a Put Sample even though
// the subscriber missed the original declare.
func TestLivelinessHistoryReplay(t *testing.T) {
	requireZenohd(t)

	const key = "interop/live/history"

	// Declare the token first, while no subscriber exists.
	tok := startPython(t, "python_liveliness_token.py", "--key", key)
	defer tok.close(t)
	tok.waitFor(t, readyMarker, readyTimeout)
	// Let the router register the token before the subscriber declares.
	time.Sleep(subPropagationDelay)

	session := openGoSession(t)
	defer session.Close()

	ke, _ := zenoh.NewKeyExpr(key)
	ch := make(chan zenoh.Sample, 4)
	sub, err := session.Liveliness().DeclareSubscriber(ke, zenoh.Closure[zenoh.Sample]{
		Call: func(s zenoh.Sample) { ch <- s },
	}, &zenoh.LivelinessSubscriberOptions{History: true})
	if err != nil {
		t.Fatalf("DeclareSubscriber: %v", err)
	}
	defer sub.Drop()

	select {
	case s := <-ch:
		if s.Kind() != zenoh.SampleKindPut {
			t.Errorf("history event kind=%v, want Put", s.Kind())
		}
		if s.KeyExpr().String() != key {
			t.Errorf("history event key=%q, want %q", s.KeyExpr(), key)
		}
	case <-time.After(ioTimeout):
		t.Fatal("no history Put event within ioTimeout; History=true did not replay pre-existing token")
	}

	// Tell Python to drop the token → the live subscriber must still
	// observe the Delete (proves the subscriber is wired for future
	// events after replay, not just history).
	tok.sendLine(t, "DROP")
	tok.waitFor(t, doneMarker, ioTimeout)
	select {
	case s := <-ch:
		if s.Kind() != zenoh.SampleKindDelete {
			t.Errorf("post-history event kind=%v, want Delete", s.Kind())
		}
	case <-time.After(ioTimeout):
		t.Fatal("no Delete event after token drop")
	}
}

// TestLivelinessGetTerminates asserts that the reply channel returned by
// Liveliness.Get closes on its own — the router sends DECLARE Final for
// the Current-mode interest, which the Go session translates into
// channel close. Without that terminator the caller would have no way
// to know "all alive tokens delivered".
func TestLivelinessGetTerminates(t *testing.T) {
	requireZenohd(t)

	const (
		tokenKey = "interop/live/get/alpha"
		getKey   = "interop/live/get/**"
	)

	// zenohd does not echo a session's own liveliness tokens back to
	// that session's Liveliness.Get; we need separate sessions for the
	// declarer and the asker.
	declarerSession := openGoSession(t)
	defer declarerSession.Close()

	tokKE, _ := zenoh.NewKeyExpr(tokenKey)
	tok, err := declarerSession.Liveliness().DeclareToken(tokKE, nil)
	if err != nil {
		t.Fatalf("DeclareToken: %v", err)
	}
	defer tok.Drop()
	time.Sleep(subPropagationDelay)

	asker := openGoSession(t)
	defer asker.Close()
	ke, _ := zenoh.NewKeyExpr(getKey)
	replies, err := asker.Liveliness().Get(ke, nil)
	if err != nil {
		t.Fatalf("Liveliness.Get: %v", err)
	}

	var got []string
	deadline := time.After(ioTimeout)
loop:
	for {
		select {
		case r, ok := <-replies:
			if !ok {
				break loop
			}
			got = append(got, r.KeyExpr().String())
		case <-deadline:
			t.Fatal("Liveliness.Get channel did not close within ioTimeout")
		}
	}

	if !slices.Contains(got, tokenKey) {
		t.Errorf("Liveliness.Get replied %v; missing %q", got, tokenKey)
	}
}

// TestLivelinessTokenDeclarerKill verifies that a hard process kill on
// the token declarer results in a Delete Sample at the Go subscriber,
// without waiting for the session lease to expire. The Python token
// script exits via os._exit on the KILL command, skipping zenoh's
// graceful undeclare — zenohd detects the dropped TCP connection and
// revokes the token on the router side.
func TestLivelinessTokenDeclarerKill(t *testing.T) {
	requireZenohd(t)

	const key = "interop/live/kill"

	session := openGoSession(t)
	defer session.Close()

	ke, _ := zenoh.NewKeyExpr(key)
	ch := make(chan zenoh.Sample, 4)
	sub, err := session.Liveliness().DeclareSubscriber(ke, zenoh.Closure[zenoh.Sample]{
		Call: func(s zenoh.Sample) { ch <- s },
	}, nil)
	if err != nil {
		t.Fatalf("DeclareSubscriber: %v", err)
	}
	defer sub.Drop()
	time.Sleep(subPropagationDelay)

	tok := startPython(t, "python_liveliness_token.py", "--key", key)
	defer tok.close(t)
	tok.waitFor(t, readyMarker, readyTimeout)

	// Alive event first.
	select {
	case s := <-ch:
		if s.Kind() != zenoh.SampleKindPut {
			t.Fatalf("pre-kill event kind=%v, want Put", s.Kind())
		}
	case <-time.After(ioTimeout):
		t.Fatal("no alive event from token declarer")
	}

	// Trigger abrupt os._exit inside the Python script. The TCP FIN/RST
	// reaches zenohd, which revokes the token far sooner than the 10s
	// session lease would expire.
	tok.sendLine(t, "KILL")

	select {
	case s := <-ch:
		if s.Kind() != zenoh.SampleKindDelete {
			t.Errorf("post-kill event kind=%v, want Delete", s.Kind())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no Delete event within 15s of declarer kill")
	}
}
