package dashboard

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"
)

func TestRepoCardIssueBandsStaticWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`<div id="repos-legend" class="repo-legend"></div>`,
		"function renderRepoLegend()",
		"function groupedRepoIssues(issues)",
		"function issueLinkedPRState(issue)",
		"const issuePills = groupedRepoIssues(r.actionableIssues || []).map(g => {",
		"repoStaleIssueCount(r.actionableIssues || [])",
		"window._repoIssueBandConfig = normalizeIssueBandConfig(cfg.dashboard_issue_bands || {});",
		".repo-issue-pill.waiting",
		".repo-issue-pill.agent-filed",
		"REPO_LEGEND_COLLAPSED_KEY_PREFIX",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}

func TestRepoCardIssueBandsBehaviour(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: repository issue banding was not executed")
	}
	html := indexHTML(t)
	funcs := []string{
		"normalizeIssueBandConfig",
		"repoIssueBandConfig",
		"issueLabelSet",
		"issueHasAnyLabel",
		"issueAgentRole",
		"issueClaimed",
		"issueLinkedPRState",
		"issueUpdatedAt",
		"issueIsStale",
		"repoStaleIssueCount",
		"issueBandInfo",
		"issueBandLabel",
		"issueBandRank",
		"groupedRepoIssues",
	}
	var script strings.Builder
	script.WriteString(`
const assert = require('node:assert/strict');
const REPO_ISSUE_BAND_DEFAULTS = {
  waitingLabels: ['blocked', 'needs-decision', '2-discussing', 'Epic', 'needs-human', 'needs-triage'],
  doneLabels: ['hive/already-done'],
  staleDays: 14
};
const MS_PER_DAY = 24 * 60 * 60 * 1000;
const window = { _repoIssueBandConfig: Object.assign({}, REPO_ISSUE_BAND_DEFAULTS) };
Date.now = () => Date.parse('2026-09-25T00:00:00Z');
`)
	for _, name := range funcs {
		script.WriteString(jsFunc(t, html, name))
		script.WriteByte('\n')
	}
	script.WriteString(`
window._repoIssueBandConfig = normalizeIssueBandConfig({ waiting_labels: ['wait-human'], done_labels: ['done-custom'], stale_days: 7 });
assert.equal(issueBandInfo({ labels: ['done-custom', 'wait-human'], updated_at: '2026-09-24T00:00:00Z' }).band, 'done');
assert.equal(issueBandInfo({ labels: ['wait-human', 'agent/strategist'], assignees: ['dan'], updated_at: '2026-09-24T00:00:00Z' }).band, 'waiting');
assert.equal(issueBandInfo({ labels: ['agent/strategist'], assignees: ['bot'], updated_at: '2026-09-24T00:00:00Z' }).band, 'in-progress');
assert.equal(issueBandInfo({ labels: ['agent/quality'], updated_at: '2026-09-24T00:00:00Z' }).band, 'agent-filed');
assert.equal(issueBandInfo({ labels: [], updated_at: '2026-09-24T00:00:00Z' }).band, 'ready');
const groups = groupedRepoIssues([
  { number: 5, labels: ['done-custom'], updated_at: '2026-09-20T00:00:00Z' },
  { number: 4, labels: ['wait-human'], updated_at: '2026-09-18T00:00:00Z' },
  { number: 3, labels: ['agent/quality'], updated_at: '2026-09-17T00:00:00Z' },
  { number: 2, labels: [], assignees: ['dan'], updated_at: '2026-09-16T00:00:00Z' },
  { number: 1, labels: [], updated_at: '2026-09-15T00:00:00Z' },
  { number: 6, labels: [], updated_at: '2026-09-14T00:00:00Z' }
]);
assert.deepEqual(groups.map(g => g.band), ['ready', 'in-progress', 'agent-filed', 'waiting', 'done']);
assert.deepEqual(groups[0].issues.map(e => e.issue.number), [6, 1]);
assert.equal(repoStaleIssueCount([{ updated_at: '2026-09-01T00:00:00Z' }, { updated_at: '2026-09-24T00:00:00Z' }]), 1);
`)
	if out, err := exec.Command(node, "-e", script.String()).CombinedOutput(); err != nil {
		t.Fatalf("repository issue banding check failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}

func TestConfigEndpointExposesIssueBandTaxonomy(t *testing.T) {
	s := covApiServer(t)
	s.deps.Config.Dashboard.IssueBands.WaitingLabels = []string{"blocked", "needs-decision"}
	s.deps.Config.Dashboard.IssueBands.DoneLabels = []string{"hive/already-done"}
	s.deps.Config.Dashboard.IssueBands.StaleDays = 21
	rec := doOwnerGet(s, "/api/config")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/config: expected 200, got %d", rec.Code)
	}
	var body struct {
		Bands struct {
			WaitingLabels []string `json:"waiting_labels"`
			DoneLabels    []string `json:"done_labels"`
			StaleDays     int      `json:"stale_days"`
		} `json:"dashboard_issue_bands"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if strings.Join(body.Bands.WaitingLabels, ",") != "blocked,needs-decision" || strings.Join(body.Bands.DoneLabels, ",") != "hive/already-done" || body.Bands.StaleDays != 21 {
		t.Fatalf("dashboard_issue_bands = %+v", body.Bands)
	}
}
