//go:build zenoh_unstable

package zenoh

import (
	"context"
	"testing"
	"time"
)

// TestCancellationTokenCancelsGet verifies that triggering a
// CancellationToken closes the replies channel of a Get that accepted it
// via GetOptions.WithCancellation.
func TestCancellationTokenCancelsGet(t *testing.T) {
	router := newMockRouter(t)
	defer router.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sess, err := Open(ctx, NewConfig().WithEndpoint("tcp/"+router.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	tok := NewCancellationToken()
	ke, _ := NewKeyExpr("demo/cancel/**")
	replies, err := sess.Get(ke, (&GetOptions{}).WithCancellation(tok))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Let the REQUEST reach the router so the Get is truly in flight.
	select {
	case <-router.requests:
	case <-time.After(2 * time.Second):
		t.Fatal("router did not observe REQUEST")
	}

	tok.Cancel()

	select {
	case _, ok := <-replies:
		if ok {
			t.Error("expected closed replies channel after token Cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replies channel did not close after token Cancel")
	}

	if !tok.IsCancelled() {
		t.Error("token.IsCancelled should be true after Cancel")
	}
}
