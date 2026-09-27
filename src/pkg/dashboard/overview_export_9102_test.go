package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestOverviewServerClassifierAndCSVParity9102(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: Overview classifier parity was not executed")
	}
	html := indexHTML(t)
	funcs := []string{
		"normalizeIssueBandConfig", "repoIssueBandConfig", "canonicalHiveHoldLabel", "holdLabels", "heldReason",
		"issueLabelSet", "issueHasAnyLabel", "issueAgentRole", "issueClaimed", "issueAcknowledged", "issueLinkedPRState", "issueUpdatedAt", "issueIsStale", "issueBandInfo", "issueBandSpec", "issueBandLabel", "issueBandRule", "issueBandTip", "issueBandRank", "groupedRepoIssues", "overviewRepoName", "overviewIssueBandSlices",
		"prLabelSet", "prHasAnyLabel", "prQueued", "prAgentRole", "prUpdatedAt", "prCreatedAt", "prReviewClassRank", "prIsStale", "prCIFailing", "prGitHubReview", "prRequestedReviews", "prConversation", "prBandInfo", "prBandSpec", "prBandLabel", "prBandRule", "prBandTip", "prBandRank", "groupedRepoPRs", "overviewPRBandSlices",
		"overviewCsvCell", "overviewCsv", "overviewCsvColumns", "overviewItemUrl", "overviewSignalLabels", "overviewLinkedPRs", "overviewMergeVerdictText", "overviewReviewDecision", "overviewIssueCsvRow", "overviewPRCsvRow", "overviewCsvRows",
	}
	var script strings.Builder
	script.WriteString(`
const REPO_ISSUE_BAND_DEFAULTS = {
  waitingLabels: ['blocked', 'needs-decision', '2-discussing', 'Epic', 'needs-human', 'needs-triage'],
  doneLabels: ['hive/already-done', 'hive/covered-by-pr', 'hive/likely-done'],
  staleDays: 14
};
const MS_PER_DAY = 24 * 60 * 60 * 1000;
const PR_HUMAN_GATE_LABELS = ['needs-human', 'needs-decision', '2-discussing'];
const PR_BAND_ORDER = ['waiting', 'eligible', 'blocked', 'in-review', 'open', 'draft'];
const OVERVIEW_ISSUE_BAND_ORDER = ['ready', 'in-progress', 'agent-filed', 'waiting', 'done'];
const HOLD_LABEL_SPELLINGS = ['hold', 'on-hold', 'hold/review'];
const window = { _repoIssueBandConfig: Object.assign({}, REPO_ISSUE_BAND_DEFAULTS), _hiveAutoMergeLabel: 'lgtm', _lastStatus: { hiveId: 'h1' } };
let _overviewLastRepos = [];
Date.now = () => Date.parse('2026-09-27T00:00:00Z');
`)
	for _, name := range funcs {
		script.WriteString(jsFunc(t, html, name))
		script.WriteByte('\n')
	}
	script.WriteString(`
const repos = [{
  full: 'octo/demo',
  actionableIssues: [
    { number: 1, title: '=formula', labels: [], assignees: [], updated_at: '2026-09-27T00:00:00Z', created_at: '2026-09-01T00:00:00Z', url: 'https://github.com/octo/demo/issues/1' },
    { number: 2, title: '+sum', labels: ['hive/claimed-by-bot'], assignees: [], updated_at: '2026-09-12T00:00:00Z', created_at: '2026-09-01T00:00:00Z' },
    { number: 3, title: '-@risky', labels: ['Agent/Strategist'], assignees: [], updated_at: '2026-09-11T23:59:59Z', created_at: '2026-09-01T00:00:00Z' },
    { number: 4, title: '\tTabbed', labels: ['agent/quality', 'approved-direction'], assignees: [], updated_at: '2026-09-13T00:00:00Z', created_at: '2026-09-01T00:00:00Z' },
    { number: 5, title: 'Needs "quotes", comma', labels: ['NEEDS-HUMAN'], assignees: [], updated_at: '2026-09-10T00:00:00Z', created_at: '2026-09-01T00:00:00Z' },
    { number: 6, title: '-5', labels: ['hive/likely-done'], assignees: [], updated_at: '2026-09-09T00:00:00Z', created_at: '2026-09-01T00:00:00Z' },
    { number: 7, title: '@hold', labels: ['hold'], assignees: [], updated_at: '2026-09-08T00:00:00Z', created_at: '2026-09-01T00:00:00Z', human_acknowledged: true }
  ],
  heldIssues: [
    { number: 8, title: 'held via list', labels: ['agent/triage', 'hold'], assignees: [], created_at: '2026-09-07T00:00:00Z', url: 'https://github.com/octo/demo/issues/8' }
  ],
  openPrs: [
    { number: 10, title: 'needs human', labels: ['needs-human'], updated_at: '2026-09-01T00:00:00Z', created_at: '2026-08-31T00:00:00Z', author: 'alice' },
    { number: 11, title: 'eligible but no', labels: [], merge_verdict: { state: 'eligible' }, mergeable: 'no', updated_at: '2026-09-02T00:00:00Z', created_at: '2026-09-01T00:00:00Z', failing_checks: [] },
    { number: 12, title: 'blocked', labels: [], mergeable: 'no', ci_status: 'failing', updated_at: '2026-09-03T00:00:00Z', created_at: '2026-09-02T00:00:00Z', failing_checks: ['ci/test'] },
    { number: 13, title: 'review', labels: [], review_url: 'https://reviews/13', updated_at: '2026-09-04T00:00:00Z', created_at: '2026-09-03T00:00:00Z' },
    { number: 14, title: 'open', labels: [], updated_at: '2026-09-05T00:00:00Z', created_at: '2026-09-04T00:00:00Z' },
    { number: 15, title: 'draft failing', labels: [], draft: true, ci_status: 'failing', failing_checks: ['ci/lint'], updated_at: '2026-09-06T00:00:00Z', created_at: '2026-09-05T00:00:00Z' }
  ],
  heldPrs: [
    { number: 9, title: 'held pr', labels: ['hold'], updated_at: '2026-08-30T00:00:00Z', created_at: '2026-08-29T00:00:00Z' }
  ]
}];
_overviewLastRepos = repos;
const issueRows = overviewCsvRows('issues');
const prRows = overviewCsvRows('prs');
process.stdout.write(JSON.stringify({
  issueBands: issueRows.map(r => ({ number: r.number, band: r.band, stale: r.stale, held: r.held, signals: r.state_signals })),
  prBands: prRows.map(r => ({ number: r.number, band: r.band, stale: r.stale, held: r.held, signals: r.signals })),
  issueCSV: overviewCsv(issueRows, overviewCsvColumns('issues')),
  prCSV: overviewCsv(prRows, overviewCsvColumns('prs')),
  cells: [overviewCsvCell('-5'), overviewCsvCell('-@risk'), overviewCsvCell(['=a', 'b'])]
}));
`)
	out, err := exec.Command(node, "-e", script.String()).CombinedOutput()
	if err != nil {
		t.Fatalf("node parity failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	var js struct {
		IssueBands []map[string]any `json:"issueBands"`
		PRBands    []map[string]any `json:"prBands"`
		IssueCSV   string           `json:"issueCSV"`
		PRCSV      string           `json:"prCSV"`
		Cells      []string         `json:"cells"`
	}
	if err := json.Unmarshal(out, &js); err != nil {
		t.Fatalf("decode node output: %v", err)
	}
	status := parityStatus9102(t)
	s := newTestServer()
	s.deps = &Dependencies{Config: &config.Config{HiveID: "h1", Dashboard: config.DashboardConfig{}}}
	now := mustTime9102(t, "2026-09-27T00:00:00Z")
	issueRows, _ := s.overviewRows(status, overviewKindIssues, config.DashboardIssueBandsConfig{}, overviewFilters{}, now)
	prRows, _ := s.overviewRows(status, overviewKindPRs, config.DashboardIssueBandsConfig{}, overviewFilters{}, now)
	issueCSV := overviewCSV(issueRows, overviewCSVColumns(overviewKindIssues))
	prCSV := overviewCSV(prRows, overviewCSVColumns(overviewKindPRs))
	if issueCSV != js.IssueCSV {
		t.Fatalf("issue CSV diverged from browser\nGo:\n%s\nJS:\n%s", issueCSV, js.IssueCSV)
	}
	if prCSV != js.PRCSV {
		t.Fatalf("PR CSV diverged from browser\nGo:\n%s\nJS:\n%s", prCSV, js.PRCSV)
	}
	if got := []string{overviewCSVCell("-5"), overviewCSVCell("-@risk"), overviewCSVCell([]any{"=a", "b"})}; strings.Join(got, "|") != strings.Join(js.Cells, "|") {
		t.Fatalf("cell encoding = %#v, want %#v", got, js.Cells)
	}
}

