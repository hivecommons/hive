package dashboard

import (
	"strings"
	"testing"
)

func TestOverviewBandChartsStaticWiring9003(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`id="overview-section"`,
		`id="overview-card"`,
		`data-action="toggleSection" data-arg0="overview-section"`,
		"renderOverviewCharts(repos)",
		"applySectionCollapse('overview-section')",
		"function overviewIssueBandSlices(repos)",
		"function overviewPRBandSlices(repos)",
		"function renderOverviewDonut(title, subtitle, slices)",
		"groupedRepoIssues(issues)",
		"groupedRepoPRs(r.openPrs || [], r.heldPrs || [])",
		"(r.actionableIssues || []).concat(r.heldIssues || [])",
		"OVERVIEW_ISSUE_BAND_ORDER = ['ready', 'in-progress', 'agent-filed', 'waiting', 'done']",
		"PR_BAND_ORDER.map(band => ({",
		"Issues by band",
		"PRs by band",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"fetch('/api/overview",
		"setTimeout(renderOverview",
	} {
		if strings.Contains(html, forbidden) {
			t.Errorf("overview charts should render from repository-card data, found %q", forbidden)
		}
	}
}

func TestOverviewBandChartClassMappings9003(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"overview-issue-ready { --slice-c: var(--status-info); }",
		"overview-issue-in-progress { --slice-c: var(--status-warn); }",
		"overview-issue-agent-filed { --slice-c: var(--acmm-level-5); }",
		"overview-issue-waiting { --slice-c: var(--status-attention); }",
		"overview-issue-done { --slice-c: var(--status-ok); }",
		"overview-pr-waiting { --slice-c: var(--status-attention); }",
		"overview-pr-eligible { --slice-c: var(--status-ok); }",
		"overview-pr-blocked { --slice-c: var(--status-error); }",
		"overview-pr-in-review { --slice-c: var(--status-warn); }",
		"overview-pr-open { --slice-c: var(--acmm-level-5); }",
		"overview-pr-draft { --slice-c: var(--status-neutral); }",
		"className: 'overview-issue-' + band",
		"className: 'overview-pr-' + band",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("overview band chart mapping missing %q", want)
		}
	}
}
