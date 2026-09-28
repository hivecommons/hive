package findingidentity

import (
	"strings"
	"testing"
)

func TestKeyStableAndVersioned(t *testing.T) {
	r := Record{SubjectDigest: "sha256:abc", Predicate: "coverage-gap", Location: "pkg/foo/bar.go"}
	k1 := Key(r)
	k2 := Key(r)
	if k1 == "" || k1 != k2 {
		t.Fatalf("key not stable: %q vs %q", k1, k2)
	}
	if !strings.HasPrefix(k1, KeyVersion+":") {
		t.Fatalf("key %q missing version prefix %q", k1, KeyVersion)
	}
	const wantLen = len(KeyVersion) + 1 + 24
	if len(k1) != wantLen {
		t.Fatalf("key length = %d, want %d", len(k1), wantLen)
	}
}

func TestKeyDistinguishesFields(t *testing.T) {
	base := Record{SubjectDigest: "sha256:abc", Predicate: "coverage-gap", Location: "pkg/foo/bar.go"}
	variants := []Record{
		{SubjectDigest: "sha256:def", Predicate: base.Predicate, Location: base.Location},
		{SubjectDigest: base.SubjectDigest, Predicate: "other", Location: base.Location},
		{SubjectDigest: base.SubjectDigest, Predicate: base.Predicate, Location: "pkg/foo/baz.go"},
	}
	baseKey := Key(base)
	for i, v := range variants {
		if Key(v) == baseKey {
			t.Errorf("variant %d collides with base", i)
		}
	}
}

func TestKeyNormalizesCaseAndWhitespace(t *testing.T) {
	a := Key(Record{SubjectDigest: "SHA256:ABC", Predicate: "  Coverage   Gap ", Location: "pkg/foo/bar.go"})
	b := Key(Record{SubjectDigest: "sha256:abc", Predicate: "coverage gap", Location: "pkg/foo/bar.go"})
	if a == "" || a != b {
		t.Fatalf("normalized records disagree: %q vs %q", a, b)
	}
}

func TestKeyIgnoresLineCoordinates(t *testing.T) {
	locations := []string{
		"pkg/foo/bar.go",
		"pkg/foo/bar.go:42",
		"pkg/foo/bar.go:42:7",
		"pkg/foo/bar.go#L42",
		"pkg/foo/bar.go#L42-L57",
		"pkg/foo/bar.go#l42",
	}
	want := Key(Record{SubjectDigest: "s", Predicate: "p", Location: locations[0]})
	for _, loc := range locations[1:] {
		if got := Key(Record{SubjectDigest: "s", Predicate: "p", Location: loc}); got != want {
			t.Errorf("location %q keys to %q, want %q", loc, got, want)
		}
	}
}

func TestKeyIncompleteRecords(t *testing.T) {
	cases := []Record{
		{},
		{SubjectDigest: "s"},
		{Predicate: "p"},
		{SubjectDigest: "s", Predicate: "p"},
		{SubjectDigest: "s", Location: "l"},
		{Predicate: "p", Location: "l"},
		{SubjectDigest: "  ", Predicate: "p", Location: "l"},
		{SubjectDigest: "s", Predicate: "\t\n", Location: "l"},
		{SubjectDigest: "s", Predicate: "p", Location: "   "},
	}
	for i, r := range cases {
		if got := Key(r); got != "" {
			t.Errorf("case %d: incomplete record keyed to %q, want empty", i, got)
		}
	}
}

func TestNormalizeLocation(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"pkg/foo/bar.go", "pkg/foo/bar.go"},
		{"pkg/foo/bar.go:12", "pkg/foo/bar.go"},
		{"pkg/foo/bar.go:12:5", "pkg/foo/bar.go"},
		{"pkg/foo/bar.go#L12", "pkg/foo/bar.go"},
		{"pkg/foo/bar.go#L12-L20", "pkg/foo/bar.go"},
		{"  pkg/foo/bar.go#L3  ", "pkg/foo/bar.go"},
		{"scope with   internal\tspaces", "scope with internal spaces"},
		// Only trailing coordinates are volatile; embedded digits stay.
		{"pkg/v2/handler.go", "pkg/v2/handler.go"},
	}
	for _, c := range cases {
		if got := NormalizeLocation(c.in); got != c.want {
			t.Errorf("NormalizeLocation(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestKeyFromFieldsPrecomputedKeyWins(t *testing.T) {
	fields := map[string]string{
		MetaKey:           " precomputed ",
		MetaSubjectDigest: "s",
		MetaPredicate:     "p",
		MetaLocation:      "l",
	}
	if got := KeyFromFields(fields); got != "precomputed" {
		t.Fatalf("KeyFromFields = %q, want trimmed precomputed key", got)
	}
	for _, alias := range []string{"finding_identity", "finding_identity_key"} {
		if got := KeyFromFields(map[string]string{alias: "k", MetaSubjectDigest: "s", MetaPredicate: "p", MetaLocation: "l"}); got != "k" {
			t.Errorf("alias %q ignored: got %q", alias, got)
		}
	}
}

func TestKeyFromFieldsComputesFromParts(t *testing.T) {
	want := Key(Record{SubjectDigest: "s", Predicate: "p", Location: "l"})
	if want == "" {
		t.Fatal("sanity: complete record must key")
	}
	canonical := map[string]string{MetaSubjectDigest: "s", MetaPredicate: "p", MetaLocation: "l"}
	if got := KeyFromFields(canonical); got != want {
		t.Fatalf("canonical fields = %q, want %q", got, want)
	}
	aliased := map[string]string{"audit_content_hash": "s", "audit_predicate": "p", "file": "l"}
	if got := KeyFromFields(aliased); got != want {
		t.Fatalf("aliased fields = %q, want %q", got, want)
	}
	// Earlier alias spellings win over later ones.
	precedence := map[string]string{"finding_subject_digest": "s", "content_hash": "other", MetaPredicate: "p", MetaLocation: "l"}
	if got := KeyFromFields(precedence); got != want {
		t.Fatalf("alias precedence = %q, want %q", got, want)
	}
}

func TestKeyFromFieldsUnidentifiable(t *testing.T) {
	cases := []map[string]string{
		nil,
		{},
		{MetaSubjectDigest: "s", MetaPredicate: "p"},
		{MetaKey: "   ", MetaSubjectDigest: "s"},
		{"unrelated": "x"},
	}
	for i, fields := range cases {
		if got := KeyFromFields(fields); got != "" {
			t.Errorf("case %d: KeyFromFields = %q, want empty", i, got)
		}
	}
}
