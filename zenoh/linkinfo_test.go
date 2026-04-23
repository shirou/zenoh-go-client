package zenoh

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSessionLinkInfo(t *testing.T) {
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

	info := sess.LinkInfo()
	if info == nil {
		t.Fatal("LinkInfo() returned nil on an open session")
	}

	if info.Scheme != "tcp" {
		t.Errorf("Scheme = %q, want tcp", info.Scheme)
	}
	if info.RemoteAddress != router.Addr() {
		t.Errorf("RemoteAddress = %q, want %q", info.RemoteAddress, router.Addr())
	}
	if info.RemoteLocator != "tcp/"+router.Addr() {
		t.Errorf("RemoteLocator = %q", info.RemoteLocator)
	}
	// TCP outbound sockets always have a local addr; exact port is
	// OS-picked so just assert non-empty + host:port shape.
	if info.LocalAddress == "" || !strings.Contains(info.LocalAddress, ":") {
		t.Errorf("LocalAddress = %q, want host:port form", info.LocalAddress)
	}
	if info.PeerWhatAmI != WhatAmIRouter {
		t.Errorf("PeerWhatAmI = %v, want Router", info.PeerWhatAmI)
	}
	// mockRouter advertises ZID 0xAB 0xCD.
	if info.PeerZID.String() != "abcd" {
		t.Errorf("PeerZID = %q, want abcd", info.PeerZID.String())
	}
	if info.NegotiatedBatchSize == 0 {
		t.Error("NegotiatedBatchSize = 0")
	}
	if !info.QoSEnabled {
		t.Error("QoSEnabled = false, mock echoes the QoS ext so it should be on")
	}
	if info.LocalLeaseMillis == 0 || info.PeerLeaseMillis == 0 {
		t.Errorf("leases should be non-zero: local=%d peer=%d",
			info.LocalLeaseMillis, info.PeerLeaseMillis)
	}
}

func TestSessionLinkInfoNilAfterClose(t *testing.T) {
	router := newMockRouter(t)
	defer router.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cfg := NewConfig().WithEndpoint("tcp/" + router.Addr())
	sess, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sess.LinkInfo() == nil {
		t.Fatal("expected LinkInfo before Close")
	}
	_ = sess.Close()
	if info := sess.LinkInfo(); info != nil {
		t.Errorf("LinkInfo after Close = %+v, want nil", info)
	}
}
