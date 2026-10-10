package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestDiagnosticsCollapsedHeadlineRendersQuotaAndTokens(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: diagnostics headline render rule was not executed")
	}
	html := indexHTML(t)
	script := `const assert = require('node:assert/strict');
process.env.TZ = 'America/New_York';
let _navbarClockTimeZone = 'America/New_York';
// Pin "now" before the fixture reset so the reset-passed branch does not fire.
Date.now = () => 1791000000000;
const GH_RATE_CRITICAL_REMAINING_PCT = 10;
const GH_RATE_WARN_REMAINING_PCT = 25;
` + jsFunc(t, html, "escapeHtml") + `
` + jsFunc(t, html, "textOrDash") + `
` + jsFunc(t, html, "debugQuotaClass") + `
` + jsFunc(t, html, "debugQuotaChipClass") + `
` + jsFunc(t, html, "debugCompactNum") + `
` + jsFunc(t, html, "debugQuotaResetLabel") + `
` + jsFunc(t, html, "debugGhRateClassFromRemainingPct") + `
` + jsFunc(t, html, "debugGhRateChipClassFromRemainingPct") + `
` + jsFunc(t, html, "debugGhRateLimitStatus") + `
` + jsFunc(t, html, "debugQuotaNormalize") + `
` + jsFunc(t, html, "debugQuotaFromWorkSources") + `
` + jsFunc(t, html, "debugGitHubQuotaSources") + `
` + jsFunc(t, html, "debugQuotaWidgetHtml") + `
` + jsFunc(t, html, "debugDiagnosticsHeadlineHtml") + `
const fixture = {
  ghRateLimits: { core: { limit: 5000, remaining: 4929, reset: 1791027360 } },
  tokens: { totals: { input: 371933665, output: 2949302, cacheRead: 339598125 } },
};
const out = debugDiagnosticsHeadlineHtml(fixture, 'healthy');
assert.match(out, /class="debug-headline sec-headline" data-collapsed-keep/);
assert.match(out, /GitHub API 4929\/5000 remaining \(99% free\) · resets 07:36 AM EDT/);
assert.match(out, /372M in · 2\.9M out · 340M cache/);
assert.match(out, /debug-quota-ok/);
assert.match(out, /debug-quota-strip/);
assert.match(out, /debug-quota-chip ok/);
assert.match(out, /GitHub API: 4929\/5000 remaining \(99% free\)\. resets 07:36 AM EDT/);
assert.match(out, /<span class="debug-quota-label">GitHub API<\/span>/);
assert.match(out, /<span class="debug-quota-frac">4929\/5000<\/span>/);
assert.doesNotMatch(out, /mini-health-dot/);
assert.doesNotMatch(out, /mini-spark/);
assert.equal(debugQuotaClass(51), 'debug-quota-ok');
assert.equal(debugQuotaClass(61), 'debug-quota-warn');
assert.equal(debugQuotaClass(86), 'debug-quota-bad');
assert.equal(debugGhRateClassFromRemainingPct(10), 'debug-quota-bad');
assert.equal(debugGhRateClassFromRemainingPct(25), 'debug-quota-warn');
assert.equal(debugGhRateClassFromRemainingPct(26), 'debug-quota-ok');
assert.equal(debugGhRateChipClassFromRemainingPct(5), 'bad');
const passed = debugGhRateLimitStatus({ limit: 5000, remaining: 12, reset: 1791027360 }, 1791027360 * 1000);
assert.equal(passed.remaining, 5000);
assert.equal(passed.resetText, 'reset passed; refreshing');
assert.equal(debugCompactNum(371933665), '372M');
assert.equal(debugCompactNum(2949302), '2.9M');
const future = debugQuotaWidgetHtml({ quotaSources: [{ name: 'GitLab', quotas: [{ limit: 100, remaining: 10, reset: 1791027360 }] }] });
assert.match(future, /GitLab/);
assert.match(future, /debug-quota-chip bad/);
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node diagnostics collapsed headline failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	render := jsFunc(t, html, "renderDebugSection")
	for _, snippet := range []string{
		"${debugDiagnosticsHeadlineHtml(data, debugSummary)}",
		"applySectionCollapse('debug-section');",
		".debug-quota-strip",
		".debug-quota-chip",
	} {
		if !strings.Contains(html, snippet) && !strings.Contains(render, snippet) {
			t.Fatalf("System Diagnostics collapsed quota contract missing %q", snippet)
		}
	}
}
