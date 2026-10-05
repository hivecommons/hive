package dashboard

import (
	"strings"
	"testing"
)

// The Overview "Actionable now" KPI is the dashboard's only actionable count.
// The Governor strip shows mode-pressure inputs (all non-held issues + all
// open PRs) and must not reuse the "actionable" label (#10561).
func TestGovernorStripDoesNotClaimActionableCount10561(t *testing.T) {
	html := indexHTML(t)

	kpis := jsFunctionBody(t, html, "function renderOverviewKPIs(repos, issueSlices, prSlices, state)")
	for _, want := range []string{
		"const actionableNow = (issueSlices || []).concat(prSlices || []).filter(s => !['waiting', 'done', 'draft', 'blocked'].includes(s.key))",
		"label: 'Actionable now (issues and PRs)', value: actionableNow",
	} {
		if !strings.Contains(kpis, want) {
			t.Fatalf("Overview actionable KPI contract missing %q", want)
		}
	}

	for _, banned := range []string{
		`<span class="gm-label">actionable</span>`,
		`<span class="gm-label">PRs</span>`,
	} {
		if strings.Contains(html, banned) {
			t.Fatalf("Governor strip still labels a non-Overview metric as actionable: %q", banned)
		}
	}
	for _, want := range []string{
		`<span class="gm-label">issue queue</span><span class="gm-val">${govActionable}</span>`,
		`<span class="gm-label">open PRs</span><span class="gm-val">${govPrs}</span>`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("Governor strip missing relabelled pressure tile %q", want)
		}
	}
}
