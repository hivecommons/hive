package hub

import (
	"context"
	"strings"
	"testing"
)

// Tests for server.go utility functions that have NO existing dedicated test coverage.
// Functions already tested elsewhere (secureCompareHub, clampInt, clampInt64,
// sanitizeHeartbeatField, sanitizeRepoEntry, isPrivateURL, repoRefHost,
// sameGitHubHost, repoDisplayLine) are omitted to avoid redeclaration.

// --- downsampleSpark ---

func TestDownsampleSpark_NilInput(t *testing.T) {
	got := downsampleSpark(nil, 10)
	if got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestDownsampleSpark_MaxZero(t *testing.T) {
	pts := []SparkPoint{{T: 1, V: 1}}
	got := downsampleSpark(pts, 0)
	if got != nil {
		t.Fatalf("expected nil for max=0, got %v", got)
	}
}

func TestDownsampleSpark_MaxNegative(t *testing.T) {
	pts := []SparkPoint{{T: 1, V: 1}}
	got := downsampleSpark(pts, -5)
	if got != nil {
		t.Fatalf("expected nil for max<0, got %v", got)
	}
}

func TestDownsampleSpark_UnderMax(t *testing.T) {
	pts := []SparkPoint{{T: 1, V: 10}, {T: 2, V: 20}, {T: 3, V: 30}}
	got := downsampleSpark(pts, 5)
	if len(got) != 3 {
		t.Fatalf("expected 3 points unchanged, got %d", len(got))
	}
}

func TestDownsampleSpark_ExactMax(t *testing.T) {
	pts := []SparkPoint{{T: 1, V: 10}, {T: 2, V: 20}, {T: 3, V: 30}}
	got := downsampleSpark(pts, 3)
	if len(got) != 3 {
		t.Fatalf("expected 3 points unchanged, got %d", len(got))
	}
}

func TestDownsampleSpark_DownsampleEndpoints(t *testing.T) {
	pts := make([]SparkPoint, 100)
	for i := range pts {
		pts[i] = SparkPoint{T: int64(i), V: i * 10}
	}
	got := downsampleSpark(pts, 10)
	if len(got) != 10 {
		t.Fatalf("expected 10 samples, got %d", len(got))
	}
	if got[0].T != 0 {
		t.Errorf("first sample should be index 0, got T=%d", got[0].T)
	}
	if got[9].T != 99 {
		t.Errorf("last sample should be index 99, got T=%d", got[9].T)
	}
}

func TestDownsampleSpark_MaxOne(t *testing.T) {
	pts := []SparkPoint{{T: 1, V: 10}, {T: 2, V: 20}, {T: 3, V: 30}}
	got := downsampleSpark(pts, 1)
	if len(got) != 1 {
		t.Fatalf("expected 1 sample, got %d", len(got))
	}
}

// --- sanitizeField ---

func TestSanitizeField_TrimSpace(t *testing.T) {
	got := sanitizeField("  hello  ")
	if got != "hello" {
		t.Fatalf("expected 'hello', got %q", got)
	}
}

func TestSanitizeField_HTMLEscape(t *testing.T) {
	got := sanitizeField("<script>alert('xss')</script>")
	if strings.Contains(got, "<") || strings.Contains(got, ">") {
		t.Fatalf("expected HTML escaping, got %q", got)
	}
}

func TestSanitizeField_Truncation(t *testing.T) {
	long := strings.Repeat("x", 300)
	got := sanitizeField(long)
	if len([]rune(got)) > 200 {
		t.Fatalf("expected max 200 runes, got %d", len([]rune(got)))
	}
}

// --- isValidName ---

func TestIsValidName_Table(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"hello", true},
		{"my-org.repo_v2", true},
		{"a", true},
		{"", false},
		{"has space", false},
		{"has/slash", false},
		{"has:colon", false},
		{strings.Repeat("x", 100), true},
		{strings.Repeat("x", 101), false},
	}
	for _, tc := range cases {
		if got := isValidName(tc.input); got != tc.want {
			t.Errorf("isValidName(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

// --- normalizeForgeHost ---

func TestNormalizeForgeHost_Table(t *testing.T) {
	cases := []struct {
		input    string
		wantHost string
		wantOK   bool
	}{
		{"github.com", "github.com", true},
		{"https://github.com", "github.com", true},
		{"github.ibm.com", "github.ibm.com", true},
		{"https://github.ibm.com/", "github.ibm.com", true},
		{"", "", false},
		{"   ", "", false},
		{"github", "github", false},
		{"..", "..", false},
	}
	for _, tc := range cases {
		host, ok := normalizeForgeHost(tc.input)
		if host != tc.wantHost || ok != tc.wantOK {
			t.Errorf("normalizeForgeHost(%q) = (%q, %v), want (%q, %v)", tc.input, host, ok, tc.wantHost, tc.wantOK)
		}
	}
}

// --- normalizeOrgRef ---

func TestNormalizeOrgRef_Table(t *testing.T) {
	cases := []struct {
		input    string
		wantHost string
		wantOrg  string
	}{
		{"https://github.ibm.com/z-aiops-unite", "github.ibm.com", "z-aiops-unite"},
		{"github.ibm.com/z-aiops-unite", "github.ibm.com", "z-aiops-unite"},
		{"z-aiops-unite", "", "z-aiops-unite"},
		{"https://github.ibm.com", "github.ibm.com", ""},
		{"github.ibm.com", "github.ibm.com", ""},
		{"", "", ""},
	}
	for _, tc := range cases {
		host, org := normalizeOrgRef(tc.input)
		if host != tc.wantHost || org != tc.wantOrg {
			t.Errorf("normalizeOrgRef(%q) = (%q, %q), want (%q, %q)", tc.input, host, org, tc.wantHost, tc.wantOrg)
		}
	}
}

// --- normalizeProjectRef ---

func TestNormalizeProjectRef_Table(t *testing.T) {
	cases := []struct {
		input     string
		wantHost  string
		wantOrg   string
		wantRepos []string
	}{
		{"github.ibm.com/org/repo", "github.ibm.com", "org", []string{"repo"}},
		{"https://github.com/myorg/myrepo", "github.com", "myorg", []string{"myrepo"}},
		{"myorg/myrepo", "", "myorg", []string{"myrepo"}},
		{"myrepo", "", "myrepo", nil},
		{"", "", "", nil},
		{"https://github.com/org/repo.git", "github.com", "org", []string{"repo"}},
	}
	for _, tc := range cases {
		host, org, repos := normalizeProjectRef(tc.input)
		if host != tc.wantHost || org != tc.wantOrg {
			t.Errorf("normalizeProjectRef(%q) host/org = (%q, %q), want (%q, %q)", tc.input, host, org, tc.wantHost, tc.wantOrg)
		}
		if len(repos) != len(tc.wantRepos) {
			t.Errorf("normalizeProjectRef(%q) repos = %v, want %v", tc.input, repos, tc.wantRepos)
		}
	}
}

// --- firstCSV / replaceFirstCSV ---

func TestFirstCSV_Table(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"a,b,c", "a"},
		{" , , x", "x"},
		{"", ""},
		{"  hello  ", "hello"},
	}
	for _, tc := range cases {
		if got := firstCSV(tc.input); got != tc.want {
			t.Errorf("firstCSV(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestReplaceFirstCSV_Table(t *testing.T) {
	cases := []struct {
		s, first, want string
	}{
		{"a,b,c", "X", "X,b,c"},
		{" , ,c", "X", " , ,X"},
		{"a,b,c", "", "b,c"},
		{"", "X", "X"},
	}
	for _, tc := range cases {
		if got := replaceFirstCSV(tc.s, tc.first); got != tc.want {
			t.Errorf("replaceFirstCSV(%q, %q) = %q, want %q", tc.s, tc.first, got, tc.want)
		}
	}
}

// --- looksLikeGitHubForgeHost ---

func TestLooksLikeGitHubForgeHost_Table(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"github.com", true},
		{"github.ibm.com", true},
		{"GITHUB.COM", true},
		{"github.", false},
		{"github", false},
		{"gitlab.com", false},
		{"my.org", false},
		{"github.ibm.com.", false},
	}
	for _, tc := range cases {
		if got := looksLikeGitHubForgeHost(tc.input); got != tc.want {
			t.Errorf("looksLikeGitHubForgeHost(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

// --- normalizeRepoRef ---

func TestNormalizeRepoRef_Table(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"https://github.ibm.com/z-aiops-unite/ui", "ui"},
		{"z-aiops-unite/ui", "ui"},
		{"ui", "ui"},
		{"", ""},
		{"repo.git", "repo"},
	}
	for _, tc := range cases {
		if got := normalizeRepoRef(tc.input); got != tc.want {
			t.Errorf("normalizeRepoRef(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// --- validateSingleRepoHost ---

func TestValidateSingleRepoHost_AllSameHost(t *testing.T) {
	err := validateSingleRepoHost("github.ibm.com", "github.ibm.com/org/repo", []string{"github.ibm.com/org/other"})
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestValidateSingleRepoHost_Mismatch(t *testing.T) {
	err := validateSingleRepoHost("github.ibm.com", "github.com/org/repo", nil)
	if err == nil {
		t.Fatal("expected error for mismatched host")
	}
}

func TestValidateSingleRepoHost_BareRepoNoError(t *testing.T) {
	err := validateSingleRepoHost("github.ibm.com", "repo", []string{"other-repo"})
	if err != nil {
		t.Fatalf("expected nil for bare repos, got %v", err)
	}
}

// --- isValidRepoRef ---

func TestIsValidRepoRef_Table(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"repo", true},
		{"owner/repo", true},
		{"a/b/c", false},
		{"", false},
		{"has space", false},
		{strings.Repeat("x", 202), false},
	}
	for _, tc := range cases {
		if got := isValidRepoRef(tc.input); got != tc.want {
			t.Errorf("isValidRepoRef(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

// --- clampFleetCount ---

func TestClampFleetCount_NilReturnsNil(t *testing.T) {
	if got := clampFleetCount(nil); got != nil {
		t.Fatal("nil input should return nil")
	}
}

func TestClampFleetCount_NegativeReturnsNil(t *testing.T) {
	v := -1
	if got := clampFleetCount(&v); got != nil {
		t.Fatal("negative input should return nil")
	}
}

func TestClampFleetCount_ValidPassthrough(t *testing.T) {
	v := 42
	got := clampFleetCount(&v)
	if got == nil || *got != 42 {
		t.Fatalf("expected 42, got %v", got)
	}
}

func TestClampFleetCount_ClampsOverMax(t *testing.T) {
	v := maxFleetCount + 1
	got := clampFleetCount(&v)
	if got == nil || *got != maxFleetCount {
		t.Fatalf("expected %d, got %v", maxFleetCount, got)
	}
}

// --- sanitizeProseField ---

func TestSanitizeProseField_PreservesNormalText(t *testing.T) {
	got := sanitizeProseField("Hello, world!")
	if got != "Hello, world!" {
		t.Errorf("expected 'Hello, world!', got %q", got)
	}
}

func TestSanitizeProseField_StripsHTMLChars(t *testing.T) {
	got := sanitizeProseField("<b>bold</b> & stuff")
	if strings.ContainsAny(got, "<>&") {
		t.Errorf("should strip HTML chars, got %q", got)
	}
}

func TestSanitizeProseField_CollapsesWhitespace(t *testing.T) {
	got := sanitizeProseField("line1\n\nline2\ttab")
	if strings.Contains(got, "\n") || strings.Contains(got, "\t") {
		t.Errorf("should collapse whitespace, got %q", got)
	}
	if !strings.Contains(got, "line1") || !strings.Contains(got, "line2") {
		t.Errorf("should keep content, got %q", got)
	}
}

func TestSanitizeProseField_TruncatesLong(t *testing.T) {
	long := strings.Repeat("x", 600)
	got := sanitizeProseField(long)
	if len([]rune(got)) > maxProseFieldRunes {
		t.Fatalf("expected max %d runes, got %d", maxProseFieldRunes, len([]rune(got)))
	}
}

func TestSanitizeProseField_DropsBackticks(t *testing.T) {
	got := sanitizeProseField("use `code` here")
	if strings.Contains(got, "`") {
		t.Errorf("should drop backticks, got %q", got)
	}
}

func TestSanitizeProseField_DropsControlChars(t *testing.T) {
	got := sanitizeProseField("hello\x1b[31mred\x1b[0m world")
	if strings.Contains(got, "\x1b") {
		t.Errorf("should drop ESC sequences, got %q", got)
	}
}

// --- sanitizeImageRef ---

func TestSanitizeImageRef_ValidRef(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"ghcr.io/kubestellar/hive:v2-latest", "ghcr.io/kubestellar/hive:v2-latest"},
		{"registry:5000/img@sha256:abc123", "registry:5000/img@sha256:abc123"},
		{"img<script>", "imgscript"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := sanitizeImageRef(tc.input); got != tc.want {
			t.Errorf("sanitizeImageRef(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// --- gheAPIURLForHost ---

func TestGheAPIURLForHost_Table(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"github.ibm.com", "https://github.ibm.com/api/v3"},
		{"github.com", ""},
		{"", ""},
		{"api.github.com", ""},
		{"GITHUB.IBM.COM", "https://github.ibm.com/api/v3"},
	}
	for _, tc := range cases {
		if got := gheAPIURLForHost(tc.input); got != tc.want {
			t.Errorf("gheAPIURLForHost(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// --- isPrivateURL (extended cases not in hub_test.go) ---

func TestIsPrivateURL_VariousSchemes(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		input string
		want  bool
	}{
		{"wss://localhost/ws", true},
		{"ws://127.0.0.1/ws", true},
		{"https://172.16.0.1/internal", true},
		{"http://[::ffff:127.0.0.1]/path", true},
	}
	for _, tc := range cases {
		if got := isPrivateURL(ctx, tc.input); got != tc.want {
			t.Errorf("isPrivateURL(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

// --- isAvailableRegistryEntry ---

func TestIsAvailableRegistryEntry_ByStatus(t *testing.T) {
	e := RegistryEntry{ProvStatus: statusAvailable}
	if !isAvailableRegistryEntry(e) {
		t.Fatal("expected true for statusAvailable")
	}
}

func TestIsAvailableRegistryEntry_ByPlaceholderOrg(t *testing.T) {
	e := RegistryEntry{Org: placeholderOrgPrefix + "test"}
	if !isAvailableRegistryEntry(e) {
		t.Fatal("expected true for placeholder org prefix")
	}
}

func TestIsAvailableRegistryEntry_Claimed(t *testing.T) {
	e := RegistryEntry{ProvStatus: "claimed", Org: "real-org"}
	if isAvailableRegistryEntry(e) {
		t.Fatal("expected false for claimed entry")
	}
}
