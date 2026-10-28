package dashboard

import (
	"encoding/json"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The owner's rule (2026-10-27): until a PR is in the merge gate, the
// tracker must not show pills nobody can act on while the PR is in motion
// — sentinel, needs-human, needs-decision, a merge-time unauthorized step.
// Pills a person must act on right now (hold, draft, stalled, regressed,
// held workflow runs) always show. The split is computed server-side:
// every modifier carries an explicit ActionableNow class, and
// stageTrackerModifiers hands the row its shown and deferred lists.
func TestStageTrackerModifiers_DeferredUntilMergeGate(t *testing.T) {
	now := time.Now()
	heldRuns := prstage.StageReasonUnauthorizedPrefix + prstage.UnauthorizedStepApproveWorkflowRuns
	merge403 := prstage.StageReasonUnauthorizedPrefix + "merge"
	for _, tc := range []struct {
		name         string
		labels       []string
		draft        bool
		reason       string
		kind         string
		stage        stagelabels.Stage
		mirrored     bool
		wantShown    []string
		wantDeferred []string
	}{
		{name: "needs-human at 050 is deferred", labels: []string{"needs-human"}, kind: "pr", stage: stagelabels.ReviewPending, wantShown: []string{}, wantDeferred: []string{modNeedsHuman}},
		{name: "needs-human at 072 is deferred", labels: []string{"needs-human"}, kind: "pr", stage: stagelabels.ReturnedToContributor, wantShown: []string{}, wantDeferred: []string{modNeedsHuman}},
		{name: "needs-human at the gate shows", labels: []string{"needs-human"}, kind: "pr", stage: stagelabels.AwaitingMergeAdmission, wantShown: []string{modNeedsHuman}},
		{name: "needs-human at 085 shows", labels: []string{"needs-human"}, kind: "pr", stage: stagelabels.NeedsDecision, wantShown: []string{modNeedsHuman}},
		{name: "needs-decision at 050 is deferred", labels: []string{stagelabels.NeedsDecision.Label()}, kind: "pr", stage: stagelabels.ReviewPending, wantShown: []string{}, wantDeferred: []string{modNeedsDecision}},
		{name: "hold at 050 shows", labels: []string{"hold"}, kind: "pr", stage: stagelabels.ReviewPending, wantShown: []string{modHold}},
		{name: "draft at 050 shows", draft: true, kind: "pr", stage: stagelabels.ReviewPending, wantShown: []string{modDraft}},
		{name: "stalled at 050 shows", labels: []string{stagelabels.StalledLabel}, kind: "pr", stage: stagelabels.ReviewPending, wantShown: []string{modStalled}},
		{name: "unauthorized held runs at 072 shows", labels: []string{stagelabels.UnauthorizedLabel}, reason: heldRuns, kind: "pr", stage: stagelabels.ReturnedToContributor, wantShown: []string{modUnauthorized}},
		{name: "unauthorized held runs with approver suffix shows", labels: []string{stagelabels.UnauthorizedLabel}, reason: heldRuns + ":human-approved@pat", kind: "pr", stage: stagelabels.ReviewPending, wantShown: []string{modUnauthorized}},
		{name: "unauthorized merge 403 at 050 is deferred", labels: []string{stagelabels.UnauthorizedLabel}, reason: merge403, kind: "pr", stage: stagelabels.ReviewPending, wantShown: []string{}, wantDeferred: []string{modUnauthorized}},
		{name: "unauthorized merge 403 at the gate shows", labels: []string{stagelabels.UnauthorizedLabel}, reason: merge403, kind: "pr", stage: stagelabels.AwaitingMergeAdmission, wantShown: []string{modUnauthorized}},
		{name: "unauthorized without a step is deferred before the gate", labels: []string{stagelabels.UnauthorizedLabel}, reason: "contributor-lane:no-verdict", kind: "pr", stage: stagelabels.ReviewPending, wantShown: []string{}, wantDeferred: []string{modUnauthorized}},
		{name: "mixed row keeps order in both lists", labels: []string{"needs-human", "hold", stagelabels.UnauthorizedLabel, "needs-rebase"}, reason: merge403, kind: "pr", stage: stagelabels.Fixing, wantShown: []string{modHold, modNeedsRebase}, wantDeferred: []string{modUnauthorized, modNeedsHuman}},
		{name: "a PR with no stage yet is before the gate", labels: []string{"needs-human"}, kind: "pr", stage: stagelabels.None, wantShown: []string{}, wantDeferred: []string{modNeedsHuman}},
		{name: "an issue on its own stage keeps needs-human", labels: []string{"needs-human"}, kind: "issue", stage: stagelabels.Actionable, wantShown: []string{modNeedsHuman}},
		{name: "an issue at 045 keeps needs-human", labels: []string{"needs-human"}, kind: "issue", stage: stagelabels.IssueNeedsDecision, wantShown: []string{modNeedsHuman}},
		{name: "an issue riding its PR at 050 defers", labels: []string{"needs-human"}, kind: "issue", stage: stagelabels.ReviewPending, mirrored: true, wantShown: []string{}, wantDeferred: []string{modNeedsHuman}},
		{name: "an issue riding its PR at 085 shows", labels: []string{"needs-human"}, kind: "issue", stage: stagelabels.NeedsDecision, mirrored: true, wantShown: []string{modNeedsHuman}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shown, deferred := stageTrackerModifiers(tc.labels, "hv", stagelabels.Record{}, false, nil, now, tc.draft, tc.reason, tc.kind, tc.stage, tc.mirrored)
			if !slices.Equal(shown, tc.wantShown) || !slices.Equal(deferred, tc.wantDeferred) {
				t.Fatalf("shown %v deferred %v; want %v / %v", shown, deferred, tc.wantShown, tc.wantDeferred)
			}
		})
	}
}

