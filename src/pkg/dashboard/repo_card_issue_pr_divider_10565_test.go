package dashboard

import (
	"strings"
	"testing"
)

// #10565: issue and PR bands can share operator-facing labels such as
// "Needs human" and "Open". The repo card therefore labels the two runs before
// the repeated bucket headers so a PR bucket is not mistaken for another issue
// bucket.
func TestRepoCardIssuePRDividerStaticWiring10565(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		".repo-pill-run-title { color: var(--muted); font-size: var(--fs-2xs); font-weight: 800; letter-spacing: 0.08em; text-transform: uppercase;",
		"const issueCol = (issuePills || nonActionableIssuePills) ? `<div class=\"repo-pill-run-title\">ISSUES</div>${issuePills}${nonActionableIssuePills}` : '';",
		"const prCol = prPills ? `<div class=\"repo-pill-run-title\">PULL REQUESTS</div>${prPills}` : '';",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}

	prDivider := strings.Index(html, "const prCol = prPills ? `<div class=\"repo-pill-run-title\">PULL REQUESTS</div>${prPills}` : '';")
	prBands := strings.Index(html, "const prPills = groupedRepoPRs(r.openPrs || [], r.heldPrs || []).map(g => {")
	pillsHTML := strings.Index(html, "? `<div class=\"repo-pills${pillColClass}\"><div class=\"repo-pill-col repo-pill-col-issues\">${issueCol}</div><div class=\"repo-pill-col repo-pill-col-prs\">${prCol}</div></div>`")
	if prDivider < 0 || prBands < 0 || pillsHTML < 0 {
		t.Fatalf("index.html missing PR divider, PR buckets, or repo pill columns")
	}
	if prDivider <= prBands {
		t.Fatal("the PULL REQUESTS divider must be composed after PR buckets are known")
	}
	if pillsHTML <= prDivider {
		t.Fatal("the PR column must render the PULL REQUESTS divider before the PR bucket HTML")
	}
}
