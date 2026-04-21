package keyexpr

import "testing"

// mustNew is the construct-or-fail helper used throughout table-driven tests.
func mustNew(t *testing.T, s string) KeyExpr {
	t.Helper()
	k, err := New(s)
	if err != nil {
		t.Fatalf("New(%q): %v", s, err)
	}
	return k
}

func TestNewCanonical(t *testing.T) {
	good := []string{
		"a",
		"a/b",
		"demo/example/*",
		"demo/**",
		"*/demo",
		"a/*/b",
		"a/*/**",
		"**",
	}
	for _, s := range good {
		if _, err := New(s); err != nil {
			t.Errorf("New(%q) should succeed: %v", s, err)
		}
	}
}

func TestNewRejectsMalformed(t *testing.T) {
	bad := []string{
		"",
		"/a",
		"a/",
		"a//b",
		"a/#/b",
		"a/?/b",
		"a/$/b",
		"a**",    // partial-chunk wildcard not supported
		"**a",    // same
		"a*b",
		"**/**",  // not canonical
		"a/**/**/b",
		"**/*",   // not canonical (need */**)
		"a/**/*",
	}
	for _, s := range bad {
		if _, err := New(s); err == nil {
			t.Errorf("New(%q) should fail", s)
		}
	}
}

func TestAutocanonize(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"a", "a"},
		{"a/b/c", "a/b/c"},
		// Collapse consecutive `**`:
		{"a/**/**/b", "a/**/b"},
		{"**/**", "**"},
		// Normalise `**/*` to `*/**`:
		{"a/**/*", "a/*/**"},
		{"**/*", "*/**"},
		// Strip empty chunks from slack paths:
		{"a//b", "a/b"},
		{"/a/b", "a/b"},
		{"a/b/", "a/b"},
	}
	for _, tt := range tests {
		got, err := Autocanonize(tt.in)
		if err != nil {
			t.Errorf("Autocanonize(%q): %v", tt.in, err)
			continue
		}
		if got.String() != tt.want {
			t.Errorf("Autocanonize(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestIntersects(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		// Literal equality
		{"a/b", "a/b", true},
		{"a/b", "a/c", false},
		// Single-chunk wildcard
		{"a/*", "a/b", true},
		{"a/*/c", "a/b/c", true},
		{"a/*/c", "a/b/d", false},
		{"a/*/c", "a/b/d/c", false}, // too many chunks
		// Double-chunk wildcard
		{"a/**", "a", true},
		{"a/**", "a/b", true},
		{"a/**", "a/b/c", true},
		{"a/**/c", "a/c", true},
		{"a/**/c", "a/b/c", true},
		{"a/**/c", "a/b/d/c", true},
		{"a/**/c", "a/b/d", false},
		// Mutual wildcards
		{"a/*", "*/b", true},  // matches "a/b"
		{"*/a", "a/*", true},  // matches "a/a"
		{"**", "**", true},
		{"**", "a/b/c/d", true},
	}
	for _, c := range cases {
		a := mustNew(t, c.a)
		b := mustNew(t, c.b)
		if got := a.Intersects(b); got != c.want {
			t.Errorf("Intersects(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
		// Intersects is symmetric.
		if got := b.Intersects(a); got != c.want {
			t.Errorf("Intersects(%q, %q) [reversed] = %v, want %v", c.b, c.a, got, c.want)
		}
	}
}

func TestIncludes(t *testing.T) {
	cases := []struct {
		bigger, smaller string
		want            bool
	}{
		// Literal
		{"a/b", "a/b", true},
		{"a/b", "a/c", false},
		// Single wild
		{"a/*", "a/b", true},
		{"a/*", "a/b/c", false},
		{"a/*/c", "a/x/c", true},
		// Double wild on bigger
		{"a/**", "a", true},
		{"a/**", "a/b", true},
		{"a/**", "a/b/c/d", true},
		{"a/**/c", "a/c", true},
		{"a/**/c", "a/b/c", true},
		{"a/**/c", "a/b/x/c", true},
		{"a/**/c", "a/c/c", true},
		// Double wild on smaller → bigger cannot include unless bigger == smaller
		{"a/*", "a/**", false},
		{"a/b", "a/**", false},
		// Different heads
		{"*/b", "a/b", true},
		{"a/b", "*/b", false},
	}
	for _, c := range cases {
		big := mustNew(t, c.bigger)
		small := mustNew(t, c.smaller)
		if got := big.Includes(small); got != c.want {
			t.Errorf("%q.Includes(%q) = %v, want %v", c.bigger, c.smaller, got, c.want)
		}
	}
}

func TestIncludesImpliesIntersects(t *testing.T) {
	// Any KE that includes another must also intersect with it.
	pairs := []struct{ a, b string }{
		{"a/**", "a/b/c"},
		{"*/b", "a/b"},
		{"a/*/c", "a/x/c"},
		{"demo/**", "demo/foo/bar"},
	}
	for _, p := range pairs {
		a := mustNew(t, p.a)
		b := mustNew(t, p.b)
		if a.Includes(b) && !a.Intersects(b) {
			t.Errorf("%q includes %q but does not intersect — inconsistent", p.a, p.b)
		}
	}
}

func TestJoin(t *testing.T) {
	k := mustNew(t, "demo")
	joined, err := k.Join("foo/*")
	if err != nil {
		t.Fatal(err)
	}
	if joined.String() != "demo/foo/*" {
		t.Errorf("Join = %q", joined)
	}

	// Join with an expression that requires autocanonisation
	k2 := mustNew(t, "a")
	joined2, err := k2.Join("**/**")
	if err != nil {
		t.Fatal(err)
	}
	if joined2.String() != "a/**" {
		t.Errorf("Join with ** collapsing: got %q, want a/**", joined2)
	}
}

func TestConcat(t *testing.T) {
	k := mustNew(t, "demo/foo")
	concat, err := k.Concat("bar")
	if err != nil {
		t.Fatal(err)
	}
	if concat.String() != "demo/foobar" {
		t.Errorf("Concat = %q, want demo/foobar", concat)
	}

	// "demo/foo" + "*" → "demo/foo*" is invalid (mixed literal+wild chunk).
	if _, err := k.Concat("*"); err == nil {
		t.Error("Concat producing invalid chunk should fail")
	}
}

// TestIntersectsSpecExamples: the exact examples from
// repo/zenoh-spec/docs/modules/concepts/pages/key-expressions.adoc.
func TestIntersectsSpecExamples(t *testing.T) {
	want := []struct {
		pattern, key string
		intersects   bool
	}{
		{"a/*/c", "a/b/c", true},
		{"a/*/c", "a/xyz/c", true},
		{"a/*/c", "a/b/d/c", false},
		{"a/**/c", "a/c", true},
		{"a/**/c", "a/b/c", true},
		{"a/**/c", "a/b/d/c", true},
	}
	for _, tt := range want {
		p := mustNew(t, tt.pattern)
		k := mustNew(t, tt.key)
		if got := p.Intersects(k); got != tt.intersects {
			t.Errorf("%q ∩ %q = %v, want %v", tt.pattern, tt.key, got, tt.intersects)
		}
	}
}

// TestAutocanonizeHandlesInterleavedStars: pathological `**/*/**` input
// requires either a fixpoint or single-pass normaliser. Regression guard
// against the earlier two-pass-without-recollapse bug.
func TestAutocanonizeHandlesInterleavedStars(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a/**/*/**", "a/*/**"},
		{"**/**/*", "*/**"},
		{"**/*/**/**", "*/**"},
		{"a/**/*/b", "a/*/**/b"},
	}
	for _, c := range cases {
		got, err := Autocanonize(c.in)
		if err != nil {
			t.Errorf("Autocanonize(%q): %v", c.in, err)
			continue
		}
		if got.String() != c.want {
			t.Errorf("Autocanonize(%q) = %q, want %q", c.in, got.String(), c.want)
		}
	}
}
