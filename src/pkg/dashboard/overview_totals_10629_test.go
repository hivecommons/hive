package dashboard

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestOverviewTotalsUseForgeCountsAndExplainScannerGap10629(t *testing.T) {
	status := &StatusPayload{Repos: []FrontendRepo{
		{
			Name: "hive", Full: "hivecommons/hive",
			ActionableIssues: []any{github.Issue{Number: 1, Repo: "hivecommons/hive", Title: "human issue", AuthorIsHuman: true, CreatedAt: time.Now()}, github.Issue{Number: 2, Repo: "hivecommons/hive", Title: "bot issue", Author: "hive[bot]", CreatedAt: time.Now()}},
			HeldIssues:       []any{github.HoldItem{Number: 3, Repo: "hivecommons/hive", Type: "issue", Labels: []string{"hold"}}},
			OpenPrs:          []any{github.PullRequest{Number: 4, Repo: "hivecommons/hive", Title: "ready PR"}},
			HeldPrs:          []any{github.PullRequest{Number: 5, Repo: "hivecommons/hive", Title: "held PR", Labels: []string{"hold"}}},
			WorkBreakdown: &github.RepoWorkBreakdown{
				Issues: github.RepoIssueBreakdown{Actionable: 2, Hold: 1, Filtered: 2, NeedsDirection: 1, Exempt: 1},
				PRs:    github.RepoPRBreakdown{Actionable: 1, Hold: 1, Draft: 1},
			},
		},
		{
			Name: "docs", Full: "hivecommons/docs",
			ActionableIssues: []any{github.Issue{Number: 6, Repo: "hivecommons/docs", Title: "docs issue", AuthorIsHuman: true, CreatedAt: time.Now()}},
			OpenPrs:          []any{github.PullRequest{Number: 7, Repo: "hivecommons/docs", Title: "docs PR"}},
			WorkBreakdown: &github.RepoWorkBreakdown{
				Issues: github.RepoIssueBreakdown{Actionable: 1, Filtered: 1, NeedsDecision: 1},
				PRs:    github.RepoPRBreakdown{Actionable: 1},
			},
		},
	}}

	got := (&Server{}).statusWithOverviewBands(status, time.Now())
	if got.OverviewTotals.Issues.Forge != 7 || got.OverviewTotals.Issues.Tracked != 4 || got.OverviewTotals.Issues.Outside != 3 {
		t.Fatalf("issue totals = %+v, want forge=7 tracked=4 outside=3", got.OverviewTotals.Issues)
	}
	if got.OverviewTotals.Issues.Breakdown["needs_direction"] != 1 || got.OverviewTotals.Issues.Breakdown["needs_decision"] != 1 || got.OverviewTotals.Issues.Breakdown["exempt"] != 1 {
		t.Fatalf("issue breakdown = %+v, want needs-direction, needs-decision, and exempt gaps", got.OverviewTotals.Issues.Breakdown)
	}
	if got.OverviewTotals.PRs.Forge != 4 || got.OverviewTotals.PRs.Tracked != 3 || got.OverviewTotals.PRs.Outside != 1 || got.OverviewTotals.PRs.Breakdown["draft"] != 1 {
		t.Fatalf("PR totals = %+v, want multi-repo forge total with draft breakdown", got.OverviewTotals.PRs)
	}
}

