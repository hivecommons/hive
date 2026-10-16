package dashboard

import (
	"strings"
	"testing"
)

func TestDashboardRateLimitUIUsesRemainingSemantics(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)
	for _, want := range []string{
		"function debugGhRateLimitStatus(core, nowMs)",
		"GH_RATE_CRITICAL_REMAINING_PCT = 10",
		"GH_RATE_WARN_REMAINING_PCT = 25",
		"reset passed; refreshing",
		"remaining (${remainingPct}% free)",
		"semantics: 'remaining'",
		"q.semantics === 'remaining'",
		"debugGhRateClassFromRemainingPct(remainingPct)",
		"async function maybeRefreshGhRateLimits(ghRateLimits)",
		"id=\"api-budget-pill\"",
		"fetch('/api/gh-rate-limits', { cache: 'no-store' })",
		"!status.resetPassed && !dueForPeriodicRefresh",
		"function renderGitHubAPIBudgetPanel(ghRate)",
		"id=\"gh-api-budget-panel\"",
		"GitHub API budget (1h)",
		"id=\"gh-api-budget-core-bar\"",
		"id=\"gh-api-budget-consumers\"",
		"ghRate.top_consumers || ghRate.topConsumers",
		"top.map(c =>",
		// The caller table has six columns; the tile must span two grid cells and
		// never let the grid item grow to the table's min-content width (#12203).
		".gh-api-budget-card { background: var(--surface); border: 1px solid var(--border); border-radius: var(--r); padding: var(--sp-6); grid-column: span 2; min-width: 0; }",
		"@media (max-width: 640px) { .gh-api-budget-card { grid-column: span 1; } }",
		".gh-api-budget-table-wrap { overflow-x: auto;",
		".gh-api-budget-table { width: 100%; table-layout: fixed;",
		"text-overflow: ellipsis; }",
		"<td class=\"gh-api-budget-col-endpoint\" title=\"${endpoint}\">${endpoint}</td>",
		"<td class=\"gh-api-budget-col-caller\" title=\"${caller}\">${caller}</td>",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing rate-limit remaining-semantics snippet %q", want)
		}
	}
	for _, gone := range []string{
		"GitHub API ${used}/${limit || '—'} (${pct}%) · resets ${resetAt}",
		"RATE_LIMIT_WARN_THRESHOLD = 100",
		"${q.used}/${q.limit}</span></span>`;",
	} {
		if strings.Contains(html, gone) {
			t.Errorf("index.html still contains ambiguous used-quota snippet %q", gone)
		}
	}
}

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
