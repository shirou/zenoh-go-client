//go:build interop && zenoh_unstable

package interop

import (
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/zenoh"
)

// TestCancellationTokenClosesGetInterop: a Get with no matching Queryable
// stays open until the token's Cancel() fires, then the reply channel
// closes. The token path is the only way to cancel a Get that didn't
// accept a context.
func TestCancellationTokenClosesGetInterop(t *testing.T) {
	requireZenohd(t)

	session := openGoSession(t)
	defer session.Close()

	tok := zenoh.NewCancellationToken()
	ke, _ := zenoh.NewKeyExpr("interop/ctx/tokenget")
	replies, err := session.Get(ke, (&zenoh.GetOptions{}).WithCancellation(tok))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Give the REQUEST time to reach zenohd before cancelling, so the
	// cancellation path exercises in-flight termination and not the
	// entry-guard early exit.
	time.Sleep(100 * time.Millisecond)
	tok.Cancel()

	select {
	case _, ok := <-replies:
		if ok {
			t.Error("received a reply before token Cancel; want channel close")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("replies channel did not close within 3s of token Cancel")
	}

	if !tok.IsCancelled() {
		t.Error("token.IsCancelled should be true after Cancel")
	}
}