func TestOverviewActionableEquationPartitionsOpenWork(t *testing.T) {
	now := time.Date(2026, 10, 5, 15, 0, 0, 0, time.UTC)
	status := &StatusPayload{Repos: []FrontendRepo{{
		Name: "hive", Full: "hivecommons/hive", Issues: 9, PRs: 5,
		ActionableIssues: []any{
			github.Issue{Repo: "hivecommons/hive", Number: 1, Title: "ready"},
			github.Issue{Repo: "hivecommons/hive", Number: 2, Title: "claimed", Assignees: []string{"alice"}},
			github.Issue{Repo: "hivecommons/hive", Number: 3, Title: "needs human", Labels: []string{"needs-human"}},
			github.Issue{Repo: "hivecommons/hive", Number: 4, Title: "covered by PR", Labels: []string{"hive/covered-by-pr"}},
		},
		HeldIssues: []any{github.HoldItem{Repo: "hivecommons/hive", Number: 5, Type: "issue", Labels: []string{"hold"}}},
		OpenPrs: []any{
			FrontendPR{PullRequest: github.PullRequest{Repo: "hivecommons/hive", Number: 10, Title: "open"}},
			FrontendPR{PullRequest: github.PullRequest{Repo: "hivecommons/hive", Number: 11, Title: "dependency blocked", Mergeable: github.MergeableNo}},
			FrontendPR{PullRequest: github.PullRequest{Repo: "hivecommons/hive", Number: 12, Title: "draft", Draft: true}},
		},
		HeldPrs: []any{FrontendPR{PullRequest: github.PullRequest{Repo: "hivecommons/hive", Number: 13, Title: "held", Labels: []string{"hold"}}}},
		WorkBreakdown: &github.RepoWorkBreakdown{
			Issues: github.RepoIssueBreakdown{Actionable: 4, Hold: 1, Filtered: 2, NeedsHuman: 1, Exempt: 1, HiveAdvisory: 1, DependencyDashboard: 1},
			PRs:    github.RepoPRBreakdown{Actionable: 2, Hold: 1, Draft: 1, Filtered: 1},
		},
	}}}

	got := (&Server{}).statusWithOverviewBands(status, now).ActionableNow
	if got.Equation == nil {
		t.Fatal("missing actionable equation")
	}

	terms := map[string]int{}
	sum := 0
	for _, term := range got.Equation.Terms {
		terms[term.Key] = term.Count
		sum += term.Count
	}
	if got.Equation.OpenIssues != 9 || got.Equation.OpenPRs != 5 || got.Equation.TotalOpen != 14 {
		t.Fatalf("open side = %+v, want 9 issues + 5 PRs", got.Equation)
	}
	if got.Total != 3 || terms["actionable"] != 3 || terms["held"] != 2 || terms["blocked_needs_human"] != 4 || terms["outside"] != 5 {
		t.Fatalf("partition terms = %+v, actionableNow=%+v", terms, got)
	}
	if sum != got.Equation.TotalOpen {
		t.Fatalf("partition sum = %d, want total open %d (%s)", sum, got.Equation.TotalOpen, got.Equation.Text)
	}
	if want := "9 issues + 5 PRs = 3 actionable + 2 held + 4 blocked/needs-human + 5 outside"; got.Equation.Text != want {
		t.Fatalf("equation text = %q, want %q", got.Equation.Text, want)
	}
	for _, want := range []string{"1 exempt", "1 hive advisory", "1 dependency dashboard", "outside PRs: 1 filtered"} {
		if !strings.Contains(got.Equation.Title, want) {
			t.Fatalf("equation title missing %q: %q", want, got.Equation.Title)
		}
	}
}

func TestOverviewOutsideBreakdownExplainsEveryScannerFilter(t *testing.T) {
	reporterTrustEnabled := true
	status := &StatusPayload{HiveID: "hive-test", Repos: []FrontendRepo{{
		Name: "hive", Full: "hivecommons/hive",
		ActionableIssues: []any{
			github.Issue{Repo: "hivecommons/hive", Number: 1, Title: "ready"},
			github.Issue{Repo: "hivecommons/hive", Number: 2, Title: "also ready"},
		},
		HeldIssues: []any{github.HoldItem{Repo: "hivecommons/hive", Number: 3, Type: "issue", Labels: []string{"hold"}}},
		OpenPrs:    []any{github.PullRequest{Repo: "hivecommons/hive", Number: 10, Title: "ready PR"}},
		HeldPrs:    []any{github.PullRequest{Repo: "hivecommons/hive", Number: 11, Title: "held PR", Labels: []string{"hold"}}},
		WorkBreakdown: &github.RepoWorkBreakdown{
			Issues: github.RepoIssueBreakdown{
				Actionable: 2, Hold: 1, Filtered: 21,
				NeedsHuman: 1, NeedsDirection: 2, NeedsDecision: 3, NeedsSpec: 4, Exempt: 5,
				ReporterTriage: 7, HiveAdvisory: 8, DependencyDashboard: 9, Other: 10,
			},
			PRs: github.RepoPRBreakdown{Actionable: 1, Hold: 1, Draft: 11, Filtered: 12, Other: 13},
		},
	}}}
	srv := &Server{deps: &Dependencies{Config: &config.Config{
		HiveID:   "hive-test",
		Governor: config.GovernorConfig{Labels: config.LabelsConfig{Exempt: []string{"no-ai", "waiting-on-author"}}},
		Project: config.ProjectConfig{IssueFilter: config.IssueFilterConfig{
			RequireLabels: []string{"approved"},
			ReporterTrust: config.ReporterTrustConfig{
				Enabled:                &reporterTrustEnabled,
				UntrustedRequireLabels: []string{"triage/accepted"},
			},
		}},
	}}}

	got := srv.statusWithOverviewBands(status, time.Now()).ActionableNow
	if got.Outside == nil {
		t.Fatal("missing actionableNow.outside")
	}
	sum := 0
	byKey := map[string]FrontendActionableBreakdownTerm{}
	for _, row := range got.Outside.Breakdown {
		sum += row.Count
		byKey[row.Key] = row
		if row.Rule == "" || row.SettingPath == "" || row.SettingValue == "" || row.HowToChange == "" {
			t.Fatalf("outside row missing explanation fields: %+v", row)
		}
	}
	if sum != got.Outside.Count {
		t.Fatalf("outside breakdown sum = %d, outside count = %d (%+v)", sum, got.Outside.Count, got.Outside.Breakdown)
	}
	for key, want := range map[string]int{
		"needs-direction":            2,
		"needs-decision":             3,
		"needs-spec":                 4,
		"exempt-labels":              5,
		"project-issue-filter":       6,
		"reporter-triage":            7,
		"standing-meta-advisory":     8,
		"dependency-dashboard":       9,
		"hold-adjacent-other-issues": 10,
		"draft-prs":                  11,
		"exempt-pr-labels":           12,
		"hold-adjacent-other-prs":    13,
	} {
		if byKey[key].Count != want {
			t.Fatalf("%s count = %d, want %d; rows=%+v", key, byKey[key].Count, want, got.Outside.Breakdown)
		}
	}
	if !strings.Contains(byKey["exempt-labels"].SettingValue, "no-ai") || !strings.Contains(byKey["project-issue-filter"].SettingValue, "approved") || !strings.Contains(byKey["reporter-triage"].SettingValue, "enabled=true") {
		t.Fatalf("breakdown did not resolve live config values: %+v", got.Outside.Breakdown)
	}
}

