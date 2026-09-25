package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestRepoCardPillRowsUseSharedGrid(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		".repo-issue-pill-wrap, .repo-pr-pill-wrap { display: grid;",
		"grid-template-columns: minmax(3.65rem, max-content) minmax(10.5rem, 1fr) max-content;",
		".repo-pill-actions { display: grid; grid-template-columns: 1.65rem 1.65rem 1.65rem minmax(4.8rem, max-content);",
		"function repoPillActionCluster(slots)",
		`<span class="repo-issue-pill-wrap repo-pill-row">`,
		`<span class="repo-pr-pill-wrap repo-pill-row">`,
		"repoPillActionCluster([prBadge, planBtn, designBtn, holdBtn])",
		"repoPillActionCluster([reviewPill, queueBtn + stateBadge, '', holdBtn])",
		"repoPillActionCluster(['', '', '', holdBtn])",
		"repo-pill-action-slot empty",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing shared repo pill layout snippet %q", want)
		}
	}
}

func TestRepoCardPillTitleBudgetKeepsReadableText(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: repo pill title budget was not executed")
	}
	html := indexHTML(t)
	script := `const assert = require('node:assert/strict');
const REPO_CARD_DEFAULT_W = 240;
const REPO_CARD_MIN_W = 180;
const REPO_CARD_MAX_W = 1400;
const PILL_TITLE_BASE_LEN = 50;
const PILL_TITLE_MAX_LEN = 240;
const PILL_TITLE_MIN_VISIBLE = 18;
` + jsFunc(t, html, "clampRepoCardWidth") + "\n" + jsFunc(t, html, "pillTitleLimit") + "\n" + jsFunc(t, html, "pillDisplayTitle") + `
const longTitle = '🐝 governor fixes repo card layout so operators can read the actual work item';
const rendered = pillDisplayTitle(longTitle, pillTitleLimit(REPO_CARD_DEFAULT_W));
assert.ok(rendered.length >= 19, rendered);
assert.ok(rendered.includes('governor fixes'), rendered);
assert.ok(rendered.endsWith('…'), rendered);
assert.equal(pillDisplayTitle('short title', 3), 'short title');
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("repo pill title budget check failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
