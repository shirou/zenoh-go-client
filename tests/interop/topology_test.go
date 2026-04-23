//go:build interop && chain

package interop

import (
	"context"
	"fmt"
	"net"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/zenoh"
)

// Router-chain interop suite. Uses tests/docker-compose.chain.yml:
//
//	Go (host) --tcp/127.0.0.1:17447--> zenohd1 <--connect-- zenohd2 --tcp/127.0.0.1:17448--> Go (host)
//
// Both routers share their subscriber / queryable state across the link
// zenohd2 opens to zenohd1, so a subscriber on one side receives pushes
// from the other side and a Get reaches a queryable on the other side.
const (
	chainEndpoint1 = "tcp/127.0.0.1:17447"
	chainEndpoint2 = "tcp/127.0.0.1:17448"
)

// requireChainInfra skips unless both router ports accept a TCP dial.
func requireChainInfra(t *testing.T) {
	t.Helper()
	for _, addr := range []string{"127.0.0.1:17447", "127.0.0.1:17448"} {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Skipf("chain router not reachable at %s (run `make interop-chain-up` first): %v", addr, err)
		}
		_ = conn.Close()
	}
}

// openChainSession opens a session against the supplied endpoint with
// the default reconnect config. Each chain test uses distinct endpoints
// for the two sides so that messages must traverse the chain.
func openChainSession(t *testing.T, endpoint string) *zenoh.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := zenoh.NewConfig().WithEndpoint(endpoint)
	s, err := zenoh.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("zenoh.Open(%s): %v", endpoint, err)
	}
	return s
}

// TestChainPubSub publishes on router 1 and subscribes on router 2:
// the sample must cross the router-to-router link to be delivered.
func TestChainPubSub(t *testing.T) {
	requireChainInfra(t)

	pubSess := openChainSession(t, chainEndpoint1)
	defer pubSess.Close()
	subSess := openChainSession(t, chainEndpoint2)
	defer subSess.Close()

	const key = "interop/chain/pubsub"
	ke, err := zenoh.NewKeyExpr(key)
	if err != nil {
		t.Fatalf("NewKeyExpr: %v", err)
	}

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

	// Chain propagation needs longer than the single-router case because
	// the D_SUBSCRIBER INTEREST must traverse both router hops before
	// zenohd1 will forward the first PUSH back to the publisher side.
	time.Sleep(subPropagationDelay * 3)

	pub, err := pubSess.DeclarePublisher(ke, nil)
	if err != nil {
		t.Fatalf("DeclarePublisher: %v", err)
	}
	defer pub.Drop()

	for i := 0; i < 5; i++ {
		if err := pub.Put(zenoh.NewZBytesFromString(fmt.Sprintf("chain-%d", i)), nil); err != nil {
			t.Fatalf("pub.Put[%d]: %v", i, err)
		}
	}

	waitForSampleCount(t, &mu, &samples, 5, 5*time.Second)
	mu.Lock()
	got := slices.Clone(samples)
	mu.Unlock()
	for i, s := range got {
		want := fmt.Sprintf("chain-%d", i)
		if s != want {
			t.Errorf("samples[%d] = %q, want %q", i, s, want)
		}
	}
}

