package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#11716: the issue BLOCKED pill names the open "blocked by"
// links, or says plainly that no reason is recorded. The server writes the
// text; the browser only displays it.

const noReasonSignal11716 = "labelled blocked — the label records no reason; see the issue"

func blockedSignals11716(info IssueBandInfo) []string {
	var out []string
	for _, s := range info.Signals {
		if s.Glyph == "⛔" {
			out = append(out, s.Label)
		}
	}
	return out
}

func TestIssueBandBlockedSignalNamesOpenLinks11716(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	cfg := config.DashboardIssueBandsConfig{}
	issue := github.Issue{Repo: "o/r", Number: 1, Labels: []string{"blocked"}, DependsOn: []github.IssueDependency{
		{Key: "o/r#15", Resolved: false},
		{Key: "o/r#9", Resolved: true},
		{Key: "o/r#12", Resolved: false},
	}}
	info := IssueBand(issue, false, cfg, now)
	got := blockedSignals11716(info)
	if len(got) != 1 || got[0] != "blocked by o/r#12, o/r#15" {
		t.Fatalf("blocked signals = %q, want one naming only the unresolved links in order", got)
	}
	for _, s := range info.Signals {
		if s.Label == "blocked" || strings.Contains(s.Label, "o/r#9") {
			t.Fatalf("signal %q repeats the bare label or names a resolved link", s.Label)
		}
	}
}

func TestIssueBandBlockedLabelAloneSaysNoReason11716(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	cfg := config.DashboardIssueBandsConfig{}
	for name, deps := range map[string][]github.IssueDependency{
		"no links":           nil,
		"only resolved link": {{Key: "o/r#9", Resolved: true}},
	} {
		info := IssueBand(github.Issue{Repo: "o/r", Number: 1, Labels: []string{"blocked"}, DependsOn: deps}, false, cfg, now)
		got := blockedSignals11716(info)
		if len(got) != 1 || got[0] != noReasonSignal11716 {
			t.Fatalf("%s: blocked signals = %q, want %q", name, got, noReasonSignal11716)
		}
	}
}

func TestIssueBandOpenLinkWithoutLabelKeepsBand11716(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	cfg := config.DashboardIssueBandsConfig{}
	deps := []github.IssueDependency{{Key: "o/r#12"}}
	with := IssueBand(github.Issue{Repo: "o/r", Number: 1, DependsOn: deps}, false, cfg, now)
	without := IssueBand(github.Issue{Repo: "o/r", Number: 1}, false, cfg, now)
	got := blockedSignals11716(with)
	if len(got) != 1 || got[0] != "blocked by o/r#12" {
		t.Fatalf("blocked signals = %q, want the link signal", got)
	}
	if with.Band != without.Band || with.Chip != without.Chip {
		t.Fatalf("band/chip = %q/%q, want unchanged %q/%q", with.Band, with.Chip, without.Band, without.Chip)
	}
}