func parityStatus9102(t *testing.T) *StatusPayload {
	t.Helper()
	mt := func(s string) time.Time { return mustTime9102(t, s) }
	return &StatusPayload{HiveID: "h1", GitHubBaseURL: "https://github.com", Repos: []FrontendRepo{{
		Full: "octo/demo",
		ActionableIssues: []any{
			github.Issue{Number: 1, Title: "=formula", Labels: []string{}, UpdatedAt: mt("2026-09-27T00:00:00Z"), CreatedAt: mt("2026-09-01T00:00:00Z"), URL: "https://github.com/octo/demo/issues/1"},
			github.Issue{Number: 2, Title: "+sum", Labels: []string{"hive/claimed-by-bot"}, UpdatedAt: mt("2026-09-12T00:00:00Z"), CreatedAt: mt("2026-09-01T00:00:00Z")},
			github.Issue{Number: 3, Title: "-@risky", Labels: []string{"Agent/Strategist"}, UpdatedAt: mt("2026-09-11T23:59:59Z"), CreatedAt: mt("2026-09-01T00:00:00Z")},
			github.Issue{Number: 4, Title: "\tTabbed", Labels: []string{"agent/quality", "approved-direction"}, UpdatedAt: mt("2026-09-13T00:00:00Z"), CreatedAt: mt("2026-09-01T00:00:00Z")},
			github.Issue{Number: 5, Title: "Needs \"quotes\", comma", Labels: []string{"NEEDS-HUMAN"}, UpdatedAt: mt("2026-09-10T00:00:00Z"), CreatedAt: mt("2026-09-01T00:00:00Z")},
			github.Issue{Number: 6, Title: "-5", Labels: []string{"hive/likely-done"}, UpdatedAt: mt("2026-09-09T00:00:00Z"), CreatedAt: mt("2026-09-01T00:00:00Z")},
			github.Issue{Number: 7, Title: "@hold", Labels: []string{"hold"}, HumanAcknowledged: true, UpdatedAt: mt("2026-09-08T00:00:00Z"), CreatedAt: mt("2026-09-01T00:00:00Z")},
		},
		HeldIssues: []any{github.HoldItem{Number: 8, Repo: "octo/demo", Title: "held via list", Labels: []string{"agent/triage", "hold"}, CreatedAt: mt("2026-09-07T00:00:00Z"), URL: "https://github.com/octo/demo/issues/8"}},
		OpenPrs: []any{
			FrontendPR{PullRequest: github.PullRequest{Number: 10, Title: "needs human", Author: "alice", Labels: []string{"needs-human"}, UpdatedAt: mt("2026-09-01T00:00:00Z"), CreatedAt: mt("2026-08-31T00:00:00Z")}},
			FrontendPR{PullRequest: github.PullRequest{Number: 11, Title: "eligible but no", Labels: []string{}, Mergeable: github.MergeableNo, UpdatedAt: mt("2026-09-02T00:00:00Z"), CreatedAt: mt("2026-09-01T00:00:00Z")}, MergeVerdict: &github.MergeVerdict{State: github.MergeVerdictEligible}},
			FrontendPR{PullRequest: github.PullRequest{Number: 12, Title: "blocked", Labels: []string{}, Mergeable: github.MergeableNo, CIStatus: "failing", FailingChecks: []string{"ci/test"}, UpdatedAt: mt("2026-09-03T00:00:00Z"), CreatedAt: mt("2026-09-02T00:00:00Z")}},
			FrontendPR{PullRequest: github.PullRequest{Number: 13, Title: "review", Labels: []string{}, ReviewURL: "https://reviews/13", UpdatedAt: mt("2026-09-04T00:00:00Z"), CreatedAt: mt("2026-09-03T00:00:00Z")}},
			FrontendPR{PullRequest: github.PullRequest{Number: 14, Title: "open", Labels: []string{}, UpdatedAt: mt("2026-09-05T00:00:00Z"), CreatedAt: mt("2026-09-04T00:00:00Z")}},
			FrontendPR{PullRequest: github.PullRequest{Number: 15, Title: "draft failing", Labels: []string{}, Draft: true, CIStatus: "failing", FailingChecks: []string{"ci/lint"}, UpdatedAt: mt("2026-09-06T00:00:00Z"), CreatedAt: mt("2026-09-05T00:00:00Z")}},
		},
		HeldPrs: []any{github.PullRequest{Number: 9, Title: "held pr", Labels: []string{"hold"}, UpdatedAt: mt("2026-08-30T00:00:00Z"), CreatedAt: mt("2026-08-29T00:00:00Z")}},
	}}}
}

