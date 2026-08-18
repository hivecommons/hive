package advisory

import (
	"strings"
	"testing"
)

// TestVerifyFindingPathsFlagsMissingFile is the direct regression guard for
// #3704: a finding whose file path no longer exists at the analyzed commit must
// be flagged PathStale, while findings for paths that do exist are left alone
// and GitHub issue/PR refs are never checked at all.
func TestVerifyFindingPathsFlagsMissingFile(t *testing.T) {
	findings := []Finding{
		// The #3704 case: cited path is absent at the analyzed commit.
		{Agent: "scanner", Severity: "high", Type: "advisory",
			Title: "install.md does not document the launcher image OCI repo",
			File:  "docs/install.md"},
		// A path that still exists.
		{Agent: "quality", Severity: "medium", Type: "perf",
			Title: "slow loop", File: "pkg/a.go", Line: 12},
		// A GitHub issue ref — must never be treated as a file path.
		{Agent: "scanner", Severity: "low", Type: "bug",
			Title: "requester explosion", File: "gh-42"},
	}
	d := BuildDigest(findings, "busy")
	d.AnalyzedSnapshot = &Snapshot{
		Owner: "acme", Repo: "widgets", Branch: "main",
		SHA: "8a804806a1306f01a911abefb1769c49f000eb35",
	}

	var checkedPaths []string
	VerifyFindingPaths(d, func(path string) bool {
		checkedPaths = append(checkedPaths, path)
		return path != "docs/install.md" // docs/install.md is gone
	})

	// gh-42 must not have been checked; the two real file paths must have.
	for _, p := range checkedPaths {
		if p == "gh-42" {
			t.Errorf("gh-42 was checked as a file path; it is a GitHub ref")
		}
	}
	if !containsStr(checkedPaths, "docs/install.md") || !containsStr(checkedPaths, "pkg/a.go") {
		t.Errorf("expected both file paths checked, got %v", checkedPaths)
	}

	got := map[string]bool{}
	for _, fs := range d.ByAgent {
		for _, f := range fs {
			got[f.Title] = f.PathStale
		}
	}
	if !got["install.md does not document the launcher image OCI repo"] {
		t.Error("missing docs/install.md finding should be flagged PathStale")
	}
	if got["slow loop"] {
		t.Error("existing pkg/a.go finding must not be flagged PathStale")
	}
	if got["requester explosion"] {
		t.Error("gh-ref finding must never be flagged PathStale")
	}
}

// TestFormatDigestMarkdownRendersStalePathAsOutdated verifies the renderer drops
// the dead file link for a PathStale finding and marks it outdated instead of
// emitting a `docs/install.md`-style code span (#3704).
func TestFormatDigestMarkdownRendersStalePathAsOutdated(t *testing.T) {
	findings := []Finding{
		{Agent: "scanner", Severity: "high", Type: "advisory",
			Title: "install.md missing launcher OCI repo", File: "docs/install.md", PathStale: true},
	}
	d := BuildDigest(findings, "busy")
	md := FormatDigestMarkdown(d, "acme", "widgets")

	if strings.Contains(md, "`docs/install.md`") {
		t.Errorf("stale file path was rendered as a live code span:\n%s", md)
	}
	if !strings.Contains(md, "not found at analyzed commit") {
		t.Errorf("stale finding not marked as outdated:\n%s", md)
	}
}

// TestFormatDigestMarkdownCitesAnalyzedSnapshot verifies invariant 2: the exact
// analyzed commit is cited in the rendered comment.
func TestFormatDigestMarkdownCitesAnalyzedSnapshot(t *testing.T) {
	findings := []Finding{
		{Agent: "scanner", Severity: "high", Type: "bug", Title: "some bug", File: "pkg/a.go", Line: 3},
	}
	d := BuildDigest(findings, "busy")
	sha := "8a804806a1306f01a911abefb1769c49f000eb35"
	d.AnalyzedSnapshot = &Snapshot{Owner: "acme", Repo: "widgets", Branch: "main", SHA: sha}

	md := FormatDigestMarkdown(d, "acme", "widgets")

	if !strings.Contains(md, "Analyzed at") {
		t.Errorf("digest does not cite analyzed snapshot:\n%s", md)
	}
	if !strings.Contains(md, sha[:12]) {
		t.Errorf("digest does not cite the analyzed SHA %s:\n%s", sha[:12], md)
	}
	if !strings.Contains(md, "https://github.com/acme/widgets/commit/"+sha) {
		t.Errorf("digest does not link the analyzed commit:\n%s", md)
	}
}

// TestFormatDigestMarkdownNoSnapshotNoFooter confirms backward compatibility:
// with no AnalyzedSnapshot, no citation footer is emitted.
func TestFormatDigestMarkdownNoSnapshotNoFooter(t *testing.T) {
	findings := []Finding{
		{Agent: "scanner", Severity: "high", Type: "bug", Title: "some bug"},
	}
	d := BuildDigest(findings, "busy")
	md := FormatDigestMarkdown(d, "acme", "widgets")
	if strings.Contains(md, "Analyzed at") {
		t.Errorf("no snapshot set but footer was rendered:\n%s", md)
	}
}

// TestVerifyFindingPathsNoSnapshotNoOp confirms VerifyFindingPaths is inert when
// no snapshot is pinned (older/unpinned callers keep prior behavior).
func TestVerifyFindingPathsNoSnapshotNoOp(t *testing.T) {
	findings := []Finding{{Agent: "scanner", Title: "x", File: "docs/install.md"}}
	d := BuildDigest(findings, "busy")
	called := false
	VerifyFindingPaths(d, func(string) bool { called = true; return false })
	if called {
		t.Error("exists callback invoked with no AnalyzedSnapshot set")
	}
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