func TestBlockedPillHoverShowsServerSignal11716(t *testing.T) {
	cfg := config.DashboardIssueBandsConfig{WaitingLabels: []string{"blocked"}}
	now := ownershipNow11565
	repos := []FrontendRepo{{
		Name: "r", Full: "o/r",
		ActionableIssues: []any{
			github.Issue{Repo: "o/r", Number: 30, Title: "has links", Labels: []string{"blocked"}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
				DependsOn: []github.IssueDependency{{Key: "o/r#15"}, {Key: "o/r#9", Resolved: true}, {Key: "o/r#12"}}},
			github.Issue{Repo: "o/r", Number: 31, Title: "label only", Labels: []string{"blocked"}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)},
			github.Issue{Repo: "o/r", Number: 32, Title: "blocked and assigned", Labels: []string{"blocked", "agent/strategist"}, Assignees: []string{"someone"}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)},
			github.Issue{Repo: "o/r", Number: 33, Title: "plain", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)},
		},
		HeldIssues: []any{
			github.HoldItem{Repo: "o/r", Number: 34, Title: "held and blocked", Type: "issue", Labels: []string{"blocked", "hold"}, CreatedAt: now.Add(-time.Hour)},
		},
	}}
	env := ownershipEnv11565(3)
	env.repoLevel = func(string) int { return 3 }
	decideRepoOwnership(repos, cfg, env)
	s := newTestServer()
	s.deps = &Dependencies{Config: &config.Config{Dashboard: config.DashboardConfig{IssueBands: cfg}}}
	raw, err := json.Marshal(s.statusWithOverviewBands(&StatusPayload{HiveID: "h", ACMMLevel: 3, Repos: repos}, now))
	if err != nil {
		t.Fatal(err)
	}
	script := yourTurnPageScript(t, map[string]bool{"renderOverviewCharts": true, "refreshSectionCardShell": true, "ensureRepoHoldPermission": true, "renderRepoLegend": true},
		`
function renderOverviewCharts() {} function refreshSectionCardShell() {} function ensureRepoHoldPermission() {} function renderRepoLegend() {}
window._lastStatus = `+string(raw)+`;
renderRepos(window._lastStatus.repos);
const html = els['repos'].innerHTML;
const unesc = s => s.replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&#96;/g, '\x60').replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&');
function issueRow(n) {
  const all = window._lastStatus.repos[0].actionableIssues.concat(window._lastStatus.repos[0].heldIssues);
  return all.find(i => i.number === n);
}
function pillTip(n) {
  const row = html.split(/(?=<span class="repo-issue-pill-wrap)/).find(s => s.includes('<span class="pill-num">#' + n + '</span>'));
  assert.ok(row, 'row #' + n);
  const m = row.match(/<button type="button" class="repo-pill-status[^"]*"[^>]*title="([^"]*)"[^>]*>([^<]*)</);
  assert.ok(m, '#' + n + ' has a status pill with a tip');
  return { tip: unesc(m[1]), word: unesc(m[2]) };
}
// The hover text v6 produced for every row, before the BLOCKED override.
function v6Tip(i, held) {
  const sig = i.signals || [];
  return (held ? heldReason(i) + ' — band: ' : '') + issueBandTip(i.band) + (sig.length ? ' — ' + sig.map(s => s.label).join(', ') : '');
}
function serverSignal(n) {
  const issue = issueRow(n);
  assert.equal(issue.signals.filter(s => s.glyph === '⛔').length, 1);
  return issue.signals.find(s => s.glyph === '⛔').label;
}
assert.equal(serverSignal(30), 'blocked by o/r#12, o/r#15');
assert.equal(pillTip(30).word, 'blocked');
assert.equal(pillTip(30).tip, serverSignal(30));
assert.equal(serverSignal(31), `+"`"+noReasonSignal11716+"`"+`);
assert.equal(pillTip(31).tip, serverSignal(31));

// A blocked row that is also assigned and agent-filed: the server's text
// first and verbatim, then the other signals in their existing order.
{
  const i = issueRow(32);
  assert.equal(i.chip, 'blocked');
  const others = i.signals.filter(s => s.glyph !== '⛔').map(s => s.label);
  assert.ok(others.includes('assigned or claimed') && others.some(l => l.startsWith('agent-filed by strategist')), JSON.stringify(others));
  const p = pillTip(32);
  assert.equal(p.word, 'blocked');
  assert.equal(p.tip, serverSignal(32) + ' — ' + others.join(', '));
  assert.notEqual(p.tip, v6Tip(i, false));
}

// A held row labelled blocked: its pill says held and the hover is v6's.
{
  const i = issueRow(34);
  assert.equal(i.chip, 'blocked');
  assert.ok(i.signals.some(s => s.glyph === '⛔'));
  const p = pillTip(34);
  assert.equal(p.word, 'held');
  assert.equal(p.tip, v6Tip(i, true));
  assert.ok(p.tip.includes(' — band: ' + issueBandTip(i.band)), p.tip);
}

// A plain issue: the hover is v6's.
{
  const i = issueRow(33);
  assert.ok(!i.chip);
  assert.equal(pillTip(33).tip, v6Tip(i, false));
}
`, "renderRepos")
	runYourTurnNode(t, script)
}
