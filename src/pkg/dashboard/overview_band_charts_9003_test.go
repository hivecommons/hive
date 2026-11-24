package dashboard

import (
	"os/exec"
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
		"function overviewSliceSpecs(kind)",
		"const list = items.get(issue && issue.chart_slice);",
		"const list = items.get(pr && pr.chart_slice);",
		"(r.actionableIssues || []).forEach(issue => place(issue, false));",
		"(r.nonActionableIssues || []).forEach(issue => place(issue, false));",
		"groupedRepoIssues(issues)",
		"groupedRepoPRs(r.openPrs || [], r.heldPrs || [])",
		"repoCardTrackedIssues(repo)",
		"OVERVIEW_ISSUE_BAND_ORDER = ['ready', 'in-progress', 'agent-filed', 'waiting', 'done']",
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
		"overview-issue-your-turn, .overview-pr-your-turn { --slice-c: var(--band-needs-human); }",
		"overview-issue-to-close, .overview-pr-to-close { --slice-c: var(--band-confirm-close); }",
		"overview-issue-waiting-elsewhere, .overview-pr-waiting-elsewhere { --slice-c: var(--muted); }",
		"className: 'overview-issue-' + spec.key",
		"className: 'overview-pr-' + spec.key",
		// The Overview card's bands: one class per donut and band, from the
		// approved mock-up's palette, and one per headed group.
		"className: 'overview-card-' + kind + '-' + spec.key",
		".overview-card-group-your-turn { --slice-c: var(--band-needs-human); }",
		".overview-card-issue-decide, .overview-card-pr-decide { --slice-c: var(--card-band-decide); }",
		".overview-card-issue-review, .overview-card-pr-review { --slice-c: var(--card-band-review); }",
		".overview-card-issue-to-close, .overview-card-pr-to-close { --slice-c: var(--band-confirm-close); }",
		".overview-card-issue-queued, .overview-card-pr-queued { --slice-c: var(--band-unclaimed); }",
		".overview-card-issue-working, .overview-card-pr-working { --slice-c: var(--band-claimed); }",
		".overview-card-issue-author-update, .overview-card-pr-author-update { --slice-c: var(--band-blocked); }",
		"className: 'overview-card-group-' + s.group",
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

// The band donuts draw tracked items only (actionable + held); the "Total
// open" cards count every open item. Without the Outside slice the charts
// showed 100 issues / 93 PRs beside cards saying 134 / 95 (knuckle,
// 2026-11-19). Each chart must add up to its card for all repos and for a
// single selected repo, and the Outside slice must not double count.
func TestOverviewBandChartsAddUpToKPITotals(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: Overview chart totals were not executed")
	}
	html := indexHTML(t)
	script := overviewKPIRenderPrelude(t, html) + `
const OVERVIEW_HISTORY_MAX_SAMPLES = 288;
const OVERVIEW_HISTORY_MIN_SAMPLE_MS = 60000;
` + jsFunc(t, html, "overviewOutsideSlice") + `
` + jsFunc(t, html, "overviewChartSlices") + `
` + jsFunc(t, html, "overviewHistoryCounts") + `
` + jsFunc(t, html, "overviewSampleHistory") + `
` + jsFunc(t, html, "overviewAnimationSignature") + `
const state = { showKPIs: true };
const fill = n => Array.from({ length: n }, (_, i) => ({ number: i + 1 }));
const repos = [
  { full: 'acme/app', actionableIssues: fill(95), heldIssues: fill(5), openPrs: fill(90), heldPrs: fill(3),
    workBreakdown: { issues: { actionable: 95, hold: 5, dependency_dashboard: 22, filtered: 11, hive_advisory: 1 }, prs: { actionable: 90, hold: 3, draft: 2 } } },
  { full: 'acme/lib', actionableIssues: fill(4), heldIssues: [], openPrs: fill(1), heldPrs: [],
    workBreakdown: { issues: { actionable: 4, filtered: 3 }, prs: { actionable: 1 } } },
];
_overviewLastRepos = repos;
const issueSlices = (n, held) => [
  { key: 'ready', count: n - held - 10, items: [] }, { key: 'in-progress', count: held, items: [] },
  { key: 'agent-filed', count: 0, items: [] }, { key: 'waiting', count: 6, items: [] }, { key: 'done', count: 4, items: [] },
];
const prSlices = (n) => [
  { key: 'waiting', count: 0, items: [] }, { key: 'eligible', count: n - 3, items: [] }, { key: 'blocked', count: 1, items: [] },
  { key: 'in-review', count: 2, items: [] }, { key: 'open', count: 0, items: [] }, { key: 'draft', count: 0, items: [] },
];
const cards = out => Object.fromEntries([...out.matchAll(/data-overview-kpi-key="([^"]+)" data-overview-kpi-value="([^"]+)"[^>]*>([^<]*)<\/span><span class="overview-kpi-label">([^<]*)/g)].map(m => [m[4], Number(m[3])]));
const sum = slices => slices.reduce((n, s) => n + s.count, 0);
const check = (selected, iSlices, pSlices) => {
  const issueChart = overviewChartSlices('issue', iSlices, selected);
  const prChart = overviewChartSlices('pr', pSlices, selected);
  const kpi = cards(renderOverviewKPIs(selected, iSlices, pSlices, state));
  assert.equal(sum(issueChart), kpi['Total open issues'], 'issue donut total');
  assert.equal(sum(prChart), kpi['Total open PRs'], 'PR donut total');
  assert.equal(kpi['Total open issues'] + kpi['Total open PRs'],
    kpi['Actionable now'] + kpi.Held + kpi['Blocked / needs-human'] + kpi.Outside, 'card arithmetic');
  // The band slices are untouched: the Outside slice only adds the gap.
  assert.deepEqual(issueChart.slice(0, iSlices.length), iSlices);
  assert.deepEqual(prChart.slice(0, pSlices.length), pSlices);
  return { issueChart, prChart };
};

// All repos: 104 tracked issues of 141, 94 tracked PRs of 96.
const all = check(repos, issueSlices(104, 5), prSlices(94));
const issueOutside = all.issueChart.find(s => s.key === 'outside');
const prOutside = all.prChart.find(s => s.key === 'outside');
assert.equal(issueOutside.count, 37);
assert.equal(prOutside.count, 2);
assert.equal(issueOutside.className, 'overview-issue-outside');
assert.equal(prOutside.className, 'overview-pr-outside');
assert.match(issueOutside.rule, /14 filtered · 1 hive advisory · 22 dependency dashboard/);
assert.match(prOutside.rule, /: 2 draft$/);
assert.doesNotMatch(prOutside.rule, /held/);

// One repo selected (the knuckle shape: 134/95 on the cards, 100/93 in the
// bands): the chart follows that repo's card.
const one = check([repos[0]], issueSlices(100, 5), prSlices(93));
assert.equal(one.issueChart.find(s => s.key === 'outside').count, 34);
assert.match(one.issueChart.find(s => s.key === 'outside').rule, /11 filtered · 1 hive advisory · 22 dependency dashboard/);

// Nothing outside: no Outside slice at all.
const tidy = { full: 'acme/tidy', actionableIssues: fill(2), heldIssues: [], openPrs: fill(1), heldPrs: [],
  workBreakdown: { issues: { actionable: 2 }, prs: { actionable: 1 } } };
const tidyIssues = [{ key: 'ready', count: 2, items: [] }];
const tidyPRs = [{ key: 'eligible', count: 1, items: [] }];
assert.deepEqual(overviewChartSlices('issue', tidyIssues, [tidy]), tidyIssues);
assert.deepEqual(overviewChartSlices('pr', tidyPRs, [tidy]), tidyPRs);

// History and the animation signature carry the Outside count.
const hist = { history: {} };
assert.equal(overviewSampleHistory('issue', all.issueChart, hist), true);
assert.equal(hist.history.issue[0].counts.outside, 37);
assert.match(overviewAnimationSignature(all.issueChart, all.prChart, []), /\["outside",37\]/);
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node overview chart totals failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}

func TestOverviewBandChartsDrawOutsideSliceStatic(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"const issueChartSlices = overviewChartSlices('issue', issueSlices, selectedRepos);",
		"const prChartSlices = overviewChartSlices('pr', prSlices, selectedRepos);",
		"overviewSampleHistory('issue', issueChartSlices, state)",
		"overviewAnimationSignature(issueChartSlices, prChartSlices, selectedNames)",
		// The KPI cards keep the band slices: their arithmetic reads band keys.
		"renderOverviewKPIs(selectedRepos, issueSlices, prSlices, state)",
		".overview-issue-outside, .overview-pr-outside { --slice-c: var(--muted); }",
		"if (band === 'outside') return overviewApplyKPIProjectFilter('outside');",
		"kind && !s.outside ?",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}
