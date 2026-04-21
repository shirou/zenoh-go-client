package wire

import (
	"testing"

	"github.com/shirou/zenoh-go-client/internal/codec"
)

// Each test feeds a malformed or truncated byte sequence to the decoder and
// verifies it returns an error (not panic, not silent success).

func TestMalformedInitTruncatedAfterHeader(t *testing.T) {
	// Header only, no version byte following.
	r := codec.NewReader([]byte{0xC1})
	h, err := r.DecodeHeader()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeInit(r, h); err == nil {
		t.Error("expected error for truncated INIT")
	}
}

func TestMalformedInitTruncatedAfterZidLen(t *testing.T) {
	// Header + version + packed (claims 4-byte ZID) but only 2 body bytes.
	r := codec.NewReader([]byte{0xC1, 0x09, 0x32, 0x11, 0x22})
	h, _ := r.DecodeHeader()
	if _, err := DecodeInit(r, h); err == nil {
		t.Error("expected error for truncated ZID")
	}
}

func TestMalformedInitAckMissingCookie(t *testing.T) {
	// A=1 but no cookie bytes follow.
	r := codec.NewReader([]byte{
		0xE1,                   // Z=1, S=1, A=1, ID=0x01
		0x09,                   // version
		0x32,                   // packed (4-byte ZID, Client)
		0x11, 0x22, 0x33, 0x44, // ZID
		0x0A,       // resolution
		0xFF, 0xFF, // batch_size
		// missing cookie <u8;z16>
	})
	h, _ := r.DecodeHeader()
	if _, err := DecodeInit(r, h); err == nil {
		t.Error("expected error for missing cookie")
	}
}

func TestMalformedExtensionChainTruncated(t *testing.T) {
	// Extension header with Z=1 (more follows) but no next extension.
	r := codec.NewReader([]byte{0x81}) // Z=1, ENC=Unit, M=0, ID=1
	if _, err := r.DecodeExtensions(); err == nil {
		t.Error("expected error when extension chain is truncated")
	}
}

func TestMalformedZBufExtensionBodyOverlong(t *testing.T) {
	// ZBuf extension claims z32 length of 10 but body has 2 bytes.
	r := codec.NewReader([]byte{
		0x43,       // ENC=ZBuf, M=0, Z=0, ID=3
		0x0A,       // z32 length = 10
		0x01, 0x02, // only 2 bytes
	})
	if _, err := r.DecodeExtension(); err == nil {
		t.Error("expected error for ZBuf body truncation")
	}
}

func TestMalformedFrameTruncatedSeqNum(t *testing.T) {
	// FRAME header + partial z64 (continuation bit but no next byte).
	r := codec.NewReader([]byte{0x25, 0x80})
	h, _ := r.DecodeHeader()
	if _, err := DecodeFrame(r, h); err == nil {
		t.Error("expected error for truncated seq_num")
	}
}

func TestMalformedDeclareUnknownSubID(t *testing.T) {
	// DECLARE header (Z=0, I=0) + sub-msg with unknown id 0x10.
	r := codec.NewReader([]byte{
		0x1E,              // DECLARE header (ID=0x1E)
		0x10, 0x00, 0x00, // fake sub-msg header + some bytes
	})
	h, _ := r.DecodeHeader()
	if _, err := DecodeDeclare(r, h); err == nil {
		t.Error("expected error for unknown declaration sub-id")
	}
}

// TestFragmentWithExtensions verifies FRAGMENT roundtrips with an attached
// QoS extension (priority lane indicator, Z64 body).
func TestFragmentWithExtensions(t *testing.T) {
	qosExt := codec.Extension{
		Header: codec.ExtHeader{ID: 0x1, Encoding: codec.ExtEncZ64, Mandatory: true},
		Z64:    uint64(QoS{Priority: QoSPriorityDataHigh}.EncodeZ64()),
	}
	orig := &Fragment{
		Reliable:   true,
		More:       true,
		SeqNum:     42,
		Extensions: []codec.Extension{qosExt},
		Body:       []byte{0xDE, 0xAD},
	}
	w := codec.NewWriter(32)
	if err := orig.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	r := codec.NewReader(w.Bytes())
	h, _ := r.DecodeHeader()
	got, err := DecodeFragment(r, h)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Extensions) != 1 || got.Extensions[0].Header.ID != 0x1 {
		t.Errorf("extensions lost: %+v", got.Extensions)
	}
	if got.SeqNum != 42 || !got.More || !got.Reliable {
		t.Errorf("header fields mismatch: %+v", got)
	}
}

// TestCloseWithExtensions — spec allows extensions on CLOSE; ensure chain
// roundtrips.
func TestCloseWithExtensions(t *testing.T) {
	orig := &Close{
		Session: true,
		Reason:  0x05, // EXPIRED
		Extensions: []codec.Extension{
			{Header: codec.ExtHeader{ID: 0x2, Encoding: codec.ExtEncZ64}, Z64: 0x1234},
		},
	}
	w := codec.NewWriter(16)
	if err := orig.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	r := codec.NewReader(w.Bytes())
	h, _ := r.DecodeHeader()
	got, err := DecodeClose(r, h)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reason != 0x05 {
		t.Errorf("reason = %#x", got.Reason)
	}
	if len(got.Extensions) != 1 || got.Extensions[0].Z64 != 0x1234 {
		t.Errorf("extensions: %+v", got.Extensions)
	}
}

// TestDeclareWithQoSExtension verifies DECLARE roundtrips with its QoS ext.
func TestDeclareWithQoSExtension(t *testing.T) {
	orig := &Declare{
		Extensions: []codec.Extension{
			{Header: codec.ExtHeader{ID: 0x1, Encoding: codec.ExtEncZ64}, Z64: 0x0D},
		},
		Body: NewDeclareSubscriber(77, WireExpr{Scope: 0, Suffix: "a/b"}),
	}
	w := codec.NewWriter(32)
	if err := orig.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	r := codec.NewReader(w.Bytes())
	h, _ := r.DecodeHeader()
	got, err := DecodeDeclare(r, h)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Extensions) != 1 || got.Extensions[0].Z64 != 0x0D {
		t.Errorf("DECLARE extensions lost: %+v", got.Extensions)
	}
	sub := got.Body.(*DeclareEntity)
	if sub.Kind != IDDeclareSubscriber || sub.EntityID != 77 {
		t.Errorf("body = %+v", sub)
	}
}
