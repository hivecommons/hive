package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// Execute the shipped formatter so the repository cards' visible labels are
// pinned to the structured server counts, including zero actionable work.
func TestRepoWorkBreakdownRendering(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: repository breakdown rendering was not executed")
	}
	script := jsFunc(t, indexHTML(t), "formatRepoWorkBreakdown") + `
const assert = require('node:assert/strict');
let html = formatRepoWorkBreakdown({ actionable: 0, dependency_dashboard: 1 }, 1, 'issues');
assert.match(html, /0 actionable<\/span> · <span[^>]*>1 dependency dashboard/);
html = formatRepoWorkBreakdown({ actionable: 0, hive_advisory: 1 }, 1, 'issues');
assert.match(html, /0 actionable<\/span> · <span[^>]*>1 advisory/);
html = formatRepoWorkBreakdown({ actionable: 1 }, 1, 'issues');
assert.match(html, />1 actionable<\/span>/);
html = formatRepoWorkBreakdown({ actionable: 0, hold: 1 }, 1, 'prs');
assert.match(html, /0 actionable<\/span> · <span[^>]*>1 hold/);
html = formatRepoWorkBreakdown({ actionable: 0, draft: 1 }, 1, 'prs');
assert.match(html, /0 actionable<\/span> · <span[^>]*>1 draft/);
html = formatRepoWorkBreakdown({ actionable: 0, filtered: 1, other: 1 }, 2, 'issues');
assert.match(html, /1 filtered<\/span> · <span[^>]*>1 other/);
assert.equal(formatRepoWorkBreakdown({ actionable: 0 }, 0, 'issues'), '');
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("repository breakdown renderer failed: %v\n%s\n%s", err, out, strings.TrimSpace(script))
	}
}

func TestRepoCardsUseStructuredWorkBreakdown(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"formatRepoWorkBreakdown(r.workBreakdown?.issues, r.issues, 'issues', repoStaleIssueCount(r.actionableIssues || []))",
		"formatRepoWorkBreakdown(r.workBreakdown?.prs, r.prs, 'prs')",
		"${issueBreakdown}",
		"${prBreakdown}",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("repository cards do not consume server breakdown: missing %q", snippet)
		}
	}
}
