package dashboard

import (
	"strings"
	"testing"
)

// The "Core remaining" and "ETag hit rate" rows carry a hover help mark that
// explains what each figure measures and how the hive reacts to it.
func TestDashboardGitHubAPIBudgetRowsHaveHelpMarks(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)
	for _, want := range []string{
		"function ghApiBudgetHelpMark(key, label)",
		"const GH_API_BUDGET_HELP = {",
		"<span>Core remaining${ghApiBudgetHelpMark('core', 'Core remaining')}</span>",
		"<span>ETag hit rate${ghApiBudgetHelpMark('etag', 'ETag hit rate')}</span>",
		`id="${id}" class="config-tooltip" role="tooltip">${esc(GH_API_BUDGET_HELP[key] || '')}</span>`,
		"answered 304 Not Modified because the hive re-sent a remembered ETag (If-None-Match)",
		"GitHub does not charge a 304 against the core rate limit",
		"Counted since the process started, not over the 1h window",
		"Below the configured API reserve (default 800 requests) the governor enters conserve mode",
		"below the API critical floor (default 250)",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing GitHub API budget help-mark snippet %q", want)
		}
	}
}
