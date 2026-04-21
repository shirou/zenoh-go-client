package locator

import (
	"testing"
)

func TestParseValid(t *testing.T) {
	tests := []struct {
		in        string
		wantSchem Scheme
		wantAddr  string
	}{
		{"tcp/127.0.0.1:7447", SchemeTCP, "127.0.0.1:7447"},
		{"tcp/[::1]:7447", SchemeTCP, "[::1]:7447"},
		{"tcp/example.com:7447", SchemeTCP, "example.com:7447"},
		{"tls/host:7447", SchemeTLS, "host:7447"},
		{"quic/host:7443", SchemeQUIC, "host:7443"},
		{"ws/host:8080", SchemeWS, "host:8080"},
		{"wss/host:8443", SchemeWSS, "host:8443"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			loc, err := Parse(tt.in)
			if err != nil {
				t.Fatalf("Parse(%q) err: %v", tt.in, err)
			}
			if loc.Scheme != tt.wantSchem {
				t.Errorf("Scheme = %q, want %q", loc.Scheme, tt.wantSchem)
			}
			if loc.Address != tt.wantAddr {
				t.Errorf("Address = %q, want %q", loc.Address, tt.wantAddr)
			}
		})
	}
}

func TestParseMetadata(t *testing.T) {
	loc, err := Parse("tls/host:7447?cert_name=foo;tls_root_ca=bar")
	if err != nil {
		t.Fatal(err)
	}
	if loc.Metadata["cert_name"] != "foo" {
		t.Errorf("cert_name = %q, want foo", loc.Metadata["cert_name"])
	}
	if loc.Metadata["tls_root_ca"] != "bar" {
		t.Errorf("tls_root_ca = %q, want bar", loc.Metadata["tls_root_ca"])
	}
}

// zenoh's locator syntax does not define a '#' fragment separator. The
// parser must not try to interpret '#'; '#' inside an address or metadata
// value is passed through unchanged.
func TestParseNoFragmentHandling(t *testing.T) {
	// '#' before '?' is part of the address — but '#' is not a valid char
	// in a host:port address so validateHostPort will reject it. Test that
	// the parser doesn't silently strip it.
	if _, err := Parse("quic/host:7443#a=1"); err == nil {
		t.Error("Parse should reject '#' in address")
	}
}

func TestParseInvalid(t *testing.T) {
	cases := []string{
		"",
		"tcp",                 // missing slash
		"tcp/",                // empty address
		"tcp/host",            // missing port
		"tcp/host:0",          // port 0
		"tcp/:7447",           // empty host
		"unknown-scheme/addr", // unknown scheme
		"tcp/::1:7447",        // IPv6 must be bracketed
	}
	for _, c := range cases {
		if _, err := Parse(c); err == nil {
			t.Errorf("Parse(%q) should have failed", c)
		}
	}
}

func TestLocatorString(t *testing.T) {
	loc, _ := Parse("tcp/[::1]:7447?cert=foo")
	want := "tcp/[::1]:7447"
	if got := loc.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
