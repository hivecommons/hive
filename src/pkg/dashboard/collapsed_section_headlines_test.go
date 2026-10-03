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
function visualSectionSummary(){ return '<span class="mini-health-dot ok"></span><svg class="mini-spark"></svg>'; }
` + jsFunc(t, html, "escapeHtml") + `
` + jsFunc(t, html, "textOrDash") + `
` + jsFunc(t, html, "debugQuotaClass") + `
` + jsFunc(t, html, "debugCompactNum") + `
` + jsFunc(t, html, "debugDiagnosticsHeadlineHtml") + `
const fixture = {
  ghRateLimits: { core: { limit: 5000, remaining: 4929, reset: 1791027360 } },
  tokens: { totals: { input: 371933665, output: 2949302, cacheRead: 339598125 } },
};
const out = debugDiagnosticsHeadlineHtml(fixture, 'healthy');
assert.match(out, /class="debug-headline sec-headline" data-collapsed-keep/);
assert.match(out, /GitHub API 71\/5000 \(1%\) · resets 07:36 AM/);
assert.match(out, /372M in · 2\.9M out · 340M cache/);
assert.match(out, /debug-quota-ok/);
assert.match(out, /mini-health-dot ok/);
assert.match(out, /mini-spark/);
assert.equal(debugQuotaClass(51), 'debug-quota-warn');
assert.equal(debugQuotaClass(81), 'debug-quota-bad');
assert.equal(debugCompactNum(371933665), '372M');
assert.equal(debugCompactNum(2949302), '2.9M');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node diagnostics collapsed headline failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	render := jsFunc(t, html, "renderDebugSection")
	for _, snippet := range []string{
		"${debugDiagnosticsHeadlineHtml(data, debugSummary)}",
		"applySectionCollapse('debug-section');",
	} {
		if !strings.Contains(render, snippet) {
			t.Fatalf("System Diagnostics render missing collapsed-headline refresh snippet %q", snippet)
		}
	}
}
