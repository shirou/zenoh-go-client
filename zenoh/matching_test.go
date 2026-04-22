package zenoh

import (
	"context"
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/internal/wire"
)

// injectEntityDeclare emits a D_SUBSCRIBER / D_QUERYABLE / D_TOKEN. When
// interestID != 0 the DECLARE carries it (current-snapshot path);
// otherwise it's the passive fanout path.
func (m *mockRouter) injectEntityDeclare(t *testing.T, kind byte, interestID, entityID uint32, keyExpr string) {
	t.Helper()
	m.inject(t, &wire.Declare{
		InterestID:    interestID,
		HasInterestID: interestID != 0,
		Body: &wire.DeclareEntity{
			Kind:     kind,
			EntityID: entityID,
			KeyExpr:  wire.WireExpr{Scope: 0, Suffix: keyExpr},
		},
	})
}

// injectEntityUndeclare emits a U_SUBSCRIBER / U_QUERYABLE / U_TOKEN.
func (m *mockRouter) injectEntityUndeclare(t *testing.T, kind byte, entityID uint32, keyExpr string) {
	t.Helper()
	m.inject(t, &wire.Declare{
		Body: &wire.UndeclareEntity{
			Kind:     kind,
			EntityID: entityID,
			WireExpr: wire.WireExpr{Scope: 0, Suffix: keyExpr},
		},
	})
}

