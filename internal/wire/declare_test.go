package wire

import (
	"testing"

	"github.com/shirou/zenoh-go-client/internal/codec"
)

func TestDeclareKeyExprRoundtrip(t *testing.T) {
	orig := &Declare{
		Body: &DeclareKeyExpr{
			ExprID: 42,
			Scope:  0,
			Suffix: "demo/example/**",
		},
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
	body, ok := got.Body.(*DeclareKeyExpr)
	if !ok {
		t.Fatalf("wrong body type: %T", got.Body)
	}
	if body.ExprID != 42 || body.Suffix != "demo/example/**" {
		t.Errorf("body = %+v", body)
	}
}

func TestDeclareSubscriberWithInterestID(t *testing.T) {
	orig := &Declare{
		HasInterestID: true,
		InterestID:    7,
		Body:          NewDeclareSubscriber(99, WireExpr{Scope: 0, Suffix: "demo/**"}),
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
	if !got.HasInterestID || got.InterestID != 7 {
		t.Errorf("interest_id lost: %+v", got)
	}
	body := got.Body.(*DeclareEntity)
	if body.Kind != IDDeclareSubscriber || body.EntityID != 99 || body.KeyExpr.Suffix != "demo/**" {
		t.Errorf("body = %+v", body)
	}
}

func TestUndeclareSubscriberCarriesWireExpr(t *testing.T) {
	orig := &Declare{
		Body: &UndeclareEntity{
			Kind:     IDUndeclareSubscriber,
			EntityID: 99,
			WireExpr: WireExpr{Scope: 0, Suffix: "demo/**"},
		},
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
	body := got.Body.(*UndeclareEntity)
	if body.Kind != IDUndeclareSubscriber || body.EntityID != 99 {
		t.Errorf("body header wrong: %+v", body)
	}
	if body.WireExpr.Suffix != "demo/**" {
		t.Errorf("WireExpr suffix lost: %+v", body.WireExpr)
	}
}

func TestDeclareFinal(t *testing.T) {
	orig := &Declare{
		HasInterestID: true,
		InterestID:    3,
		Body:          &DeclareFinal{},
	}
	w := codec.NewWriter(8)
	if err := orig.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	r := codec.NewReader(w.Bytes())
	h, _ := r.DecodeHeader()
	got, err := DecodeDeclare(r, h)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Body.(*DeclareFinal); !ok {
		t.Errorf("want DeclareFinal, got %T", got.Body)
	}
	if got.InterestID != 3 {
		t.Errorf("interest_id = %d", got.InterestID)
	}
}

func TestInterestCurrentFuture(t *testing.T) {
	ke := WireExpr{Scope: 0, Suffix: "demo/**"}
	orig := &Interest{
		InterestID: 1,
		Mode:       InterestModeCurrentFuture,
		Filter:     InterestFilter{KeyExprs: true, Subscribers: true},
		KeyExpr:    &ke,
	}
	w := codec.NewWriter(32)
	if err := orig.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	r := codec.NewReader(w.Bytes())
	h, _ := r.DecodeHeader()
	got, err := DecodeInterest(r, h)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != InterestModeCurrentFuture {
		t.Errorf("mode = %d", got.Mode)
	}
	if !got.Filter.KeyExprs || !got.Filter.Subscribers {
		t.Errorf("filter lost: %+v", got.Filter)
	}
	if got.KeyExpr == nil || got.KeyExpr.Suffix != "demo/**" {
		t.Errorf("KeyExpr = %+v", got.KeyExpr)
	}
}

// TestInterestUnrestrictedDoesNotSetNM verifies that when KeyExpr is nil the
// N and M flags are NOT emitted. Previous bug: encoder unconditionally
// copied them from KeyExpr.Named()/Mapping.
func TestInterestUnrestrictedDoesNotSetNM(t *testing.T) {
	orig := &Interest{
		InterestID: 2,
		Mode:       InterestModeCurrent,
		Filter:     InterestFilter{Subscribers: true},
		KeyExpr:    nil,
	}
	w := codec.NewWriter(8)
	if err := orig.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	// Bytes: header(0x19, Mod=01) + VLE id(2) + options byte.
	// Options must have R=0, N=0, M=0 and only S bit set.
	if w.Len() < 3 {
		t.Fatalf("encoded too short: %d", w.Len())
	}
	opts := w.Bytes()[w.Len()-1] // last byte is options byte
	if opts&(1<<interestOptBitRestricted) != 0 {
		t.Errorf("R flag set without KeyExpr: opts=%#08b", opts)
	}
	if opts&(1<<interestOptBitNamed) != 0 {
		t.Errorf("N flag set without KeyExpr: opts=%#08b", opts)
	}
	if opts&(1<<interestOptBitMapping) != 0 {
		t.Errorf("M flag set without KeyExpr: opts=%#08b", opts)
	}
}

func TestInterestFinalHasNoOptionsByte(t *testing.T) {
	orig := &Interest{
		InterestID: 1,
		Mode:       InterestModeFinal,
	}
	w := codec.NewWriter(8)
	if err := orig.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	// Expected bytes: header(0x19, Mod=00, Z=0) + VLE(1) = 2 bytes.
	if w.Len() != 2 {
		t.Errorf("INTEREST Final length = %d, want 2", w.Len())
	}
	r := codec.NewReader(w.Bytes())
	h, _ := r.DecodeHeader()
	got, err := DecodeInterest(r, h)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != InterestModeFinal || got.InterestID != 1 {
		t.Errorf("got %+v", got)
	}
}

func TestScoutRoundtrip(t *testing.T) {
	zid := ZenohID{Bytes: []byte{0x11, 0x22, 0x33, 0x44}}
	orig := &Scout{
		Version: ProtoVersion,
		Matcher: MatcherRouter | MatcherPeer, // bitmap, not a single role
		ZID:     zid,
	}
	w := codec.NewWriter(32)
	if err := orig.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	r := codec.NewReader(w.Bytes())
	h, _ := r.DecodeHeader()
	got, err := DecodeScout(r, h)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ZID.Equal(zid) {
		t.Errorf("SCOUT ZID mismatch")
	}
	if got.Matcher != orig.Matcher {
		t.Errorf("Matcher = %#b, want %#b", got.Matcher, orig.Matcher)
	}
}

// TestScoutPackedByteLayout checks the exact byte layout of the packed byte
// against the spec figure (packed: zid_len(7:4)|I(3)|WHAT(2:0)).
func TestScoutPackedByteLayout(t *testing.T) {
	orig := &Scout{
		Version: ProtoVersion,
		Matcher: MatcherClient, // 0b100
		ZID:     ZenohID{Bytes: []byte{0xAA, 0xBB, 0xCC, 0xDD}},
	}
	w := codec.NewWriter(16)
	if err := orig.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	b := w.Bytes()
	// b[0] = header (Z=0, rest 0, ID=0x01)
	if b[0] != 0x01 {
		t.Errorf("SCOUT header = %#x, want 0x01", b[0])
	}
	// b[1] = version
	if b[1] != ProtoVersion {
		t.Errorf("version = %#x", b[1])
	}
	// b[2] = packed: zid_len=0011 (4-1), I=1, WHAT=100 → 0b0011_1100 = 0x3C
	want := byte(0b0011_1100)
	if b[2] != want {
		t.Errorf("packed = %#08b, want %#08b", b[2], want)
	}
	// b[3..6] = ZID
	for i, zb := range orig.ZID.Bytes {
		if b[3+i] != zb {
			t.Errorf("ZID byte %d = %#x, want %#x", i, b[3+i], zb)
		}
	}
}

// TestScoutWithoutZID verifies I=0 omits ZID bytes.
func TestScoutWithoutZID(t *testing.T) {
	orig := &Scout{
		Version: ProtoVersion,
		Matcher: MatcherAny,
	}
	w := codec.NewWriter(8)
	if err := orig.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	// Expect 3 bytes: header + version + packed (no ZID).
	if w.Len() != 3 {
		t.Errorf("SCOUT (no ZID) length = %d, want 3", w.Len())
	}
	// Packed byte: zid_len=0000, I=0, WHAT=111 → 0x07.
	if w.Bytes()[2] != 0x07 {
		t.Errorf("packed = %#08b, want 0b0000_0111", w.Bytes()[2])
	}
}

func TestHelloRoundtrip(t *testing.T) {
	zid := ZenohID{Bytes: []byte{0x11, 0x22, 0x33, 0x44}}
	orig := &Hello{
		Version:  ProtoVersion,
		WhatAmI:  WhatAmIRouter,
		ZID:      zid,
		Locators: []string{"tcp/localhost:7447", "tls/server:7443"},
	}
	w := codec.NewWriter(64)
	if err := orig.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	r := codec.NewReader(w.Bytes())
	h, _ := r.DecodeHeader()
	got, err := DecodeHello(r, h)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Locators) != 2 || got.Locators[0] != "tcp/localhost:7447" {
		t.Errorf("hello locators lost: %+v", got.Locators)
	}
}

// TestHelloLocatorLengthPrefixIsZ8 verifies that locator strings use a z8
// length prefix as per spec (NOT z16). Previous implementation used z16.
func TestHelloLocatorLengthPrefixIsZ8(t *testing.T) {
	orig := &Hello{
		Version:  ProtoVersion,
		WhatAmI:  WhatAmIClient,
		ZID:      ZenohID{Bytes: []byte{0xAA, 0xBB}},
		Locators: []string{"tcp/x:1"}, // 7 ASCII bytes
	}
	w := codec.NewWriter(16)
	if err := orig.EncodeTo(w); err != nil {
		t.Fatal(err)
	}
	b := w.Bytes()
	// b[0] header, b[1] version, b[2] packed, b[3..4] ZID,
	// b[5] loc_count=1 (z8 = 0x01),
	// b[6] locator length (z8) = 7, b[7..13] "tcp/x:1"
	if b[5] != 1 {
		t.Errorf("loc_count = %#x, want 1", b[5])
	}
	if b[6] != 7 {
		t.Errorf("locator length prefix = %#x, want 7 (z8)", b[6])
	}
}
