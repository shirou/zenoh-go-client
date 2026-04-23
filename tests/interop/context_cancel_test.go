//go:build interop

package interop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/zenoh"
)

// TestOpenContextCancelAbortsDial targets a non-listening endpoint so the
// dial blocks on TCP SYN retransmit; cancelling the context while Open is
// in flight must return promptly with ctx.Err() (not hang until the
// handshake times out).
func TestOpenContextCancelAbortsDial(t *testing.T) {
	// Use TEST-NET-1 (RFC 5737) on a closed port: dial will block on SYN
	// retries for many seconds without timing out on its own.
	const unreachable = "tcp/192.0.2.1:65530"

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		cfg := zenoh.NewConfig().WithEndpoint(unreachable)
		sess, err := zenoh.Open(ctx, cfg)
		if sess != nil {
			_ = sess.Close()
		}
		done <- err
	}()

	// Give the dial a head start so ctx cancel actually interrupts an
	// in-flight connect rather than short-circuiting before it starts.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Open returned nil after ctx cancel; want ctx.Err or dial error")
		}
		// Accept either context.Canceled (ctx wins the race) or a dial
		// error that wraps it. Either is a valid signal that Open stopped.
	case <-time.After(3 * time.Second):
		t.Fatal("Open did not return within 3s after ctx cancel")
	}
}

// TestOpenContextAlreadyDone covers the simpler branch: Open called with
// an already-cancelled context must fail fast without dialling.
func TestOpenContextAlreadyDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cfg := zenoh.NewConfig().WithEndpoint(hostEndpoint)
	sess, err := zenoh.Open(ctx, cfg)
	if sess != nil {
		_ = sess.Close()
	}
	if err == nil {
		t.Fatal("Open with pre-cancelled ctx succeeded; want error")
	}
}

// TestPutWithContextRejectsCancelled asserts the context.Err() guard on
// the Put entry path: a pre-cancelled ctx must yield ctx.Err() without
// touching the outbound queue.
func TestPutWithContextRejectsCancelled(t *testing.T) {
	requireZenohd(t)

	session := openGoSession(t)
	defer session.Close()

	ke, _ := zenoh.NewKeyExpr("interop/ctx/put")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := session.PutWithContext(ctx, ke, zenoh.NewZBytesFromString("x"), nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("PutWithContext(pre-cancelled) err = %v, want context.Canceled", err)
	}

	err = session.DeleteWithContext(ctx, ke, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("DeleteWithContext(pre-cancelled) err = %v, want context.Canceled", err)
	}
}

// TestGetWithContextCancelClosesReplies verifies that cancelling ctx on a
// Get with no matching Queryable closes the reply channel. Without a
// queryable the router will never send RESPONSE_FINAL for this key, so
// the test proves the ctx cancel path — not the happy path — drives the
// channel closed.
func TestGetWithContextCancelClosesReplies(t *testing.T) {
	requireZenohd(t)

	session := openGoSession(t)
	defer session.Close()

	ke, _ := zenoh.NewKeyExpr("interop/ctx/get/noqbl")
	ctx, cancel := context.WithCancel(context.Background())

	replies, err := session.GetWithContext(ctx, ke, nil)
	if err != nil {
		t.Fatalf("GetWithContext: %v", err)
	}

	// Give the REQUEST time to land at zenohd so the cancel path exercises
	// the in-flight cancellation machinery rather than the entry guard.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case _, ok := <-replies:
		if ok {
			t.Error("received a reply on a key with no queryable; want channel close")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reply channel did not close within 3s of ctx cancel")
	}
}

// TestGetWithContextAlreadyDone covers the entry guard for Get, matching
// the Put/Delete tests above.
func TestGetWithContextAlreadyDone(t *testing.T) {
	requireZenohd(t)

	session := openGoSession(t)
	defer session.Close()

	ke, _ := zenoh.NewKeyExpr("interop/ctx/get/precancel")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := session.GetWithContext(ctx, ke, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("GetWithContext(pre-cancelled) err = %v, want context.Canceled", err)
	}
}