// The class table itself: which markers are actionable now and which wait
// for the gate, independent of the stage.
func TestStageTrackerClassifyModifiers(t *testing.T) {
	heldRuns := prstage.StageReasonUnauthorizedPrefix + prstage.UnauthorizedStepApproveWorkflowRuns
	rec := stagelabels.Record{Stage: stagelabels.ReviewPending, RegressedFrom: stagelabels.MergeQueue, EnteredAt: time.Now()}
	got := stageTrackerClassifyModifiers([]string{"needs-human", stagelabels.NeedsDecision.Label(), "hold", stagelabels.UnauthorizedLabel, "needs-triage", stagelabels.PossiblyExisting.Label(), "needs-rebase", "blocked", stagelabels.StalledLabel}, "hv", rec, true, nil, time.Now(), true, heldRuns)
	want := map[string]bool{
		modStalled: true, modRegressed: true, modHold: true, modUnauthorized: true,
		modNeedsHuman: false, modNeedsDecision: false,
		modNeedsTriage: true, modPossibleDupe: true, modNeedsRebase: true, modBlocked: true, modDraft: true,
	}
	if len(got) != len(want) {
		t.Fatalf("classified %d modifiers, want %d: %+v", len(got), len(want), got)
	}
	for _, m := range got {
		if actionable, ok := want[m.Name]; !ok || actionable != m.ActionableNow {
			t.Fatalf("%s actionable_now = %v, want %v (known %v)", m.Name, m.ActionableNow, actionable, ok)
		}
	}
	if got := stageTrackerClassifyModifiers([]string{stagelabels.UnauthorizedLabel}, "hv", stagelabels.Record{}, false, nil, time.Now(), false, prstage.StageReasonUnauthorizedPrefix+"update-branch"); len(got) != 1 || got[0].ActionableNow {
		t.Fatalf("a merge-time unauthorized step must not be actionable now: %+v", got)
	}
	if !stageTrackerPillsDeferred("pr", stagelabels.Fixing.Number(), false) || stageTrackerPillsDeferred("pr", stagelabels.AwaitingMergeAdmission.Number(), false) || stageTrackerPillsDeferred("pr", stagelabels.ReadyForHumanMerge.Number(), false) {
		t.Fatal("the gate band starts at stage/080")
	}
	if stageTrackerPillsDeferred("issue", stagelabels.Claimed.Number(), false) || !stageTrackerPillsDeferred("issue", stagelabels.ReviewPending.Number(), true) {
		t.Fatal("issues defer only while riding a covering PR")
	}
}

