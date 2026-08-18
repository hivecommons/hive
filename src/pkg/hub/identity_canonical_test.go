package hub

import "testing"

func TestMakeCanonical(t *testing.T) {
	cases := []struct {
		provider, subject, want string
		ok                      bool
	}{
		{"github", "clubanderson", "github:clubanderson", true},
		{"google", "1078xxxx", "google:1078xxxx", true},
		{"GitHub", "Foo-Bar", "github:Foo-Bar", true}, // provider lowercased
		{"ibmid", "abc.def", "ibmid:abc.def", true},   // dot allowed in subject (wire form)
		{"redhat", "s_1", "redhat:s_1", true},
		{"unknown", "x", "", false},  // unknown provider
		{"github", "", "", false},    // empty subject
		{"github", "a:b", "", false}, // separator in subject
		{"github", "a/b", "", false}, // path char
		{"github", "..", "", false},  // traversal
	}
	for _, c := range cases {
		got, err := makeCanonical(c.provider, c.subject)
		if c.ok {
			if err != nil || got != c.want {
				t.Errorf("makeCanonical(%q,%q) = %q,%v want %q", c.provider, c.subject, got, err, c.want)
			}
		} else if err == nil {
			t.Errorf("makeCanonical(%q,%q) = %q, want error", c.provider, c.subject, got)
		}
	}
}

func TestParseCanonicalAndLegacyShim(t *testing.T) {
	// Bare login → legacy github (the shim).
	if p, s, ok := parseCanonical("clubanderson"); !ok || p != "github" || s != "clubanderson" {
		t.Errorf("parseCanonical(bare) = %q,%q,%v", p, s, ok)
	}
	// Already-canonical passes through.
	if p, s, ok := parseCanonical("google:1078"); !ok || p != "google" || s != "1078" {
		t.Errorf("parseCanonical(canonical) = %q,%q,%v", p, s, ok)
	}
	// Unknown provider → not ok.
	if _, _, ok := parseCanonical("bogus:x"); ok {
		t.Error("parseCanonical(bogus:x) should be !ok")
	}
	// canonicalizeLegacy.
	for in, want := range map[string]string{
		"clubanderson":        "github:clubanderson",
		"github:clubanderson": "github:clubanderson",
		"google:1078":         "google:1078",
		"":                    "",
	} {
		if got := canonicalizeLegacy(in); got != want {
			t.Errorf("canonicalizeLegacy(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCanonicalEqual is the load-bearing migration property: a legacy stored
// value matches an authenticated canonical id, and — critically — a same-subject
// identity on a DIFFERENT provider does NOT match (cross-provider defense).
func TestCanonicalEqual(t *testing.T) {
	if !canonicalEqual("clubanderson", "github:clubanderson") {
		t.Error("legacy bare login must equal github: canonical")
	}
	if !canonicalEqual("GitHub:ClubAnderson", "github:clubanderson") {
		t.Error("github identity compare must be case-insensitive")
	}
	if canonicalEqual("google:clubanderson", "github:clubanderson") {
		t.Fatal("SECURITY: google:clubanderson must NOT equal github:clubanderson")
	}
	if canonicalEqual("google:clubanderson", "clubanderson") {
		t.Fatal("SECURITY: google:clubanderson must NOT equal a legacy github login")
	}
}

func TestFilenameRoundTrip(t *testing.T) {
	ids := []string{
		"github:clubanderson",
		"github:foo-bar",
		"github:foo.bar",
		"google:1078901234567890",
		"ibmid:ABCDEF-0123",
		"redhat:user_name",
		"github:name+with@odd.chars", // exercises the -XX- escape
	}
	for _, id := range ids {
		stem, err := encodeUserFilename(id)
		if err != nil {
			t.Fatalf("encodeUserFilename(%q): %v", id, err)
		}
		if !isValidName(stem) {
			t.Errorf("encoded stem %q for %q is not path-safe", stem, id)
		}
		back, ok := decodeUserFilename(stem)
		if !ok {
			t.Fatalf("decodeUserFilename(%q) not ok (from %q)", stem, id)
		}
		// makeCanonical was applied on encode side (parseCanonical) so compare canonical forms.
		if canonicalizeLegacy(back) != canonicalizeLegacy(id) {
			t.Errorf("round-trip: %q -> %q -> %q", id, stem, back)
		}
	}
}

// TestLegacyFilenameStemMatches pins that a bare GitHub login encodes to a stem
// whose provider is github and subject is the login — so the load path can map a
// canonical github id to a file while a legacy "<login>.json" still resolves via
// the dual-read fallback (asserted here structurally).
func TestLegacyFilenameStemMatches(t *testing.T) {
	stem, err := encodeUserFilename("clubanderson") // bare → github:clubanderson
	if err != nil {
		t.Fatal(err)
	}
	if stem != "github.clubanderson" {
		t.Errorf("bare login stem = %q, want github.clubanderson", stem)
	}
	back, ok := decodeUserFilename(stem)
	if !ok || back != "github:clubanderson" {
		t.Errorf("decode(%q) = %q,%v", stem, back, ok)
	}
	// A pre-migration legacy stem (no provider dot in the github sense) with a
	// dot in the login must NOT be misread as a provider split.
	if _, ok := decodeUserFilename("someuser"); ok {
		t.Error("a bare legacy stem with no provider prefix must decode !ok (caller falls back to legacy github)")
	}
}
