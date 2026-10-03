package worksource

import (
	"strings"
	"testing"
)

// TestDisplayNeverRendersZero pins the human-facing short form: "#42" for
// GitHub-backed work, the native key for a string-keyed source, and never the
// fabricated "#0" this package exists to prevent.
func TestDisplayNeverRendersZero(t *testing.T) {
	if got := (Ref{Repo: "acme/repo", Number: 42}).Display(); got != "#42" {
		t.Errorf("github display = %q, want #42", got)
	}
	if got := (Ref{Repo: "acme/repo", ExternalID: "ENG-123", SourceType: "linear"}).Display(); got != "ENG-123" {
		t.Errorf("external display = %q, want the native key", got)
	}
	if got := (Ref{Repo: "acme/repo"}).Display(); got != "" {
		t.Errorf("unidentifiable item display = %q, want empty", got)
	}
}

// TestParseKeyRoundTripsBothShapes proves the reader half of Key() agrees with
// the writer for both the GitHub "#" form and the external "!" form — what lets
// persisted operator hold/order entries survive a restart without a migration.
func TestParseKeyRoundTripsBothShapes(t *testing.T) {
	for _, ref := range []Ref{
		{Repo: "acme/repo", Number: 42, ExternalID: "42"},
		{Repo: "acme/repo", ExternalID: "ENG-123"},
		{SourceType: SourceTypeRun, Repo: "acme/repo", ExternalID: "run-123:spec"},
	} {
		key := ref.Key()
		parsed, ok := ParseKey(key)
		if !ok {
			t.Fatalf("ParseKey(%q) failed", key)
		}
		if parsed.Key() != key {
			t.Errorf("round trip changed the key: %q -> %q", key, parsed.Key())
		}
		if parsed.IsGitHubIssue() != ref.IsGitHubIssue() {
			t.Errorf("round trip changed GitHub-backedness for %q", key)
		}
	}
}

func TestRunStageRefRoundTrip(t *testing.T) {
	ref := Ref{SourceType: SourceTypeRun, Repo: "acme/repo", ExternalID: "run-123:plan"}
	key := ref.Key()
	if key != "acme/repo!run-123:plan" {
		t.Fatalf("run key = %q", key)
	}
	parsed, ok := ParseKey(key)
	if !ok {
		t.Fatalf("ParseKey(%q) failed", key)
	}
	if !parsed.IsRunStage() {
		t.Fatalf("parsed run stage not recognized: %+v", parsed)
	}
	if parsed.Key() != key {
		t.Fatalf("round trip = %q", parsed.Key())
	}
}

func TestRunStageLeaseKeyWithIssueShapedRunKeyRoundTrips(t *testing.T) {
	key := "acme/repo!acme/repo#8533:spec"
	parsed, ok := ParseKey(key)
	if !ok {
		t.Fatalf("ParseKey(%q) failed", key)
	}
	if !parsed.IsRunStage() {
		t.Fatalf("parsed run stage not recognized: %+v", parsed)
	}
	if parsed.Key() != key {
		t.Fatalf("round trip = %q", parsed.Key())
	}
}

// TestParseKeyRefusesMalformedInput pins every rejection branch: a malformed or
// hand-edited operator entry must be skipped, never matched against the wrong
// item — and "repo#0" is exactly the fabricated identity that must be refused.
func TestParseKeyRefusesMalformedInput(t *testing.T) {
	for _, raw := range []string{
		"",
		"   ",
		"acme/repo",     // no separator at all
		"acme/repo#0",   // the fabricated identity
		"acme/repo#-3",  // negative number
		"acme/repo#abc", // non-numeric number
		"#42",           // no repository (github form)
		"!ENG-1",        // no repository (external form)
		"acme/repo!",    // no external id
		"acme/repo!   ", // whitespace external id
	} {
		if ref, ok := ParseKey(raw); ok {
			t.Errorf("ParseKey(%q) accepted a malformed key as %+v", raw, ref)
		}
	}

	// Surrounding whitespace on a well-formed key is tolerated.
	if ref, ok := ParseKey("  acme/repo#7 "); !ok || ref.Key() != "acme/repo#7" {
		t.Errorf("ParseKey should trim surrounding whitespace, got %+v ok=%v", ref, ok)
	}
	if ref, ok := ParseKey(" acme/repo!ENG-9 "); !ok || ref.Key() != "acme/repo!ENG-9" {
		t.Errorf("ParseKey should trim the external form too, got %+v ok=%v", ref, ok)
	}
}

func TestRefArtifactSlug(t *testing.T) {
	cases := map[string]string{
		"Owner/Repo#42":      "owner-repo-42",
		"owner/repo!ENG-123": "owner-repo-eng-123",
		"x/y#7":              "x-y-7",
	}
	for in, want := range cases {
		ref, ok := ParseKey(in)
		if !ok {
			t.Fatalf("ParseKey(%q) failed", in)
		}
		if got := ref.ArtifactSlug(); got != want {
			t.Errorf("ArtifactSlug(%q) = %q, want %q", in, got, want)
		}
	}
	if got := (Ref{}).ArtifactSlug(); got != "run" {
		t.Errorf("empty ref slug = %q, want run", got)
	}
}

func TestCapArtifactSlug(t *testing.T) {
	short := "owner-repo-42"
	if got := CapArtifactSlug(short); got != short {
		t.Errorf("short slug changed: %q", got)
	}
	long := strings.Repeat("a", 70) + "-42"
	got := CapArtifactSlug(long)
	if len(got) != MaxArtifactSlugLen || !strings.HasSuffix(got, "-42") {
		t.Errorf("CapArtifactSlug = %q (len %d), want %d chars ending -42", got, len(got), MaxArtifactSlugLen)
	}
	ref, ok := ParseKey("owner/" + strings.Repeat("r", 80) + "#7")
	if !ok {
		t.Fatal("ParseKey failed")
	}
	if got := ref.ArtifactSlug(); len(got) != MaxArtifactSlugLen || !strings.HasSuffix(got, "-7") {
		t.Errorf("ArtifactSlug = %q (len %d)", got, len(got))
	}
}
