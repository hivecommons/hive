package hub

import (
	"strings"
	"testing"
)

// ============================================================
// downsampleSpark
// ============================================================

func TestDownsampleSpark_NilAndEmpty(t *testing.T) {
	if got := downsampleSpark(nil, 5); got != nil {
		t.Errorf("nil input: got %v, want nil", got)
	}
	if got := downsampleSpark([]SparkPoint{}, 5); len(got) != 0 {
		t.Errorf("empty input: got %v, want empty", got)
	}
}

func TestDownsampleSpark_MaxZeroOrNegative(t *testing.T) {
	pts := []SparkPoint{{V: 1}, {V: 2}}
	if got := downsampleSpark(pts, 0); got != nil {
		t.Errorf("max=0: got %v, want nil", got)
	}
	if got := downsampleSpark(pts, -1); got != nil {
		t.Errorf("max=-1: got %v, want nil", got)
	}
}

func TestDownsampleSpark_BelowMax(t *testing.T) {
	pts := []SparkPoint{{V: 1}, {V: 2}, {V: 3}}
	got := downsampleSpark(pts, 10)
	if len(got) != 3 {
		t.Errorf("below max: got %d points, want 3", len(got))
	}
}

func TestDownsampleSpark_ExactMax(t *testing.T) {
	pts := []SparkPoint{{V: 1}, {V: 2}, {V: 3}}
	got := downsampleSpark(pts, 3)
	if len(got) != 3 {
		t.Errorf("exact max: got %d points, want 3", len(got))
	}
}

func TestDownsampleSpark_ReducesToMax(t *testing.T) {
	pts := make([]SparkPoint, 100)
	for i := range pts {
		pts[i] = SparkPoint{V: i}
	}
	got := downsampleSpark(pts, 10)
	if len(got) != 10 {
		t.Fatalf("got %d points, want 10", len(got))
	}
	if got[0].V != 0 {
		t.Errorf("first point V = %v, want 0", got[0].V)
	}
	if got[9].V != 99 {
		t.Errorf("last point V = %v, want 99", got[9].V)
	}
}

func TestDownsampleSpark_MaxOne(t *testing.T) {
	pts := []SparkPoint{{V: 10}, {V: 20}, {V: 30}}
	got := downsampleSpark(pts, 1)
	if len(got) != 1 {
		t.Fatalf("max=1: got %d, want 1", len(got))
	}
	if got[0].V != 10 {
		t.Errorf("max=1: V = %v, want first point (10)", got[0].V)
	}
}

// ============================================================
// sanitizeField
// ============================================================