// TestChainGet issues Get on router 1 and answers it from a queryable
// declared on router 2: the query and reply must traverse the chain
// in both directions.
func TestChainGet(t *testing.T) {
	requireChainInfra(t)

	getSess := openChainSession(t, chainEndpoint1)
	defer getSess.Close()
	qblSess := openChainSession(t, chainEndpoint2)
	defer qblSess.Close()

	const (
		key   = "interop/chain/get"
		reply = "chain-reply"
	)
	ke, _ := zenoh.NewKeyExpr(key)

	qbl, err := qblSess.DeclareQueryable(ke, func(q *zenoh.Query) {
		if err := q.Reply(ke, zenoh.NewZBytesFromString(reply), nil); err != nil {
			t.Errorf("q.Reply: %v", err)
		}
	}, nil)
	if err != nil {
		t.Fatalf("DeclareQueryable: %v", err)
	}
	defer qbl.Drop()

	time.Sleep(subPropagationDelay * 3)

	replies, err := getSess.Get(ke, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	payloads := collectGetPayloads(t, replies, 5*time.Second)
	if len(payloads) != 1 || payloads[0] != reply {
		t.Errorf("chain Get replies = %v, want [%q]", payloads, reply)
	}
}

// TestChainKeyExprAliasResolution exercises the D_KEYEXPR alias path:
// the publisher declares a publisher on a fairly deep key, and the
// receiving subscriber must reconstruct the full key string after the
// chain has rewritten the sample using an alias ID.
//
// A prefix-heavy key is chosen so the router is more likely to fold it
// into an alias rather than shipping it verbatim.
func TestChainKeyExprAliasResolution(t *testing.T) {
	requireChainInfra(t)

	pubSess := openChainSession(t, chainEndpoint1)
	defer pubSess.Close()
	subSess := openChainSession(t, chainEndpoint2)
	defer subSess.Close()

	const key = "interop/chain/alias/deeply/nested/resource"
	ke, _ := zenoh.NewKeyExpr(key)

	type received struct {
		key     string
		payload string
	}
	ch := make(chan received, 16)
	sub, err := subSess.DeclareSubscriber(ke, zenoh.Closure[zenoh.Sample]{
		Call: func(s zenoh.Sample) {
			ch <- received{key: s.KeyExpr().String(), payload: string(s.Payload().Bytes())}
		},
	})
	if err != nil {
		t.Fatalf("DeclareSubscriber: %v", err)
	}
	defer sub.Drop()

	time.Sleep(subPropagationDelay * 3)

	pub, err := pubSess.DeclarePublisher(ke, nil)
	if err != nil {
		t.Fatalf("DeclarePublisher: %v", err)
	}
	defer pub.Drop()

	// Publish twice so the second emission — which in the normal router
	// rewriting path uses the previously-established alias ID — round-
	// trips through the chain alias-resolved.
	for i := 0; i < 2; i++ {
		if err := pub.Put(zenoh.NewZBytesFromString(fmt.Sprintf("alias-%d", i)), nil); err != nil {
			t.Fatalf("pub.Put[%d]: %v", i, err)
		}
	}

	deadline := time.After(5 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case r := <-ch:
			if r.key != key {
				t.Errorf("sample[%d] key = %q, want %q", i, r.key, key)
			}
			want := fmt.Sprintf("alias-%d", i)
			if r.payload != want {
				t.Errorf("sample[%d] payload = %q, want %q", i, r.payload, want)
			}
		case <-deadline:
			t.Fatalf("only %d/2 samples received before deadline", i)
		}
	}
}

// TestChainMultiPublishers verifies that a single subscriber on router 2
// receives pushes from three independent publishers each hanging off
// router 1. The total set of payloads delivered equals the union of the
// three publisher streams.
func TestChainMultiPublishers(t *testing.T) {
	requireChainInfra(t)

	const (
		key     = "interop/chain/multi_pub"
		perPub  = 3
		numPubs = 3
	)
	ke, _ := zenoh.NewKeyExpr(key)

	subSess := openChainSession(t, chainEndpoint2)
	defer subSess.Close()

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

	time.Sleep(subPropagationDelay * 3)

	pubSessions := make([]*zenoh.Session, 0, numPubs)
	defer func() {
		for _, s := range pubSessions {
			s.Close()
		}
	}()
	for p := 0; p < numPubs; p++ {
		s := openChainSession(t, chainEndpoint1)
		pubSessions = append(pubSessions, s)
		pub, err := s.DeclarePublisher(ke, nil)
		if err != nil {
			t.Fatalf("DeclarePublisher[%d]: %v", p, err)
		}
		for i := 0; i < perPub; i++ {
			if err := pub.Put(zenoh.NewZBytesFromString(fmt.Sprintf("p%d-%d", p, i)), nil); err != nil {
				t.Fatalf("pub[%d].Put[%d]: %v", p, i, err)
			}
		}
		pub.Drop()
	}

	want := numPubs * perPub
	waitForSampleCount(t, &mu, &samples, want, 5*time.Second)

	mu.Lock()
	got := slices.Clone(samples)
	mu.Unlock()
	sort.Strings(got)
	var expected []string
	for p := 0; p < numPubs; p++ {
		for i := 0; i < perPub; i++ {
			expected = append(expected, fmt.Sprintf("p%d-%d", p, i))
		}
	}
	sort.Strings(expected)
	if !slices.Equal(got, expected) {
		t.Errorf("received %v, want %v", got, expected)
	}
}

