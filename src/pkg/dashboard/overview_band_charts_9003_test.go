package dashboard

import (
	"regexp"
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
		"function renderOverviewDonut(title, subtitle, slices, history, state)",
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
		"overview-issue-ready { --slice-c: var(--band-unclaimed); }",
		"overview-issue-in-progress { --slice-c: var(--band-claimed); }",
		"overview-issue-agent-filed { --slice-c: var(--band-agent-filed); }",
		"overview-issue-waiting { --slice-c: var(--band-needs-human); }",
		"overview-issue-done { --slice-c: var(--band-confirm-close); }",
		"overview-pr-waiting { --slice-c: var(--band-needs-human); }",
		"overview-pr-eligible { --slice-c: var(--band-merge-eligible); }",
		"overview-pr-blocked { --slice-c: var(--band-blocked); }",
		"overview-pr-in-review { --slice-c: var(--band-in-review); }",
		"overview-pr-open { --slice-c: var(--band-open); }",
		"overview-pr-draft { --slice-c: var(--band-draft); }",
		"className: 'overview-issue-' + band",
		"className: 'overview-pr-' + band",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("overview band chart mapping missing %q", want)
		}
	}
}

func TestOverviewBandChartPaletteIsNamedDistinctAndShared(t *testing.T) {
	html := indexHTML(t)
	palette := []string{
		"--band-unclaimed",
		"--band-claimed",
		"--band-agent-filed",
		"--band-needs-human",
		"--band-confirm-close",
		"--band-merge-eligible",
		"--band-blocked",
		"--band-in-review",
		"--band-open",
		"--band-draft",
	}
	seenValues := map[string]string{}
	for _, name := range palette {
		value := cssCustomPropertyValue(t, html, name)
		if prior := seenValues[value]; prior != "" {
			t.Fatalf("%s and %s share palette value %s; overview bands must stay visually distinct", prior, name, value)
		}
		seenValues[value] = name
	}
	classToken := func(className string) string {
		t.Helper()
		re := regexp.MustCompile(`\.` + regexp.QuoteMeta(className) + `\s*\{\s*--slice-c:\s*var\((--band-[^)]+)\);\s*\}`)
		match := re.FindStringSubmatch(html)
		if match == nil {
			t.Fatalf("missing named band palette mapping for %s", className)
		}
		return match[1]
	}
	if issue, pr := classToken("overview-issue-waiting"), classToken("overview-pr-waiting"); issue != "--band-needs-human" || pr != issue {
		t.Fatalf("Needs human should share one palette token across Issues and PRs, got issue=%s pr=%s", issue, pr)
	}
	for className, token := range map[string]string{
		"overview-issue-ready":       "--band-unclaimed",
		"overview-issue-in-progress": "--band-claimed",
		"overview-issue-agent-filed": "--band-agent-filed",
		"overview-issue-done":        "--band-confirm-close",
		"overview-pr-eligible":       "--band-merge-eligible",
		"overview-pr-blocked":        "--band-blocked",
		"overview-pr-in-review":      "--band-in-review",
		"overview-pr-open":           "--band-open",
		"overview-pr-draft":          "--band-draft",
	} {
		if got := classToken(className); got != token {
			t.Fatalf("%s maps to %s, want %s", className, got, token)
		}
	}
}

func cssCustomPropertyValue(t *testing.T, css, name string) string {
	t.Helper()
	re := regexp.MustCompile(regexp.QuoteMeta(name) + `\s*:\s*([^;]+);`)
	match := re.FindStringSubmatch(css)
	if match == nil {
		t.Fatalf("missing CSS custom property %s", name)
	}
	return strings.TrimSpace(match[1])
}