// End to end through the payload: a PR at 050 wearing the sentinel label
// and needs-human draws no pill (both wait in deferred_modifiers, sentinel
// first) while sentinel_alert stays truthful; the same PR at 085 draws
// both; a PR at 050 held by hive:unauthorized on workflow runs keeps the
// pill, its step and its tooltip; a hold at 050 shows; and the nested
// sub-row under an issue mirrors the deferred list.
func TestStageTrackerDeferredPillsPayload(t *testing.T) {
	now := time.Date(2026, 10, 27, 14, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	heldRuns := prstage.StageReasonUnauthorizedPrefix + prstage.UnauthorizedStepApproveWorkflowRuns
	store.Observe(stagelabels.Key("acme/console", 141), stagelabels.ReturnedToContributor, heldRuns)
	store.Observe(stagelabels.Key("acme/console", 142), stagelabels.ReviewPending, prstage.StageReasonUnauthorizedPrefix+"merge")
	s.statusMu.Lock()
	s.status.Repos[0].OpenPrs = append(s.status.Repos[0].OpenPrs,
		FrontendPR{PullRequest: github.PullRequest{Number: 140, Title: "In review, flagged", Author: "mallory", Labels: []string{"stage/050-review-pending", config.DefaultSentinelLabel, "needs-human"}}},
		FrontendPR{PullRequest: github.PullRequest{Number: 141, Title: "Runs held", Author: "pat", Labels: []string{"stage/072-returned-to-contributor", stagelabels.UnauthorizedLabel}}},
		FrontendPR{PullRequest: github.PullRequest{Number: 142, Title: "Merge refused", Author: "pat", Labels: []string{"stage/050-review-pending", stagelabels.UnauthorizedLabel}}},
		FrontendPR{PullRequest: github.PullRequest{Number: 143, Title: "Held in review", Author: "pat", Labels: []string{"stage/050-review-pending", "hold"}}},
		FrontendPR{PullRequest: github.PullRequest{Number: 144, Title: "At the gate, flagged", Author: "mallory", Labels: []string{"stage/085-needs-decision", config.DefaultSentinelLabel, "needs-human"}}},
	)
	s.status.Repos[0].ActionableIssues = append(s.status.Repos[0].ActionableIssues,
		github.Issue{Number: 40, Title: "Covered by a flagged PR", Author: "pat", AuthorIsHuman: true, Labels: []string{"stage/050-review-pending"},
			LinkedPRs: []github.IssueLinkedPR{{Number: 140, State: "open"}}},
	)
	s.statusMu.Unlock()
	resp := s.buildStageTracker(s.status, store, now)

	flagged := trackerItem(t, resp, "pr", 140)
	if flagged.Stage != 50 || !flagged.SentinelAlert || len(flagged.Modifiers) != 0 || !slices.Equal(flagged.DeferredModifiers, []string{modSentinel, modNeedsHuman}) {
		t.Fatalf("PR 140 = stage %d alert %v mods %v deferred %v", flagged.Stage, flagged.SentinelAlert, flagged.Modifiers, flagged.DeferredModifiers)
	}
	raw, err := json.Marshal(flagged)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"modifiers":[],"deferred_modifiers":["sentinel","needs-human"]`) || !strings.Contains(string(raw), `"sentinel_alert":true`) {
		t.Fatalf("payload: %s", raw)
	}
	if held := trackerItem(t, resp, "pr", 141); held.Stage != 72 || !hasMod(held, modUnauthorized) || len(held.DeferredModifiers) != 0 || held.UnauthorizedStep != "approve workflow runs" || !strings.Contains(held.Unauthorized, "`actions: write`") {
		t.Fatalf("PR 141 = stage %d mods %v deferred %v step %q", held.Stage, held.Modifiers, held.DeferredModifiers, held.UnauthorizedStep)
	}
	if refused := trackerItem(t, resp, "pr", 142); refused.Stage != 50 || hasMod(refused, modUnauthorized) || !slices.Equal(refused.DeferredModifiers, []string{modUnauthorized}) || refused.UnauthorizedStep != "merge" {
		t.Fatalf("PR 142 = stage %d mods %v deferred %v step %q", refused.Stage, refused.Modifiers, refused.DeferredModifiers, refused.UnauthorizedStep)
	}
	if hold := trackerItem(t, resp, "pr", 143); !hasMod(hold, modHold) || len(hold.DeferredModifiers) != 0 {
		t.Fatalf("PR 143 = mods %v deferred %v", hold.Modifiers, hold.DeferredModifiers)
	}
	if gate := trackerItem(t, resp, "pr", 144); gate.Stage != 85 || !gate.SentinelAlert || !hasMod(gate, modNeedsHuman) || len(gate.DeferredModifiers) != 0 {
		t.Fatalf("PR 144 = stage %d alert %v mods %v deferred %v", gate.Stage, gate.SentinelAlert, gate.Modifiers, gate.DeferredModifiers)
	}
	issue := trackerItem(t, resp, "issue", 40)
	k := slices.IndexFunc(issue.LinkedPRs, func(lp StageTrackerLinkedPR) bool { return lp.Number == 140 })
	if k < 0 || len(issue.LinkedPRs[k].Modifiers) != 0 || !slices.Equal(issue.LinkedPRs[k].DeferredModifiers, []string{modSentinel, modNeedsHuman}) {
		t.Fatalf("issue 40 linked PRs = %+v", issue.LinkedPRs)
	}
	for _, m := range resp.Items {
		if m.Kind == "issue" && len(m.DeferredModifiers) != 0 && !m.Mirrored {
			t.Fatalf("issue #%d on its own stage must not defer pills: %+v", m.Number, m.DeferredModifiers)
		}
	}
}

// The frontend honours the server's split: a row whose deferred_modifiers
// names sentinel draws no sentinel chip and no modifier pill, stays out of
// Needs attention and does not breathe red; its tooltip alone says what
// waits at the gate. The same row at the gate (nothing deferred) draws
// both as before.
func TestStageTrackerDeferredPillsJS(t *testing.T) {
	html := indexHTML(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — deferred pills were NOT executed by this run")
	}
	script := strings.Join([]string{
		"const assert = require('assert');",
		"var _stageTracker = { stages: [{ number: 50, short: 'CI / Review' }, { number: 85, short: 'Needs decision' }] };",
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
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_STATIC"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_ACTIVE"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_HUMAN"),
		jsConstBlock(t, html, "STAGE_TRACKER_MOTION_BY_STAGE"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_AUTHOR_STAGES"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_HUMAN_MODS"),
		jsConstLine(t, html, "STAGE_TRACKER_HANDLED_TEXT"),
		jsConstLine(t, html, "STAGE_TRACKER_HANDLED_NOTE"),
		jsFunc(t, html, "stageTrackerMotion"),
		jsFunc(t, html, "stageTrackerHeadline"),
		jsFunc(t, html, "stageTrackerRegressionHandled"),
		jsFunc(t, html, "stageTrackerRegressionText"),
		jsFunc(t, html, "stageTrackerStaleLabelsText"),
		jsFunc(t, html, "stageTrackerHopName"),
		jsFunc(t, html, "stageTrackerTrailText"),
		jsFunc(t, html, "stageTrackerTooltip"),
		jsFunc(t, html, "stageTrackerChipsHtml"),
		jsFunc(t, html, "stageTrackerNeedsAttention"),
	}, "\n") + `
const HEAD = 'feedface0123456789abcdef0123456789abcdef';
// In review, flagged: the server emptied modifiers and deferred both.
const inReview = { kind: 'pr', stage: 50, modifiers: [], deferred_modifiers: ['sentinel', 'needs-human'], sentinel_alert: true, sentinel_reason: 'touches OWNERS', head: HEAD, repo: 'o/r', number: 140 };
assert.strictEqual(stageTrackerChipsHtml(inReview), '', 'no sentinel chip, no pill before the gate');
assert.strictEqual(stageTrackerSentinelChipHtml(inReview), '');
assert.ok(!stageTrackerNeedsAttention(inReview), 'deferred pills do not count');
assert.strictEqual(stageTrackerMotion(inReview), STAGE_TRACKER_MOTION_ACTIVE, '050 keeps its working motion');
const tip = stageTrackerTooltip(inReview, Date.now());
assert.ok(tip.includes(STAGE_TRACKER_DEFERRED_NOTE + 'sentinel, needs-human'), tip);
assert.ok(!tip.includes('Flags:') && !tip.includes(STAGE_TRACKER_SENTINEL_TIP), tip);
// At the gate the same row draws both and counts.
const atGate = { kind: 'pr', stage: 85, modifiers: ['needs-human'], sentinel_alert: true, sentinel_reason: 'touches OWNERS', head: HEAD, repo: 'o/r', number: 140 };
assert.ok(stageTrackerChipsHtml(atGate).includes('>needs human<'), stageTrackerChipsHtml(atGate));
assert.ok(stageTrackerNeedsAttention(atGate));
assert.ok(stageTrackerTooltip(atGate, Date.now()).includes(STAGE_TRACKER_SENTINEL_TIP));
const gate80 = { kind: 'pr', stage: 80, modifiers: [], sentinel_alert: true, head: HEAD };
assert.ok(stageTrackerChipsHtml(gate80).includes('ham-badge-sentinel'), 'sentinel chip at 080');
// Actionable-now pills show wherever the row stands.
assert.ok(stageTrackerChipsHtml({ kind: 'pr', stage: 50, modifiers: ['hold'], deferred_modifiers: ['needs-human'] }).includes('>hold<'));
assert.ok(stageTrackerChipsHtml({ kind: 'pr', stage: 72, modifiers: ['unauthorized'], unauthorized: 'needs actions: write' }).includes('>unauthorized<'));
assert.ok(stageTrackerNeedsAttention({ kind: 'pr', stage: 50, modifiers: ['hold'], deferred_modifiers: ['needs-human'] }));
// A payload without the field (older server) renders exactly as before.
assert.ok(stageTrackerChipsHtml({ kind: 'pr', stage: 50, modifiers: ['needs-human'], sentinel_alert: true }).includes('ham-badge-sentinel'));
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	for _, want := range []string{
		`deferred_modifiers: lp.deferred_modifiers || []`,
		`deferred_modifiers: lp.deferred_modifiers || item.deferred_modifiers || []`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("index.html lacks %q (sub-rows must carry the deferred list)", want)
		}
	}
}
