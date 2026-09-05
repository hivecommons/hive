package github

import (
	"io"
	"log/slog"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// These tests pin the pure helpers behind validatePRRequestClaims: the
// artifact-file classifiers that back title claims, the closing-reference
// downgrade rewriter that keeps multi-phase trackers open, and the repo
// resolution used for cross-repo issue lookups.

func TestIsTestFile_Classification(t *testing.T) {
	tests := []struct {
		filename string
		want     bool
	}{
		// stem suffixes
		{"src/retry_test.go", true},
		{"lib/parser_spec.rb", true},
		// basename prefixes
		{"test_sync.py", true},
		{"build-aux/test-publish.sh", true},
		// dotted infixes and bats
		{"web/app.test.tsx", true},
		{"web/app.spec.js", true},
		{"cli/smoke.bats", true},
		// directory segments
		{"test/fixtures/data.json", true},
		{"pkg/tests/helper.go", true},
		{"spec/models/user.rb", true},
		{"specs/api.yaml", true},
		{"src/__tests__/index.js", true},
		// case-insensitive, ./-prefixed, uncleaned paths
		{"./SRC/Retry_TEST.GO", true},
		{"a/../test/x.go", true},
		// negatives: near-misses must not count as tests
		{"src/main.go", false},
		{"src/contest.go", false},
		{"latest/main.go", false},
		{"docs/attestation.md", false},
		{"src/testdata/golden.json", false},
		{"protest/march.go", false},
	}
	for _, tt := range tests {
		if got := isTestFile(tt.filename); got != tt.want {
			t.Errorf("isTestFile(%q) = %v, want %v", tt.filename, got, tt.want)
		}
	}
}

func TestIsMigrationFile_Classification(t *testing.T) {
	tests := []struct {
		filename string
		want     bool
	}{
		{"db/migrations/001_users.sql", true},
		{"db/migration/001_users.sql", true},
		{"scripts/migrate/step.go", true},
		{"./DB/Migrations/002.sql", true},
		{"db/schema.sql", false},
		{"docs/migrating.md", false},
		{"src/migrator.go", false},
	}
	for _, tt := range tests {
		if got := isMigrationFile(tt.filename); got != tt.want {
			t.Errorf("isMigrationFile(%q) = %v, want %v", tt.filename, got, tt.want)
		}
	}
}

func TestIsWorkflowFile_Classification(t *testing.T) {
	tests := []struct {
		filename string
		want     bool
	}{
		{".github/workflows/ci.yml", true},
		{"./.github/workflows/release.yaml", true},
		{".github/workflows/README.md", false},
		{".github/actions/setup/action.yml", false},
		{"workflows/ci.yml", false},
	}
	for _, tt := range tests {
		if got := isWorkflowFile(tt.filename); got != tt.want {
			t.Errorf("isWorkflowFile(%q) = %v, want %v", tt.filename, got, tt.want)
		}
	}
}

func TestDowngradeClosingReferences_Rewrites(t *testing.T) {
	downgrade := map[string]string{
		claimKey("o/r", 60):        "issue is a tracker",
		claimKey("other/repo", 12): "issue has unchecked task items",
	}
	tests := []struct {
		name string
		text string
		want string
	}{
		{"empty text", "", ""},
		{
			"bare ref resolved via default repo",
			"Fixes #60 for phase one",
			"Refs #60 for phase one",
		},
		{
			"colon and casing preserved after keyword swap",
			"CLOSES: #60",
			"Refs: #60",
		},
		{
			"cross-repo ref matched case-insensitively",
			"Resolves Other/Repo#12",
			"Refs Other/Repo#12",
		},
		{
			"ref outside downgrade map untouched",
			"Fixes #61",
			"Fixes #61",
		},
		{
			"cross-repo ref to unlisted repo untouched",
			"Fixes elsewhere/repo#60",
			"Fixes elsewhere/repo#60",
		},
		{
			"non-closing mention untouched",
			"See #60 and related to #60",
			"See #60 and related to #60",
		},
		{
			"mixed refs downgraded independently",
			"Fixes #60, closes #61, resolves other/repo#12",
			"Refs #60, closes #61, Refs other/repo#12",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := downgradeClosingReferences(tt.text, "o/r", downgrade); got != tt.want {
				t.Errorf("downgradeClosingReferences(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

func TestPRRequestRepo_Resolution(t *testing.T) {
	c := NewClientForTest("http://127.0.0.1:0", "defaultorg", []string{"r"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	tests := []struct {
		repo      string
		wantOwner string
		wantRepo  string
	}{
		{"o/r", "o", "r"},
		{"  o/r  ", "o", "r"},
		{"bare-repo", "defaultorg", "bare-repo"},
		{"  bare-repo  ", "defaultorg", "bare-repo"},
		{"", "defaultorg", ""},
	}
	for _, tt := range tests {
		owner, repo := c.prRequestRepo(tt.repo)
		if owner != tt.wantOwner || repo != tt.wantRepo {
			t.Errorf("prRequestRepo(%q) = (%q, %q), want (%q, %q)",
				tt.repo, owner, repo, tt.wantOwner, tt.wantRepo)
		}
	}

	// SetOrg changes the owner used for bare repo names.
	c.SetOrg("neworg")
	if owner, _ := c.prRequestRepo("bare-repo"); owner != "neworg" {
		t.Errorf("after SetOrg, prRequestRepo owner = %q, want %q", owner, "neworg")
	}
}

func TestIncompleteIssueReason_Classification(t *testing.T) {
	strp := func(s string) *string { return &s }
	label := func(name string) *gh.Label { return &gh.Label{Name: strp(name)} }

	tests := []struct {
		name  string
		issue *gh.Issue
		want  string
	}{
		{"nil issue", nil, "issue metadata is empty"},
		{
			"tracker label",
			&gh.Issue{Title: strp("work"), Body: strp("b"), Labels: []*gh.Label{label("Tracker")}},
			"issue is labeled as a tracker or epic",
		},
		{
			"meta-tracker label",
			&gh.Issue{Title: strp("work"), Body: strp("b"), Labels: []*gh.Label{label("meta-tracker")}},
			"issue is labeled as a tracker or epic",
		},
		{
			"epic title prefix",
			&gh.Issue{Title: strp("[epic] program"), Body: strp("b")},
			"issue title marks it as a tracker or epic",
		},
		{
			"tracker title prefix after whitespace",
			&gh.Issue{Title: strp("  [Tracker] program"), Body: strp("b")},
			"issue title marks it as a tracker or epic",
		},
		{
			"unchecked task item",
			&gh.Issue{Title: strp("work"), Body: strp("- [x] done\n* [ ] remaining")},
			"issue has unchecked task items",
		},
		{
			"complete issue",
			&gh.Issue{Title: strp("focused bug"), Body: strp("one acceptance criterion")},
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := incompleteIssueReason(tt.issue); got != tt.want {
				t.Errorf("incompleteIssueReason = %q, want %q", got, tt.want)
			}
		})
	}
}