// TestChainMultiSubscribers verifies that every one of three subscribers
// — two on router 1, one on router 2 — sees every sample a publisher on
// router 2 emits. Fan-out through the chain and back must not drop or
// duplicate a sample per subscriber.
func TestChainMultiSubscribers(t *testing.T) {
	requireChainInfra(t)

	const (
		key   = "interop/chain/multi_sub"
		count = 4
	)
	ke, _ := zenoh.NewKeyExpr(key)

	// Three subscribers across the two routers: idx 0,1 on router 1
	// reach the publisher through the chain; idx 2 on router 2 is
	// co-located with the publisher so it tests the same-router path.
	subEndpoints := []string{chainEndpoint1, chainEndpoint1, chainEndpoint2}

	type slot struct {
		sess    *zenoh.Session
		mu      sync.Mutex
		samples []string
	}
	slots := make([]*slot, len(subEndpoints))
	for i, ep := range subEndpoints {
		s := openChainSession(t, ep)
		sl := &slot{sess: s}
		slots[i] = sl
		sub, err := s.DeclareSubscriber(ke, zenoh.Closure[zenoh.Sample]{
			Call: func(smp zenoh.Sample) {
				sl.mu.Lock()
				sl.samples = append(sl.samples, string(smp.Payload().Bytes()))
				sl.mu.Unlock()
			},
		})
		if err != nil {
			t.Fatalf("DeclareSubscriber[%d]: %v", i, err)
		}
		defer sub.Drop()
	}
	defer func() {
		for _, sl := range slots {
			sl.sess.Close()
		}
	}()

	pubSess := openChainSession(t, chainEndpoint2)
	defer pubSess.Close()

	time.Sleep(subPropagationDelay * 3)

	pub, err := pubSess.DeclarePublisher(ke, nil)
	if err != nil {
		t.Fatalf("DeclarePublisher: %v", err)
	}
	defer pub.Drop()

	for i := 0; i < count; i++ {
		if err := pub.Put(zenoh.NewZBytesFromString(fmt.Sprintf("fan-%d", i)), nil); err != nil {
			t.Fatalf("pub.Put[%d]: %v", i, err)
		}
	}

	for i, sl := range slots {
		waitForSampleCount(t, &sl.mu, &sl.samples, count, 5*time.Second)
		sl.mu.Lock()
		got := slices.Clone(sl.samples)
		sl.mu.Unlock()
		sort.Strings(got)
		var want []string
		for j := 0; j < count; j++ {
			want = append(want, fmt.Sprintf("fan-%d", j))
		}
		sort.Strings(want)
		if !slices.Equal(got, want) {
			t.Errorf("sub[%d] got %v, want %v", i, got, want)
		}
	}
}

// TestChainMultiQueryables verifies that Get with QueryTarget=All fans
// out to queryables on both routers and that each reply crosses back
// across the chain unmodified.
func TestChainMultiQueryables(t *testing.T) {
	requireChainInfra(t)

	const key = "interop/chain/multi_qbl"
	ke, _ := zenoh.NewKeyExpr(key)

	// Two queryables on router 1, one on router 2 — the Get request
	// originates from router 2 so one reply is local and two must
	// traverse the chain.
	qblEndpoints := []string{chainEndpoint1, chainEndpoint1, chainEndpoint2}
	qblSessions := make([]*zenoh.Session, 0, len(qblEndpoints))
	defer func() {
		for _, s := range qblSessions {
			s.Close()
		}
	}()
	for i, ep := range qblEndpoints {
		s := openChainSession(t, ep)
		qblSessions = append(qblSessions, s)
		id := i
		qbl, err := s.DeclareQueryable(ke, func(q *zenoh.Query) {
			_ = q.Reply(ke, zenoh.NewZBytesFromString(fmt.Sprintf("q%d", id)), nil)
		}, nil)
		if err != nil {
			t.Fatalf("DeclareQueryable[%d]: %v", i, err)
		}
		defer qbl.Drop()
	}

	getSess := openChainSession(t, chainEndpoint2)
	defer getSess.Close()

	time.Sleep(subPropagationDelay * 3)

	replies, err := getSess.Get(ke, &zenoh.GetOptions{
		Target:    zenoh.QueryTargetAll,
		HasTarget: true,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	payloads := collectGetPayloads(t, replies, 5*time.Second)
	sort.Strings(payloads)
	want := []string{"q0", "q1", "q2"}
	if !slices.Equal(payloads, want) {
		t.Errorf("replies = %v, want %v", payloads, want)
	}
}

// waitForSampleCount blocks until the guarded slice has at least want
// entries, or fails on timeout.
func waitForSampleCount(t *testing.T, mu *sync.Mutex, samples *[]string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(*samples)
		mu.Unlock()
		if n >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	got := slices.Clone(*samples)
	mu.Unlock()
	t.Fatalf("only %d/%d samples received within %v; got %v", len(got), want, timeout, got)
}
