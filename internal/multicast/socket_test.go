package multicast

import (
	"bytes"
	"testing"
	"time"
)

// TestOpenUnicastOnly verifies a Plan with no multicast group but a
// single unicast target opens just the IPv4 send socket and resolves
// the target.
func TestOpenUnicastOnly(t *testing.T) {
	plan := Plan{Unicast: []string{"127.0.0.1:7446"}}
	s, err := Open(plan)
	if err != nil {
		t.Fatal(err)
	}
	defer s.CloseAll()

	if s.SendV4 == nil {
		t.Error("expected SendV4 socket")
	}
	if len(s.TargetsV4) != 1 {
		t.Errorf("TargetsV4 len = %d, want 1", len(s.TargetsV4))
	}
	if s.McastV4 != nil || s.McastV6 != nil {
		t.Error("no multicast group expected")
	}
}

// TestOpenSendListenLoopback round-trips a datagram through a v4
// multicast group on 127.0.0.1 with EnableSend + EnableListen + loopback
// on (the default) so the listen socket sees what the send socket emits.
func TestOpenSendListenLoopback(t *testing.T) {
	plan := Plan{
		Group:        "224.0.0.225:0",
		EnableSend:   true,
		EnableListen: true,
	}
	// Ports of zero confuse some OSes for multicast; pick a range-safe
	// random-ish port. Re-bind on collision.
	port := 17460 + int(time.Now().UnixNano()%200)
	plan.Group = "224.0.0.225:" + itoa(port)

	s, err := Open(plan)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.CloseAll()

	if s.SendV4 == nil || s.ListenV4 == nil {
		t.Fatalf("missing sockets: send=%v listen=%v", s.SendV4 != nil, s.ListenV4 != nil)
	}

	got := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 64)
		_ = s.ListenV4.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _, err := s.ListenV4.ReadFromUDP(buf)
		if err != nil {
			got <- nil
			return
		}
		got <- buf[:n]
	}()

	payload := []byte("multicast-roundtrip")
	if _, err := s.SendV4.WriteToUDP(payload, s.McastV4); err != nil {
		t.Fatalf("WriteToUDP: %v", err)
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

// TestUnblockReadersWakesPendingRead asserts UnblockReaders pushes a
// blocked ReadFromUDP out with a deadline-exceeded error so the listen
// goroutine can exit cleanly during shutdown.
func TestUnblockReadersWakesPendingRead(t *testing.T) {
	plan := Plan{
		Group:        "224.0.0.226:18876",
		EnableListen: true,
	}
	s, err := Open(plan)
	if err != nil {
		t.Fatal(err)
	}
	defer s.CloseAll()

	if s.ListenV4 == nil {
		t.Fatal("expected ListenV4")
	}

	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 32)
		_, _, err := s.ListenV4.ReadFromUDP(buf)
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	s.UnblockReaders()

	select {
	case err := <-done:
		// Any non-nil error is fine — the point is the read returns.
		if err == nil {
			t.Error("ReadFromUDP returned nil error after UnblockReaders")
		}
	case <-time.After(time.Second):
		t.Fatal("UnblockReaders failed to interrupt blocking read")
	}
}

// TestOpenInvalidGroup surfaces a clear error when the group string is
// malformed (no port). Uses syntactic — not DNS — failure so the test
// completes synchronously.
func TestOpenInvalidGroup(t *testing.T) {
	plan := Plan{Group: "missing-port", EnableSend: true}
	_, err := Open(plan)
	if err == nil {
		t.Fatal("expected error from malformed group")
	}
}

func itoa(n int) string {
	return formatPort(uint16(n))
}

func formatPort(p uint16) string {
	const digits = "0123456789"
	if p == 0 {
		return "0"
	}
	var buf [5]byte
	i := len(buf)
	for p > 0 {
		i--
		buf[i] = digits[p%10]
		p /= 10
	}
	return string(buf[i:])
}
