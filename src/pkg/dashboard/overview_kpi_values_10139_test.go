package dashboard

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func TestOverviewKPIValuesRenderFromChartSlices10139(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: Overview KPI render rule was not executed")
	}
	html := indexHTML(t)
	script := `const assert = require('node:assert/strict');
` + jsFunc(t, html, "esc") + `
` + jsFunc(t, html, "overviewItemAgeMinutes") + `
` + jsFunc(t, html, "overviewMedianAgeLabel") + `
` + jsFunc(t, html, "renderOverviewKPIs") + `
const state = { showKPIs: true, timeBasis: 'updated' };
const repos = [{ heldIssues: [{ number: 501 }], heldPrs: [] }];
const issueSlices = [
  { key: 'ready', count: 93, items: [{ updated_at: new Date(Date.now() - 7 * 60000).toISOString() }] },
  { key: 'in-progress', count: 4, items: [] },
  { key: 'agent-filed', count: 0, items: [] },
  { key: 'waiting', count: 0, items: [] },
  { key: 'done', count: 2, items: [] },
];
const prSlices = [
  { key: 'waiting', count: 0, items: [] },
  { key: 'eligible', count: 4, items: [] },
  { key: 'blocked', count: 1, items: [] },
  { key: 'in-review', count: 2, items: [] },
  { key: 'open', count: 0, items: [] },
  { key: 'draft', count: 0, items: [] },
];
const out = renderOverviewKPIs(repos, issueSlices, prSlices, state);
const values = [...out.matchAll(/<span class="overview-kpi-value"[^>]*>([^<]*)<\/span><span class="overview-kpi-label">([^<]*)<\/span>/g)].map(m => [m[2], m[1]]);
assert.equal(values.length, 6);
for (const [label, value] of values) assert.notEqual(value, '', label + ' rendered an empty KPI value');
assert.deepEqual(Object.fromEntries(values), {
  'Open issues': '99',
  'Open PRs': '7',
  'Actionable now': '103',
  'Held': '1',
  'Blocked / needs-human': '1',
  'Median age': '7m',
});
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node overview KPI render failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}

func TestOverviewKPIRenderEscapesNumericValues10139(t *testing.T) {
	html := indexHTML(t)
	kpis := jsFunc(t, html, "renderOverviewKPIs")
	for _, want := range []string{
		"const openIssues = (issueSlices || []).reduce((n, s) => n + Number(s.count || 0), 0);",
		"const openPRs = (prSlices || []).reduce((n, s) => n + Number(s.count || 0), 0);",
		"${esc(String(value ?? '—'))}",
	} {
		if !strings.Contains(kpis, want) {
			t.Errorf("renderOverviewKPIs missing %q", want)
		}
	}
	if regexp.MustCompile(`esc\(value\)`).FindString(kpis) != "" {
		t.Errorf("renderOverviewKPIs must not pass numeric KPI values directly to esc")
	}
}