func TestOverviewKPIRenderedMathSublineSumsToHeadline(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: Overview KPI render math was not executed")
	}
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(raw)
	start := strings.Index(html, "const OVERVIEW_ISSUE_BREAKDOWN_LABELS =")
	if start < 0 {
		t.Fatal("Overview KPI helper block not found")
	}
	end := strings.Index(html[start:], "    function overviewKPIEquation")
	if end < 0 {
		t.Fatal("Overview KPI helper block end not found")
	}
	block := html[start : start+end]
	script := `
function esc(v) { return String(v == null ? '' : v).replace(/[&<>"]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c])); }
` + block + `
const issueEq = { kind: 'issues', open: 42, result: 4, terms: [
  { key: 'held', label: 'held', count: 10 },
  { key: 'blocked_needs_human', label: 'blocked/needs-human', count: 3 },
  { key: 'outside', label: 'outside', count: 25, breakdown: [
    { key: 'needs-direction', label: 'needs-direction', count: 6 },
    { key: 'needs-decision', label: 'needs-decision', count: 2 },
    { key: 'exempt-labels', label: 'exempt', count: 1 },
    { key: 'reporter-triage', label: 'reporter triage', count: 7 },
    { key: 'standing-meta-advisory', label: 'hive advisory', count: 1 },
    { key: 'hold-adjacent-other-issues', label: 'other/unclassified', count: 8 }
  ] }
] };
const subline = renderOverviewTotalPartitionSubline(issueEq);
const m = subline.match(/(\d+) = (\d+) actionable \+ (\d+) held \+ (\d+) blocked \+ (\d+) outside/);
if (!m) throw new Error('partition subline did not render expected equation: ' + subline);
const nums = m.slice(1).map(Number);
if (nums[0] !== nums[1] + nums[2] + nums[3] + nums[4]) throw new Error('partition equation does not sum: ' + subline);
const outside = Array.from(subline.matchAll(/(\d+) (needs-direction|needs-decision|exempt|reporter triage|hive advisory|other\/unclassified)/g)).reduce((n, row) => n + Number(row[1]), 0);
if (outside !== nums[4]) throw new Error('outside breakdown does not sum: ' + subline);
const held = renderOverviewSplitSubline(12, 10, 2, 'held');
if (held !== '12 held = 10 issues + 2 PRs') throw new Error('held split did not render: ' + held);
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Overview KPI render math JS failed: %v\n%s", err, out)
	}
}
func TestGovernorActionableSplitsShareOverviewPartition(t *testing.T) {
	now := time.Date(2026, 10, 5, 16, 0, 0, 0, time.UTC)
	status := &StatusPayload{Repos: []FrontendRepo{{
		Name: "hive", Full: "hivecommons/hive", Issues: 6, PRs: 4,
		ActionableIssues: []any{
			github.Issue{Repo: "hivecommons/hive", Number: 1, Title: "ready"},
			github.Issue{Repo: "hivecommons/hive", Number: 2, Title: "claimed", Assignees: []string{"alice"}},
			github.Issue{Repo: "hivecommons/hive", Number: 3, Title: "needs human", Labels: []string{"needs-human"}},
			github.Issue{Repo: "hivecommons/hive", Number: 4, Title: "confirm close", Labels: []string{"hive/likely-done"}},
		},
		HeldIssues: []any{github.HoldItem{Repo: "hivecommons/hive", Number: 5, Type: "issue", Labels: []string{"hold"}}},
		OpenPrs: []any{
			FrontendPR{PullRequest: github.PullRequest{Repo: "hivecommons/hive", Number: 10, Title: "open"}},
			FrontendPR{PullRequest: github.PullRequest{Repo: "hivecommons/hive", Number: 11, Title: "blocked", Mergeable: github.MergeableNo}},
			FrontendPR{PullRequest: github.PullRequest{Repo: "hivecommons/hive", Number: 12, Title: "draft", Draft: true}},
		},
		HeldPrs: []any{FrontendPR{PullRequest: github.PullRequest{Repo: "hivecommons/hive", Number: 13, Title: "held", Labels: []string{"hold"}}}},
		WorkBreakdown: &github.RepoWorkBreakdown{
			Issues: github.RepoIssueBreakdown{Actionable: 4, Hold: 1, Filtered: 1, Exempt: 1},
			PRs:    github.RepoPRBreakdown{Actionable: 2, Hold: 1, Draft: 1},
		},
	}}}

	got := (&Server{}).statusWithOverviewBands(status, now).ActionableNow
	if got.IssueEquation == nil || got.PREquation == nil || got.Equation == nil {
		t.Fatalf("missing equations: %+v", got)
	}
	if got.Total != got.Issues+got.PRs {
		t.Fatalf("overview total = %d, want issue+pr split %d", got.Total, got.Issues+got.PRs)
	}
	if got.Total != got.IssueEquation.Result+got.PREquation.Result {
		t.Fatalf("overview actionable %d != governor split %d + %d", got.Total, got.IssueEquation.Result, got.PREquation.Result)
	}
	if got.Issues != got.IssueEquation.Result || got.PRs != got.PREquation.Result {
		t.Fatalf("governor equations do not match splits: actionable=%+v issueEq=%+v prEq=%+v", got, got.IssueEquation, got.PREquation)
	}
	if got.IssueEquation.Text != "6 open − 1 held − 2 blocked/needs-human − 1 outside = 2" {
		t.Fatalf("issue equation = %q", got.IssueEquation.Text)
	}
	if got.PREquation.Text != "4 open − 1 held − 1 blocked/needs-human − 1 outside = 1" {
		t.Fatalf("PR equation = %q", got.PREquation.Text)
	}
}

func TestOverviewKPIBindsForgeTotalsAndTooltips10629(t *testing.T) {
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(raw)
	for _, want := range []string{
		"window._lastStatus?.overviewTotals?.issues",
		"window._lastStatus.overviewTotals",
		"const openIssues = Number(forgeTotals?.issues?.forge ?? trackedIssues);",
		"const openPRs = Number(forgeTotals?.prs?.forge ?? trackedPRs);",
		"All open GitHub issues (pull requests excluded) across the selected configured repos",
		"All open GitHub pull requests across the selected configured repos, including drafts",
		"overview-kpi-subline",
		"overviewPartitionTooltip(term, context)",
		"aria-describedby",
		"data-action=\"openConfigDialog\" data-keydown-action=\"openConfigDialog\" data-keys=\"Enter, \" data-prevent=\"1\" data-arg0=\"governor\" data-arg2",
		"These items are not actionable because this hive&apos;s filters exclude them",
		"Triage buckets: what counts as outside",
		"project.issue_filter.hard_suppress_labels.needs_direction",
		"Fixed scanner rule",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static/index.html missing %q", want)
		}
	}
	for _, forbidden := range []string{
		`title="${esc(card.tip)}"`,
		"window.alert(",
		"window.prompt(",
		"window.confirm(",
		// The tooltip renders inside <button class="overview-kpi">; a nested <button> makes the HTML
		// parser close the card early and the tooltip spills inline (oke-11 screenshot, 2026-10-05).
		`<button type="button" class="hv-btn btn-secondary btn-sm" data-action="openConfigDialog" data-arg0="governor" data-arg2`,
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("static/index.html uses forbidden native/dialog tooltip pattern %q", forbidden)
		}
	}
}
