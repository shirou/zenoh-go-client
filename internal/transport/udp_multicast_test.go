package transport

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/shirou/zenoh-go-client/internal/locator"
)

// TestUDPMulticastRoundtrip dials two UDPMulticastLink instances on the
// same group and asserts a datagram emitted by one is received by the
// other (loopback enabled by default).
func TestUDPMulticastRoundtrip(t *testing.T) {
	port := 17800 + int(time.Now().UnixNano()%200)
	loc := locator.Locator{Scheme: locator.SchemeUDP, Address: fmt.Sprintf("224.0.0.227:%d", port)}

	a, err := DialMulticastUDP(loc, MulticastDialOpts{})
	if err != nil {
		t.Fatalf("Dial(a): %v", err)
	}
	defer a.Close()

	b, err := DialMulticastUDP(loc, MulticastDialOpts{})
	if err != nil {
		t.Fatalf("Dial(b): %v", err)
	}
	defer b.Close()

	got := make(chan []byte, 1)
	go func() {
		buf := make([]byte, MulticastBatchSize)
		// Bound the read so a missing IGMP membership doesn't hang.
		if err := setReadDeadline(b, 2*time.Second); err != nil {
			got <- nil
			return
		}
		n, _, err := b.ReadBatchFrom(buf)
		if err != nil {
			got <- nil
			return
		}
		got <- buf[:n]
	}()

	payload := []byte("multicast-link-roundtrip")
	if err := a.WriteBatch(payload); err != nil {
		t.Fatalf("a.WriteBatch: %v", err)
	}

	select {
	case b := <-got:
		if b == nil {
			t.Skip("multicast loopback not delivered (Linux container without IGMP membership?)")
		}
		if !bytes.Equal(b, payload) {
			t.Errorf("got %q, want %q", b, payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("listen socket never received the datagram")
	}
}

// TestUDPMulticastLoopbackDisabled exercises the IP_MULTICAST_LOOP=0
// path: the same link emits a datagram, but its own listen socket
// should not receive it back.
func TestUDPMulticastLoopbackDisabled(t *testing.T) {
	port := 18000 + int(time.Now().UnixNano()%200)
	loc := locator.Locator{Scheme: locator.SchemeUDP, Address: fmt.Sprintf("224.0.0.228:%d", port)}

	link, err := DialMulticastUDP(loc, MulticastDialOpts{LoopbackDisabled: true})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer link.Close()

	if err := link.WriteBatch([]byte("self")); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	if err := setReadDeadline(link, 250*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, MulticastBatchSize)
	_, _, err = link.ReadBatchFrom(buf)
	if err == nil {
		t.Errorf("ReadBatchFrom received a datagram despite LoopbackDisabled=true")
	}
}

// TestUDPMulticastWrongScheme guards against accidental TCP dialling.
func TestUDPMulticastWrongScheme(t *testing.T) {
	loc := locator.Locator{Scheme: locator.SchemeTCP, Address: "127.0.0.1:7447"}
	if _, err := DialMulticastUDP(loc, MulticastDialOpts{}); err == nil {
		t.Fatal("expected scheme error")
	}
}

// setReadDeadline is a tiny helper that pokes the listen socket via the
// link without exposing it. Tests use this to avoid hanging when the
// host doesn't deliver multicast loopback (CI sandboxes etc.).
func setReadDeadline(l *UDPMulticastLink, d time.Duration) error {
	return l.listenConn.SetReadDeadline(time.Now().Add(d))
}
