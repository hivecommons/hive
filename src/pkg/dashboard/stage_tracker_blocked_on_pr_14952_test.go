package dashboard

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// A `blocked` issue parked on an unmerged PR names it: the review-backlog
// marker (or the enumeration's "blocked on owner/repo#N" reason) becomes
// blocked_on_pr, its state comes from what the hive already holds, and the
// caption says what lifts the block instead of "No stage recorded yet"
// (hivecommons/hive#14952).
func TestStageTrackerBlockedOnPRNamesTheBlocker(t *testing.T) {
	now := time.Date(2026, 10, 28, 13, 18, 0, 0, time.UTC)
	s := newTestServer()
	status := minimalPayload()
	status.Repos = []FrontendRepo{{
		Name: "hive", Full: "hivecommons/hive",
		ActionableIssues: []any{
			github.Issue{Number: 14863, Title: "Review backlog: body marker", Author: "hive-app[bot]", Labels: []string{"blocked"},
				Body: "**Blocked on PR #14862.**\n\n" + github.BlockedOnMarker("hivecommons/hive", 14862)},
			github.Issue{Number: 14870, Title: "Bare blocked label", Author: "pat", AuthorIsHuman: true, Labels: []string{"blocked"}},
			github.Issue{Number: 14871, Title: "Marker without the label", Author: "pat", AuthorIsHuman: true, Labels: []string{"stage/010-actionable"},
				Body: github.BlockedOnMarker("hivecommons/hive", 14862)},
		},
		OpenPrs: []any{
			FrontendPR{PullRequest: github.PullRequest{Number: 14862, Title: "The blocker", Author: "pat", Labels: []string{"stage/050-review-pending"}}},
		},
		NonActionableIssues: []any{
			github.RepoWorkIssue{Repo: "hivecommons/hive", Number: 14867, Title: "Review backlog: filtered reason", Labels: []string{"blocked"},
				Bucket: "filtered", Reason: "blocked on hivecommons/crux#77"},
		},
	}}
	s.UpdateStatus(status)
	resp := s.buildStageTracker(s.status, s.stageStore, now)

	body := trackerItem(t, resp, "issue", 14863)
	if body.BlockedOnPR == nil {
		t.Fatalf("#14863 blocked_on_pr missing (mods %v caption %q)", body.Modifiers, body.Caption)
	}
	if got := *body.BlockedOnPR; got.Repo != "hivecommons/hive" || got.Number != 14862 || got.URL != "https://github.com/hivecommons/hive/pull/14862" || got.State != "open" {
		t.Errorf("#14863 blocked_on_pr = %+v, want hivecommons/hive#14862 open with its PR URL", got)
	}
	if body.Caption != "Blocked on PR #14862 — lifts when it merges" {
		t.Errorf("#14863 caption = %q", body.Caption)
	}
	if !hasMod(body, modBlocked) || len(body.BlockedOn) != 0 {
		t.Errorf("#14863 mods %v blocked_on %v: the label pill stays, the dependency list untouched", body.Modifiers, body.BlockedOn)
	}

	reason := trackerItem(t, resp, "issue", 14867)
	if reason.BlockedOnPR == nil || reason.BlockedOnPR.Repo != "hivecommons/crux" || reason.BlockedOnPR.Number != 77 || reason.BlockedOnPR.State != "" {
		t.Errorf("#14867 blocked_on_pr = %+v, want hivecommons/crux#77 with unknown state (no GitHub call)", reason.BlockedOnPR)
	}
	if reason.Caption != "Blocked on PR hivecommons/crux#77 — lifts when it merges" {
		t.Errorf("#14867 caption = %q", reason.Caption)
	}

	if bare := trackerItem(t, resp, "issue", 14870); bare.BlockedOnPR != nil {
		t.Errorf("#14870 with no marker = %+v, want none", bare.BlockedOnPR)
	}
	if unlabelled := trackerItem(t, resp, "issue", 14871); unlabelled.BlockedOnPR != nil {
		t.Errorf("#14871 without the blocked label = %+v, want none (the sweep already lifted it)", unlabelled.BlockedOnPR)
	}
}

func TestStageTrackerBlockedOnPRStateFromMergeRing(t *testing.T) {
	items := []StageTrackerItem{
		{Repo: "o/r", Number: 1, Kind: "issue", BlockedOnPR: &StageTrackerBlockedOnPR{Repo: "o/r", Number: 9}},
		{Repo: "o/r", Number: 2, Kind: "issue", BlockedOnPR: &StageTrackerBlockedOnPR{Repo: "O/R", Number: 10}},
		{Repo: "o/r", Number: 10, Kind: "pr"},
	}
	stageTrackerResolveBlockedOnPRState(items, []StageTrackerMerged{{Repo: "o/r", Number: 9}})
	if items[0].BlockedOnPR.State != "merged" || items[1].BlockedOnPR.State != "open" {
		t.Errorf("states = %q, %q; want merged, open", items[0].BlockedOnPR.State, items[1].BlockedOnPR.State)
	}
}

