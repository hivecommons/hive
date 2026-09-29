package dashboard

import (
	"strings"
	"testing"
)

// #9019: bands are named after the operator's action, the agent-filed band
// empties once a human acknowledges the proposal, and every place a band is
// named (repo-card header, Overview slice and legend row, pill legend) carries
// the same rule text from one spec table.
func TestDashboardBandActionsStaticWiring9019(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"function issueBandSpec(band)",
		"function prBandSpec(band)",
		// Legend issue-band pills are generated from the spec table, not hand-written.
		"const issueBandLegendEntries = OVERVIEW_ISSUE_BAND_ORDER.map(band => {",
		"tip: issueBandTip(band)",
		// Repo-card band headers carry the rule.
		`<div class="repo-issue-band-title" title="${esc(g.tip)}">`,
		`<div class="repo-pr-band-title" title="${esc(g.tip)}">`,
		// Overview legend rows and slices carry the rule.
		`<div class="overview-chart-legend-row" title="${esc(s.label + (s.rule ? ': ' + s.rule : ''))}">`,
		"<title>${esc(s.label)}: ${s.count}${s.rule ? ' — ' + esc(s.rule) : ''}</title>",
		"hover a band for its rule",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"'Likely done'",
		"'Agent-filed'",
		"'Waiting on human'",
		"'Ready'",
		"'In progress'",
		"likely done</span>",
		"Band: needs-human, held, or configured waiting labels",
	} {
		if strings.Contains(html, forbidden) {
			t.Errorf("index.html still names a classifier band %q", forbidden)
		}
	}
}
