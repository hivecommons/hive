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
		"fetch('/api/gh-rate-limits', { cache: 'no-store' })",
		"!status.resetPassed && !dueForPeriodicRefresh",
		"function renderGitHubAPIBudgetPanel(ghRate)",
		"id=\"gh-api-budget-panel\"",
		"GitHub API budget (1h)",
		"id=\"gh-api-budget-core-bar\"",
		"id=\"gh-api-budget-consumers\"",
		"ghRate.top_consumers || ghRate.topConsumers",
		"top.map(c =>",
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