func TestOverviewExportHandlers9102(t *testing.T) {
	s := NewServerWithAuth(0, "secret", nil)
	s.deps = &Dependencies{Config: &config.Config{HiveID: "h1", Dashboard: config.DashboardConfig{AuthToken: "secret"}}}
	s.statusMu.Lock()
	s.status = parityStatus9102(t)
	s.statusMu.Unlock()

	unauth := httptest.NewRecorder()
	s.Handler().ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/api/overview/issues.json", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", unauth.Code)
	}

	badReq := httptest.NewRequest(http.MethodGet, "/api/overview/issues.json?band=bogus", nil)
	badReq.Header.Set("Authorization", "Bearer secret")
	bad := httptest.NewRecorder()
	s.Handler().ServeHTTP(bad, badReq)
	if bad.Code != http.StatusBadRequest || !strings.Contains(bad.Body.String(), "ready") || !strings.Contains(bad.Body.String(), "done") {
		t.Fatalf("bad band response = %d %q", bad.Code, bad.Body.String())
	}

	jsonReq := httptest.NewRequest(http.MethodGet, "/api/overview/prs.json?band=blocked&repo=octo/demo&held=false", nil)
	jsonReq.Header.Set("Authorization", "Bearer secret")
	jsonRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(jsonRec, jsonReq)
	if jsonRec.Code != http.StatusOK {
		t.Fatalf("json status = %d body=%s", jsonRec.Code, jsonRec.Body.String())
	}
	var body overviewJSONResponse
	if err := json.Unmarshal(jsonRec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	if body.Kind != overviewKindPRs || body.HiveID != "h1" || len(body.Bands) != len(prBandOrder) || len(body.Rows) != 2 {
		t.Fatalf("unexpected json shape: kind=%s hive=%s bands=%d rows=%d", body.Kind, body.HiveID, len(body.Bands), len(body.Rows))
	}

	csvReq := httptest.NewRequest(http.MethodGet, "/api/overview/issues.csv?band=agent-filed", nil)
	csvReq.Header.Set("Authorization", "Bearer secret")
	csvRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(csvRec, csvReq)
	if csvRec.Code != http.StatusOK {
		t.Fatalf("csv status = %d", csvRec.Code)
	}
	cd := csvRec.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "hive-h1-issues-needs-triage-") || !strings.HasSuffix(cd, ".csv\"") {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	if !strings.Contains(csvRec.Body.String(), "Needs triage") {
		t.Fatalf("filtered csv missing agent-filed row: %s", csvRec.Body.String())
	}
}