// waitMatching receives the next MatchingStatus on ch or fails the test.
func waitMatching(t *testing.T, ch <-chan MatchingStatus, want bool) {
	t.Helper()
	select {
	case got := <-ch:
		if got.Matching != want {
			t.Fatalf("MatchingStatus = %v, want Matching=%v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for Matching=%v", want)
	}
}

func TestPublisherInterestSentOnDeclare(t *testing.T) {
	router := newMockRouter(t)
	defer router.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cfg := NewConfig().WithEndpoint("tcp/" + router.Addr())
	sess, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	ke, _ := NewKeyExpr("demo/foo")
	pub, err := sess.DeclarePublisher(ke, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Drop()

	select {
	case obs := <-router.interests:
		if obs.mode != wire.InterestModeCurrentFuture {
			t.Errorf("mode = %v, want CurrentFuture", obs.mode)
		}
		if !obs.filter.Subscribers || !obs.filter.KeyExprs {
			t.Errorf("filter = %+v, want K+S", obs.filter)
		}
		if obs.key != "demo/foo" {
			t.Errorf("restricted key = %q, want demo/foo", obs.key)
		}
	case <-time.After(time.Second):
		t.Fatal("did not observe publisher INTEREST")
	}
}

func TestPublisherInterestFinalOnDrop(t *testing.T) {
	router := newMockRouter(t)
	defer router.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cfg := NewConfig().WithEndpoint("tcp/" + router.Addr())
	sess, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	ke, _ := NewKeyExpr("demo/foo")
	pub, _ := sess.DeclarePublisher(ke, nil)

	// Drain the CurrentFuture interest.
	<-router.interests
	pub.Drop()

	select {
	case obs := <-router.interests:
		if !obs.final {
			t.Errorf("expected INTEREST Final, got %+v", obs)
		}
	case <-time.After(time.Second):
		t.Fatal("did not observe INTEREST Final")
	}
}

func TestMatchingListener_InitialSnapshotDelivery(t *testing.T) {
	router := newMockRouter(t)
	defer router.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cfg := NewConfig().WithEndpoint("tcp/" + router.Addr())
	sess, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	ke, _ := NewKeyExpr("demo/foo")
	pub, err := sess.DeclarePublisher(ke, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Drop()

	obs := <-router.interests
	fifo := NewFifoChannel[MatchingStatus](8)
	listener, err := pub.DeclareMatchingListener(fifo)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Drop()

	// Inject a current subscriber then DeclareFinal. The listener should
	// receive exactly one delivery with Matching=true.
	router.injectEntityDeclare(t, wire.IDDeclareSubscriber, obs.id, 11, "demo/foo")
	router.injectDeclareFinal(t, obs.id)

	waitMatching(t, listener.Handler(), true)

	// Passive undeclare → 1→0 transition fires Matching=false.
	router.injectEntityUndeclare(t, wire.IDUndeclareSubscriber, 11, "demo/foo")
	waitMatching(t, listener.Handler(), false)
}

func TestMatchingListener_NoCurrentThenArrival(t *testing.T) {
	router := newMockRouter(t)
	defer router.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cfg := NewConfig().WithEndpoint("tcp/" + router.Addr())
	sess, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	ke, _ := NewKeyExpr("demo/foo")
	pub, _ := sess.DeclarePublisher(ke, nil)
	defer pub.Drop()
	obs := <-router.interests

	listener, _ := pub.DeclareMatchingListener(NewFifoChannel[MatchingStatus](8))
	defer listener.Drop()

	// Empty snapshot → first delivery is Matching=false.
	router.injectDeclareFinal(t, obs.id)
	waitMatching(t, listener.Handler(), false)

	// Fresh D_SUBSCRIBER arrives → 0→1 fires Matching=true.
	router.injectEntityDeclare(t, wire.IDDeclareSubscriber, 0, 77, "demo/foo")
	waitMatching(t, listener.Handler(), true)
}

func TestMatchingListener_MultiplePerPublisher(t *testing.T) {
	router := newMockRouter(t)
	defer router.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sess, err := Open(ctx, NewConfig().WithEndpoint("tcp/"+router.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	ke, _ := NewKeyExpr("demo/foo")
	pub, _ := sess.DeclarePublisher(ke, nil)
	defer pub.Drop()
	obs := <-router.interests

	a, _ := pub.DeclareMatchingListener(NewFifoChannel[MatchingStatus](4))
	defer a.Drop()
	b, _ := pub.DeclareMatchingListener(NewFifoChannel[MatchingStatus](4))
	defer b.Drop()

	router.injectEntityDeclare(t, wire.IDDeclareSubscriber, obs.id, 11, "demo/foo")
	router.injectDeclareFinal(t, obs.id)

	waitMatching(t, a.Handler(), true)
	waitMatching(t, b.Handler(), true)
}

func TestQuerierMatchingListener(t *testing.T) {
	router := newMockRouter(t)
	defer router.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sess, err := Open(ctx, NewConfig().WithEndpoint("tcp/"+router.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	ke, _ := NewKeyExpr("demo/bar")
	q, err := sess.DeclareQuerier(ke, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Drop()

	obs := <-router.interests
	if !obs.filter.Queryables {
		t.Fatalf("querier interest filter = %+v, want Queryables", obs.filter)
	}

	listener, _ := q.DeclareMatchingListener(NewFifoChannel[MatchingStatus](4))
	defer listener.Drop()

	router.injectEntityDeclare(t, wire.IDDeclareQueryable, obs.id, 99, "demo/bar")
	router.injectDeclareFinal(t, obs.id)
	waitMatching(t, listener.Handler(), true)
}

func TestMatchingListener_GetMatchingStatus(t *testing.T) {
	router := newMockRouter(t)
	defer router.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sess, err := Open(ctx, NewConfig().WithEndpoint("tcp/"+router.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	ke, _ := NewKeyExpr("demo/foo")
	pub, _ := sess.DeclarePublisher(ke, nil)
	defer pub.Drop()
	obs := <-router.interests

	got, err := pub.GetMatchingStatus()
	if err != nil || got.Matching {
		t.Fatalf("pre-snapshot: %+v err=%v, want {false}", got, err)
	}

	router.injectEntityDeclare(t, wire.IDDeclareSubscriber, obs.id, 1, "demo/foo")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ = pub.GetMatchingStatus()
		if got.Matching {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !got.Matching {
		t.Fatal("after declare: expected Matching=true")
	}

	pub.Drop()
	if _, err := pub.GetMatchingStatus(); err == nil {
		t.Fatal("GetMatchingStatus on dropped publisher should error")
	}
}
