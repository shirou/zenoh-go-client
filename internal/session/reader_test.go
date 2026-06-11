package session

import (
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/shirou/zenoh-go-client/internal/codec"
	"github.com/shirou/zenoh-go-client/internal/transport"
	"github.com/shirou/zenoh-go-client/internal/wire"
)

// The reference transport packs several FRAMEs (and other transport
// messages) into one batch: a new FRAME header is opened mid-batch whenever
// the reliability or priority of the next message differs, and KEEPALIVE may
// trail the data. The reader must dispatch every network message, treating
// the first non-network header byte as the frame boundary.
func TestProcessBatchMultipleFramesPerBatch(t *testing.T) {
	w := codec.NewWriter(64)

	// Reliable FRAME carrying one RESPONSE_FINAL.
	if err := (&wire.Frame{Reliable: true, SeqNum: 10}).EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	if err := (&wire.ResponseFinal{RequestID: 1}).EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	// Best-effort FRAME carrying two more.
	if err := (&wire.Frame{Reliable: false, SeqNum: 20}).EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	if err := (&wire.ResponseFinal{RequestID: 2}).EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	if err := (&wire.ResponseFinal{RequestID: 3}).EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	// Trailing KEEPALIVE in the same batch.
	if err := (&wire.KeepAlive{}).EncodeTo(w); err != nil {
		t.Fatal(err)
	}

	var got []uint32
	cfg := ReaderConfig{
		Reassembler:    transport.NewReassembler(1<<20, 0),
		Logger:         slog.Default(),
		LastRecvUnixMs: &atomic.Int64{},
		Dispatch: func(h codec.Header, r *codec.Reader) error {
			rf, err := wire.DecodeResponseFinal(r, h)
			if err != nil {
				return err
			}
			got = append(got, rf.RequestID)
			return nil
		},
	}
	if err := processBatch(cfg, w.Bytes()); err != nil {
		t.Fatalf("processBatch: %v", err)
	}
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Errorf("dispatched request ids = %v, want [1 2 3]", got)
	}
}