func TestParseBlockedFilterReason(t *testing.T) {
	for _, c := range []struct {
		in   string
		repo string
		n    int
		ok   bool
	}{
		{"blocked on hivecommons/hive#14862", "hivecommons/hive", 14862, true},
		{"blocked", "", 0, false},
		{"blocked until 2026-10-28T12:00:00Z", "", 0, false},
		{"blocked on #5", "", 0, false},
		{"blocked on o/r#x", "", 0, false},
	} {
		repo, n, ok := github.ParseBlockedFilterReason(c.in)
		if repo != c.repo || n != c.n || ok != c.ok {
			t.Errorf("ParseBlockedFilterReason(%q) = %q, %d, %v", c.in, repo, n, ok)
		}
	}
}

// The pill reads "blocked on #N", links to the PR, and its tooltip, the row
// tooltip and the chevron tip all name the PR and what lifts the block.
func TestStageTrackerBlockedOnPRJS(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"function stageTrackerBlockedOnPRRef(item) {",
		"function stageTrackerBlockedOnPRTip(item) {",
		"' — lifts when it merges; re-triage if closed unmerged'",
		`<a class="ham-badge ham-badge-mod ham-blocked-on-pr ${esc(cls)}" href="${esc(blockedPRURL)}"`,
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the blocked-on-PR pill was NOT executed by this run")
	}
	script := strings.Join([]string{
		"const assert = require('assert');",
		"var _stageTracker = { stages: [] };",
		"function agentNowClockTime(ms, nowMs) { return 'T' + ms; }",
		"function agentNowAgo(ms) { return ms + 'ms'; }",
		"function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/\"/g, '&quot;'); }",
		"function stageTrackerHoldLabels() { return []; }",
		"function stageTrackerActorKind() { return ''; }",
		"function stageTrackerSoakLeft() { return ''; }",
		jsConstLine(t, html, "STAGE_TRACKER_MOD_PRIORITY"),
		jsConstLine(t, html, "STAGE_TRACKER_MOD_TEXT"),
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_DECISION"),
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL"),
		jsConstLine(t, html, "STAGE_TRACKER_TRAIL_HOPS"),
		jsFunc(t, html, "stageTrackerStageWord"),
		stageTrackerSentinelJS(t, html),
		jsConstLine(t, html, "STAGE_TRACKER_ATTENTION_STAGES"),
		jsConstLine(t, html, "STAGE_TRACKER_HUMAN_MODS"),
		jsConstLine(t, html, "STAGE_TRACKER_HANDLED_TEXT"),
		jsConstLine(t, html, "STAGE_TRACKER_HANDLED_NOTE"),
		jsFunc(t, html, "stageTrackerHeadline"),
		jsFunc(t, html, "stageTrackerRegressionHandled"),
		jsFunc(t, html, "stageTrackerRegressionText"),
		jsFunc(t, html, "stageTrackerStaleLabelsText"),
		jsFunc(t, html, "stageTrackerHopName"),
		jsFunc(t, html, "stageTrackerTrailText"),
		jsFunc(t, html, "stageTrackerBlockedTip"),
		jsFunc(t, html, "stageTrackerBlockedOnPRRef"),
		jsFunc(t, html, "stageTrackerBlockedOnPRTip"),
		jsFunc(t, html, "stageTrackerTooltip"),
		jsFunc(t, html, "stageTrackerChipsHtml"),
	}, "\n") + `
const item = { kind: 'issue', repo: 'hivecommons/hive', number: 14863, stage: 0, modifiers: ['blocked'], labels: ['blocked'],
  caption: 'Blocked on PR #14862 — lifts when it merges',
  blocked_on_pr: { repo: 'hivecommons/hive', number: 14862, url: 'https://github.com/hivecommons/hive/pull/14862', state: 'open' } };
const want = 'Blocked on PR hivecommons/hive#14862 (open) — lifts when it merges; re-triage if closed unmerged';
assert.strictEqual(stageTrackerBlockedOnPRTip(item), want);
const chip = stageTrackerChipsHtml(item);
assert.ok(chip.includes('>blocked on #14862</a>'), chip);
assert.ok(chip.includes('href="https://github.com/hivecommons/hive/pull/14862"'), chip);
assert.ok(chip.includes('title="' + want + '"') && chip.includes('aria-label="' + want + '"'), chip);
const tip = stageTrackerTooltip(item, Date.now());
assert.ok(tip.includes('Blocked on PR #14862 — lifts when it merges'), tip);
assert.ok(tip.includes(want), tip);
assert.strictEqual(stageTrackerBlockedTip(item), 'blocked on PR #14862');

const foreign = Object.assign({}, item, { blocked_on_pr: { repo: 'hivecommons/crux', number: 77, url: 'https://github.com/hivecommons/crux/pull/77' } });
assert.ok(stageTrackerChipsHtml(foreign).includes('>blocked on hivecommons/crux#77</a>'));
assert.strictEqual(stageTrackerBlockedOnPRTip(foreign), 'Blocked on PR hivecommons/crux#77 — lifts when it merges; re-triage if closed unmerged');

const bare = { kind: 'issue', repo: 'hivecommons/hive', number: 14870, stage: 0, modifiers: ['blocked'], labels: ['blocked'] };
assert.ok(/^<span class="ham-badge ham-badge-mod ham-mod-blocked">blocked<\/span>$/.test(stageTrackerChipsHtml(bare)), stageTrackerChipsHtml(bare));
assert.strictEqual(stageTrackerBlockedTip(bare), 'carries the blocked label');
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("blocked-on-PR pill check failed: %v\n%s", err, out)
	}
}