func TestOverviewExportHelperBranches9102(t *testing.T) {
	now := mustTime9102(t, "2026-09-27T00:00:00Z")
	cfg := config.DashboardIssueBandsConfig{WaitingLabels: []string{"Wait-Human", "wait-human", ""}, DoneLabels: []string{"Done"}, StaleDays: 3}
	pr := github.PullRequest{
		Number:             21,
		Repo:               "octo/demo",
		Title:              "review signals",
		AppAuthored:        true,
		HiveAttributed:     true,
		Labels:             []string{"lgtm", "hold/review"},
		CreatedAt:          now.Add(-time.Hour),
		UpdatedAt:          now.Add(-4 * 24 * time.Hour),
		CIStatus:           "failing",
		FailingChecks:      []string{"ci"},
		Mergeable:          github.MergeableNo,
		MergeableState:     "dirty",
		RequestedReviewers: []string{"alice"},
		RequestedTeams:     []string{"team-a"},
		CommentCount:       1,
		ReviewThreadCount:  2,
		Protection: &github.ProtectionFacts{
			ReviewDecision:     github.ReviewDecisionChangesRequested,
			ChangesRequestedBy: []string{"bob"},
			ApprovalsGiven:     2,
		},
	}
	info := PRBand(pr, false, cfg, now)
	if info.Band != "waiting" || info.Role != "app" || !info.Stale || !strings.Contains(info.HoldReason, "opened by a hive agent") {
		t.Fatalf("PRBand helper result = %+v", info)
	}
	if got := prBandSpec("waiting", cfg).Rule; !strings.Contains(got, "wait-human") || strings.Contains(got, "Wait-Human, wait-human") {
		t.Fatalf("custom waiting rule did not normalize labels: %q", got)
	}
	rows := []any{overviewPRCSVRow(pr, &github.MergeVerdict{State: github.MergeVerdictOutstanding, Reason: "review"}, true, info, cfg, "")}
	csv := overviewCSV(rows, overviewCSVColumns(overviewKindPRs))
	for _, want := range []string{"outstanding: review", "CHANGES_REQUESTED", "review requested from @alice, team team-a", "1 comment, 2 review threads"} {
		if !strings.Contains(csv, want) {
			t.Fatalf("overview PR CSV missing %q:\n%s", want, csv)
		}
	}
	issue := github.Issue{
		Number: 1,
		Repo:   "octo/demo",
		Title:  "linked",
		LinkedPRs: []github.IssueLinkedPR{
			{Number: 2, Repo: "octo/demo", State: "open"},
			{Number: 3, Merged: true},
		},
	}
	issueRow := overviewIssueCSVRow(issue, false, IssueBand(issue, false, cfg, now), cfg, "")
	if links := issueRow["linked_prs"].([]any); len(links) != 2 || links[1] != "#3 merged" {
		t.Fatalf("linked PR labels = %#v", links)
	}
	if _, ok := frontendIssue(&github.Issue{Number: 2}); !ok {
		t.Fatal("frontendIssue must accept *github.Issue")
	}
	if _, ok := frontendIssue(&github.HoldItem{Number: 3}); !ok {
		t.Fatal("frontendIssue must accept *github.HoldItem")
	}
	if _, ok := frontendPullRequest(&FrontendPR{PullRequest: pr}); !ok {
		t.Fatal("frontendPullRequest must accept *FrontendPR")
	}
	if _, ok := frontendPullRequest(&github.PullRequest{Number: 4}); !ok {
		t.Fatal("frontendPullRequest must accept *github.PullRequest")
	}
	if got := overviewFilename("bad hive/id", overviewKindIssues, overviewFilters{}, nil, now); !strings.HasPrefix(got, "hive-bad-hive-id-issues-") {
		t.Fatalf("safe filename = %q", got)
	}
	if issueBandRank("missing") != len(issueBandOrder) || prBandRank("missing") != len(prBandOrder) {
		t.Fatal("unknown band rank should sort last")
	}
}

func mustTime9102(t *testing.T, raw string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("parse time %s: %v", raw, err)
	}
	return parsed
}