func TestSanitizeField_Basic(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"plain text", "hello", "hello"},
		{"trims whitespace", "  hello  ", "hello"},
		{"escapes HTML", "<script>alert(1)</script>", "&lt;script&gt;alert(1)&lt;/script&gt;"},
		{"ampersand", "a & b", "a &amp; b"},
		{"quotes", `say "hi"`, "say &#34;hi&#34;"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeField(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeField(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeField_TruncatesAt200Runes(t *testing.T) {
	long := strings.Repeat("a", 250)
	got := sanitizeField(long)
	if len([]rune(got)) != 200 {
		t.Errorf("expected 200 runes, got %d", len([]rune(got)))
	}
}

func TestSanitizeField_MultibyteTruncation(t *testing.T) {
	long := strings.Repeat("\u65e5", 250)
	got := sanitizeField(long)
	if len([]rune(got)) != 200 {
		t.Errorf("multibyte: expected 200 runes, got %d", len([]rune(got)))
	}
}

// ============================================================
// normalizeForgeHost
// ============================================================

func TestNormalizeForgeHost(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		wantOK   bool
	}{
		{"github.com", "github.com", true},
		{"https://github.com", "github.com", true},
		{"https://github.com/", "github.com", true},
		{"GITHUB.COM", "github.com", true},
		{"github.ibm.com", "github.ibm.com", true},
		{"https://github.ibm.com/", "github.ibm.com", true},
		{"  https://GitHub.Enterprise.org  ", "github.enterprise.org", true},
		{"", "", false},
		{"   ", "", false},
	}
	for _, tc := range cases {
		host, ok := normalizeForgeHost(tc.in)
		if host != tc.wantHost || ok != tc.wantOK {
			t.Errorf("normalizeForgeHost(%q) = (%q, %v), want (%q, %v)", tc.in, host, ok, tc.wantHost, tc.wantOK)
		}
	}
}

// ============================================================
// normalizeOrgRef
// ============================================================

func TestNormalizeOrgRef(t *testing.T) {
	cases := []struct {
		in                string
		wantHost, wantOrg string
	}{
		{"", "", ""},
		{"  ", "", ""},
		{"myorg", "", "myorg"},
		{"github.com/myorg", "github.com", "myorg"},
		{"https://github.ibm.com/myorg", "github.ibm.com", "myorg"},
		{"http://github.ibm.com/myorg/", "github.ibm.com", "myorg"},
		{"github.com", "github.com", ""},
		{"github.ibm.com", "github.ibm.com", ""},
		{"my.org", "", "my.org"},
	}
	for _, tc := range cases {
		host, org := normalizeOrgRef(tc.in)
		if host != tc.wantHost || org != tc.wantOrg {
			t.Errorf("normalizeOrgRef(%q) = (%q, %q), want (%q, %q)", tc.in, host, org, tc.wantHost, tc.wantOrg)
		}
	}
}

// ============================================================
// normalizeProjectRef
// ============================================================

func TestNormalizeProjectRef(t *testing.T) {
	cases := []struct {
		in        string
		wantHost  string
		wantOrg   string
		wantRepos []string
	}{
		{"", "", "", nil},
		{"  ", "", "", nil},
		{"myorg/repo", "", "myorg", []string{"repo"}},
		{"https://github.com/org/repo", "github.com", "org", []string{"repo"}},
		{"github.ibm.com/org/repo.git", "github.ibm.com", "org", []string{"repo"}},
		{"https://github.com/org/repo/", "github.com", "org", []string{"repo"}},
		{"github.com", "github.com", "", nil},
		{"github.com/org", "github.com", "org", nil},
	}
	for _, tc := range cases {
		host, org, repos := normalizeProjectRef(tc.in)
		if host != tc.wantHost || org != tc.wantOrg {
			t.Errorf("normalizeProjectRef(%q) host/org = (%q, %q), want (%q, %q)", tc.in, host, org, tc.wantHost, tc.wantOrg)
		}
		if len(repos) != len(tc.wantRepos) {
			t.Errorf("normalizeProjectRef(%q) repos = %v, want %v", tc.in, repos, tc.wantRepos)
			continue
		}
		for i := range repos {
			if repos[i] != tc.wantRepos[i] {
				t.Errorf("normalizeProjectRef(%q) repos[%d] = %q, want %q", tc.in, i, repos[i], tc.wantRepos[i])
			}
		}
	}
}

// ============================================================
// validateSingleRepoHost
// ============================================================

func TestValidateSingleRepoHost_AllSameHost(t *testing.T) {
	err := validateSingleRepoHost("github.ibm.com", "github.ibm.com/org/main-repo", []string{
		"github.ibm.com/org/other",
		"bare-repo",
		"owner/repo",
	})
	if err != nil {
		t.Errorf("all same host: unexpected error: %v", err)
	}
}

func TestValidateSingleRepoHost_MismatchedRepo(t *testing.T) {
	err := validateSingleRepoHost("github.ibm.com", "", []string{
		"github.com/other-org/repo",
	})
	if err == nil {
		t.Error("expected error for mismatched host")
	}
	if !strings.Contains(err.Error(), "github.com") {
		t.Errorf("error should mention the offending host, got: %v", err)
	}
}

func TestValidateSingleRepoHost_PublicHostAllowsBareRepos(t *testing.T) {
	err := validateSingleRepoHost("", "org/repo", []string{"bare-repo", "other/thing"})
	if err != nil {
		t.Errorf("public host with bare repos: unexpected error: %v", err)
	}
}

func TestValidateSingleRepoHost_EmptyRepos(t *testing.T) {
	err := validateSingleRepoHost("github.ibm.com", "", nil)
	if err != nil {
		t.Errorf("no repos: unexpected error: %v", err)
	}
}

func TestValidateSingleRepoHost_PrimaryRepoMismatch(t *testing.T) {
	err := validateSingleRepoHost("github.ibm.com", "github.com/org/repo", nil)
	if err == nil {
		t.Error("expected error when primary repo is on different host")
	}
}

// ============================================================
// isValidRepoRef
// ============================================================

func TestIsValidRepoRef(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"repo", true},
		{"owner/repo", true},
		{"my-repo_v2.1", true},
		{"", false},
		{"a/b/c", false},
		{strings.Repeat("x", 202), false},
		{"has space", false},
		{"has:colon", false},
	}
	for _, tc := range cases {
		got := isValidRepoRef(tc.in)
		if got != tc.want {
			t.Errorf("isValidRepoRef(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// ============================================================
// sanitizeProseField
// ============================================================

func TestSanitizeProseField(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain text", "hello world", "hello world"},
		{"strips angle brackets", "hello <b>world</b>", "hello bworld/b"},
		{"strips ampersand", "a & b", "a b"},
		{"strips backticks", "use `foo`", "use foo"},
		{"newlines become spaces", "line1\nline2\rline3", "line1 line2 line3"},
		{"collapses whitespace", "too    much   space", "too much space"},
		{"trims result", "  hello  ", "hello"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeProseField(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeProseField(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeProseField_TruncatesAtMaxRunes(t *testing.T) {
	long := strings.Repeat("x", 600)
	got := sanitizeProseField(long)
	if len([]rune(got)) != maxProseFieldRunes {
		t.Errorf("expected %d runes, got %d", maxProseFieldRunes, len([]rune(got)))
	}
}

func TestSanitizeProseField_ControlCharsStripped(t *testing.T) {
	// ESC sequences and NUL should be removed
	input := "hello\x00world\x1b[31mred"
	got := sanitizeProseField(input)
	if strings.ContainsAny(got, "\x00\x1b") {
		t.Errorf("control chars not stripped: %q", got)
	}
	if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Errorf("readable text should survive: %q", got)
	}
}

// ============================================================
// sanitizeImageRef
// ============================================================

func TestSanitizeImageRef(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"ghcr.io/hivecommons/hive:v2-latest", "ghcr.io/hivecommons/hive:v2-latest"},
		{"registry:5000/org/repo:tag", "registry:5000/org/repo:tag"},
		{"repo@sha256:abc123", "repo@sha256:abc123"},
		{"has spaces and <html>", "hasspacesandhtml"},
		{"normal-image_v1.2/path", "normal-image_v1.2/path"},
		{"", ""},
	}
	for _, tc := range cases {
		got := sanitizeImageRef(tc.in)
		if got != tc.want {
			t.Errorf("sanitizeImageRef(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
