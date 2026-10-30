package dashboard

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// TestStageTrackerMergeAttemptInPayload: a merge call in flight (or just
// back, merged) lights the PR at 100 "Merging" with merging_since and the
// caller, whatever stage the record says; a refused one leaves the stage
// and says why at the gate (hivecommons/hive#15245).
func TestStageTrackerMergeAttemptInPayload(t *testing.T) {
	now := time.Date(2026, 10, 28, 21, 31, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	started := now.Add(-3 * time.Second)
	attempts := []github.MergeAttempt{{Repo: "acme/console", Number: 103, Path: github.PRAuditPathSweep, StartedAt: started}}
	s.mergeAttempts = func() []github.MergeAttempt { return attempts }

	// PR 103 is recorded at 050 review-pending; the sweep's merge call on it
	// is in flight.
	pr := trackerItem(t, s.buildStageTracker(s.status, store, now), "pr", 103)
	if pr.Stage != stagelabels.MergeQueue.Number() || pr.StageSlug != "merge-queue" {
		t.Fatalf("in-flight merge must light 100: stage=%d slug=%s", pr.Stage, pr.StageSlug)
	}
	if pr.MergeGate == nil || pr.MergeGate.State != MergeGateStateMerging || pr.MergeGate.MergingSince == nil || !pr.MergeGate.MergingSince.Equal(started) || pr.MergeGate.MergePath != github.PRAuditPathSweep || pr.MergeGate.MergeFailed != "" {
		t.Fatalf("in-flight merge_gate: %+v", pr.MergeGate)
	}
	if !strings.HasPrefix(pr.Caption, "Merging now") || pr.Actor != StageActorHive || pr.StageActor == nil || pr.StageActor.Kind != StageActorHive {
		t.Errorf("in-flight caption/actor: %q %q %+v", pr.Caption, pr.Actor, pr.StageActor)
	}

	// Just merged, PR still listed by a stale status: still 100 merging.
	attempts = []github.MergeAttempt{{Repo: "acme/console", Number: 103, Path: github.PRAuditPathDashboard, StartedAt: started, EndedAt: now.Add(-time.Second), Merged: true}}
	pr = trackerItem(t, s.buildStageTracker(s.status, store, now), "pr", 103)
	if pr.Stage != stagelabels.MergeQueue.Number() || pr.MergeGate == nil || pr.MergeGate.State != MergeGateStateMerging || pr.MergeGate.MergePath != github.PRAuditPathDashboard {
		t.Fatalf("merged-but-listed: stage=%d gate=%+v", pr.Stage, pr.MergeGate)
	}

	// Refused: the stage stays as recorded (050, off the gate) and the gate
	// narration says merge-failed with the reason.
	attempts = []github.MergeAttempt{{Repo: "acme/console", Number: 103, Path: github.PRAuditPathDashboard, StartedAt: started, EndedAt: now.Add(-time.Second), Error: "405 Pull Request is not mergeable"}}
	pr = trackerItem(t, s.buildStageTracker(s.status, store, now), "pr", 103)
	if pr.Stage != stagelabels.ReviewPending.Number() {
		t.Fatalf("refused merge must not move the stage: %d", pr.Stage)
	}
	if pr.MergeGate == nil || pr.MergeGate.State != MergeGateStateMergeFailed || pr.MergeGate.MergeFailed != "405 Pull Request is not mergeable" || pr.MergeGate.MergeFailedAt == nil || pr.MergeGate.MergingSince == nil {
		t.Fatalf("refused merge_gate: %+v", pr.MergeGate)
	}
	// A refusal on a PR already on the gate (102 soaking) replaces its state
	// but keeps the lane's facts.
	attempts = []github.MergeAttempt{{Repo: "acme/console", Number: 102, StartedAt: started, EndedAt: now, Error: "409 head moved"}}
	pr = trackerItem(t, s.buildStageTracker(s.status, store, now), "pr", 102)
	if pr.Stage != stagelabels.PreMergeSoak.Number() || pr.MergeGate == nil || pr.MergeGate.State != MergeGateStateMergeFailed || pr.MergeGate.LastSweepAt == nil {
		t.Fatalf("refused on the gate: stage=%d gate=%+v", pr.Stage, pr.MergeGate)
	}

	// No attempt: unchanged.
	attempts = nil
	if pr = trackerItem(t, s.buildStageTracker(s.status, store, now), "pr", 103); pr.Stage != stagelabels.ReviewPending.Number() || pr.MergeGate != nil {
		t.Fatalf("without an attempt PR 103 is 050 off the gate: stage=%d gate=%+v", pr.Stage, pr.MergeGate)
	}
}

// TestStageTrackerMergeGateAttemptOverlay: the pure fold of an attempt
// into the gate narration.
func TestStageTrackerMergeGateAttemptOverlay(t *testing.T) {
	now := time.Date(2026, 10, 28, 21, 31, 0, 0, time.UTC)
	if mg := stageTrackerMergeGateAttempt(nil, github.MergeAttempt{}, false); mg != nil {
		t.Errorf("no attempt, no gate: %+v", mg)
	}
	live := github.MergeAttempt{Repo: "a/b", Number: 1, StartedAt: now, Path: github.PRAuditPathRelay}
	soak := now.Add(time.Minute)
	mg := stageTrackerMergeGateAttempt(&StageTrackerMergeGate{State: MergeGateStateSoaking, SoakUntil: &soak, SoakRemainingSeconds: 60, NextSweepAt: &soak}, live, true)
	if mg.State != MergeGateStateMerging || mg.MergingSince == nil || mg.SoakUntil != nil || mg.SoakRemainingSeconds != 0 || mg.NextSweepAt != nil || mg.MergePath != github.PRAuditPathRelay {
		t.Errorf("live attempt over soaking: %+v", mg)
	}
	if st := stageTrackerMergeAttemptStage(stagelabels.ReadyForHumanMerge, live, true); st != stagelabels.MergeQueue {
		t.Errorf("live attempt stage = %v, want 100", st)
	}
	failed := github.MergeAttempt{Repo: "a/b", Number: 1, StartedAt: now, EndedAt: now.Add(time.Second), Error: errors.New("422 boom").Error()}
	if st := stageTrackerMergeAttemptStage(stagelabels.ReadyForHumanMerge, failed, true); st != stagelabels.ReadyForHumanMerge {
		t.Errorf("failed attempt must keep the stage, got %v", st)
	}
	if mg = stageTrackerMergeGateAttempt(nil, failed, true); mg == nil || mg.State != MergeGateStateMergeFailed || mg.MergeFailed != "422 boom" || mg.MergeFailedAt == nil {
		t.Errorf("failed attempt off the gate: %+v", mg)
	}
	for _, c := range []struct{ path, want string }{{github.PRAuditPathDashboard, "dashboard"}, {github.PRAuditPathRelay, "relay"}, {github.PRAuditPathForge, "forge"}, {github.PRAuditPathSweep, "automerge sweep"}, {"", "automerge sweep"}} {
		if got := stageTrackerMergingCaption(github.MergeAttempt{Path: c.path}); !strings.Contains(got, c.want) {
			t.Errorf("caption for %q = %q, want %q", c.path, got, c.want)
		}
	}
}

// stageTrackerMergingHoldOffJS stands in for the merge-gate "Merging" hold
// (#15245) in harnesses that watch what happens the moment a merge is
// seen: no hold, every merge is read as fresh at once. The hold itself is
// covered by TestStageTrackerMergingHold.
const stageTrackerMergingHoldOffJS = `
var _stageTrackerMergingHold = {}; var _stageTrackerPRItems = {};
function stageTrackerPRItemsOf() { return {}; }
function stageTrackerMergingHoldNext() { return {}; }
function stageTrackerMergingHoldMask(raw) { return { state: raw, held: [], until: 0 }; }
function stageTrackerMergingHoldSchedule() {}
`

// TestStageTrackerMergingHold runs the SPA's merge-gate hold under node
// with an explicit clock (hivecommons/hive#15245): "Merging" stays lit
// for at least the merge banner's lifetime before a row advances to ✓,
// whether or not the payload ever showed the merge in flight; a refused
// merge drops the hold at once.
func TestStageTrackerMergingHold(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"const STAGE_TRACKER_MERGING_HOLD_MS = STAGE_TRACKER_MERGE_BANNER_MS;",
		"_stageTrackerMergingHold = stageTrackerMergingHoldNext(_stageTrackerMergingHold, d, _stageTrackerMergeState, raw, _stageTrackerPRItems, nowMs);",
		"stageTrackerMergingHoldView(_stageTracker, _stageTrackerMergingHold, nowMs)",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	runStageTrackerRowActionsScript(t, strings.Join([]string{
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL"),
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_BANNER_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_MERGING_HOLD_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_QUEUE"),
		jsFunc(t, html, "stageTrackerPRKey"),
		jsFunc(t, html, "stageTrackerMergeState"),
		jsFunc(t, html, "stageTrackerCarryOpen"),
		jsFunc(t, html, "stageTrackerNewlyMerged"),
		jsFunc(t, html, "stageTrackerPRItemsOf"),
		jsFunc(t, html, "stageTrackerMergingHoldLive"),
		jsFunc(t, html, "stageTrackerMergingHoldNext"),
		jsFunc(t, html, "stageTrackerMergingHoldMask"),
		jsFunc(t, html, "stageTrackerMergingHoldItem"),
		jsFunc(t, html, "stageTrackerMergingHoldView"),
	}, "\n")+`
const assert = require('assert');
assert.strictEqual(STAGE_TRACKER_MERGING_HOLD_MS, STAGE_TRACKER_MERGE_BANNER_MS, 'the hold is the toast lifetime');
const T0 = Date.parse('2026-10-28T21:31:00Z');
const iso = ms => new Date(ms).toISOString();
const pr = (n, stage, mg) => Object.assign({ kind: 'pr', repo: 'o/r', number: n, stage, title: 'feat ' + n }, mg ? { merge_gate: mg } : {});
const K = n => stageTrackerPRKey('o/r', n);

// One poll loop: feed payloads in order with an explicit clock, the way
// stageTrackerNoteMerges does, and return what each poll read as fresh.
function run(polls) {
  let holds = {}, state = null, items = {};
  const out = [];
  for (const [nowMs, d] of polls) {
    const raw = stageTrackerCarryOpen(state, stageTrackerMergeState(d));
    holds = stageTrackerMergingHoldNext(holds, d, state, raw, items, nowMs);
    const masked = stageTrackerMergingHoldMask(raw, holds, nowMs);
    items = stageTrackerPRItemsOf(d);
    const fresh = stageTrackerNewlyMerged(state, masked.state);
    state = masked.state;
    out.push({ fresh, held: masked.held, until: masked.until, holds, view: stageTrackerMergingHoldView(d, holds, nowMs) });
  }
  return out;
}

// 1. The sweep's merge call is seen in flight (merging_since) at T0; the
//    next poll (30 s later) reports the PR merged. The merge is masked
//    until T0 + hold; the board keeps a stage-100 "Merging" row for #1
//    with the gate saying the merge landed; the poll at/after the end
//    reads it as fresh and the synthesised row is gone.
let r = run([
  [T0 - 30000, { items: [pr(1, 60), pr(2, 50)] }],
  [T0 + 3000, { items: [pr(1, 100, { state: 'merging', merging_since: iso(T0), merge_path: 'sweep' }), pr(2, 50)] }],
  [T0 + 15000, { items: [pr(2, 50)], recently_merged: [{ repo: 'o/r', number: 1 }] }],
  [T0 + 29000, { items: [pr(2, 50)], recently_merged: [{ repo: 'o/r', number: 1 }] }],
  [T0 + STAGE_TRACKER_MERGING_HOLD_MS, { items: [pr(2, 50)], recently_merged: [{ repo: 'o/r', number: 1 }] }],
]);
assert.deepStrictEqual(r[1].fresh, [], 'in flight: nothing fresh');
assert.strictEqual(r[1].holds[K(1)].since, T0, 'hold opens from merging_since');
assert.strictEqual(r[1].holds[K(1)].path, 'sweep');
assert.strictEqual(r[1].view.items.length, 2, 'payload lists the PR: no synthesised row');
assert.deepStrictEqual(r[2].fresh, [], 'merged inside the hold: masked');
assert.deepStrictEqual(r[2].held, [K(1)]);
assert.strictEqual(r[2].until, T0 + STAGE_TRACKER_MERGING_HOLD_MS, 'release scheduled for the hold end');
const held = r[2].view.items.find(it => it.number === 1);
assert.ok(held && held.held_merging === true, 'held row synthesised');
assert.strictEqual(held.stage, STAGE_TRACKER_MERGE_QUEUE);
assert.strictEqual(held.stage_slug, 'merge-queue');
assert.strictEqual(held.merge_gate.state, 'merging');
assert.strictEqual(held.merge_gate.held, true);
assert.strictEqual(held.merge_gate.merging_since, iso(T0));
assert.strictEqual(held.merge_gate.merge_path, 'sweep');
assert.strictEqual(held.actor, 'hive');
assert.strictEqual(held.stage_actor.kind, 'hive');
assert.strictEqual(held.title, 'feat 1', 'row keeps its last item');
assert.deepStrictEqual(r[3].fresh, [], 'still masked one second before the end');
assert.deepStrictEqual(r[4].fresh, [K(1)], 'at the hold end the merge is fresh');
assert.deepStrictEqual(r[4].held, []);
assert.strictEqual(r[4].until, 0);
assert.deepStrictEqual(r[4].holds, {}, 'hold dropped once its window passed');
assert.strictEqual(r[4].view, r[4].view, 'view is the payload itself once nothing is held');
assert.strictEqual(r[4].view.items.length, 1);

// 2. A merge never seen in flight (060 at one poll, merged at the next):
//    the hold opens from that poll, from the last open item, so "Merging"
//    still shows before ✓.
r = run([
  [T0, { items: [pr(3, 60)] }],
  [T0 + 30000, { items: [], recently_merged: [{ repo: 'o/r', number: 3 }] }],
  [T0 + 30000 + STAGE_TRACKER_MERGING_HOLD_MS - 1, { items: [], recently_merged: [{ repo: 'o/r', number: 3 }] }],
  [T0 + 30000 + STAGE_TRACKER_MERGING_HOLD_MS, { items: [], recently_merged: [{ repo: 'o/r', number: 3 }] }],
]);
assert.deepStrictEqual(r[1].fresh, [], 'surprise merge: masked');
assert.strictEqual(r[1].holds[K(3)].since, T0 + 30000, 'hold opens at the poll that reported it');
assert.strictEqual(r[1].view.items.length, 1, 'row stays on the board');
assert.strictEqual(r[1].view.items[0].stage, STAGE_TRACKER_MERGE_QUEUE);
assert.strictEqual(r[1].view.items[0].merge_gate.merge_path, undefined, 'no path known');
assert.deepStrictEqual(r[2].fresh, []);
assert.deepStrictEqual(r[3].fresh, [K(3)], 'fresh exactly at the hold end');

// 3. A merge seen in flight long enough ago: no extra wait. Seen merging
//    at T0, merged reported at T0 + hold + 5 s: fresh at once.
r = run([
  [T0, { items: [pr(4, 100, { state: 'merging', merging_since: iso(T0) })] }],
  [T0 + STAGE_TRACKER_MERGING_HOLD_MS + 5000, { items: [], recently_merged: [{ repo: 'o/r', number: 4 }] }],
]);
assert.deepStrictEqual(r[1].fresh, [K(4)], 'hold already satisfied: fresh now');
assert.deepStrictEqual(r[1].holds, {});

// 4. A refused merge drops the hold at once; the row is the payload's
//    (merge-failed at the gate), not a synthesised one.
r = run([
  [T0, { items: [pr(5, 100, { state: 'merging', merging_since: iso(T0) })] }],
  [T0 + 2000, { items: [pr(5, 90, { state: 'merge-failed', merge_failed: '405 not mergeable' })] }],
]);
assert.ok(r[0].holds[K(5)]);
assert.deepStrictEqual(r[1].holds, {}, 'merge-failed drops the hold');
assert.strictEqual(r[1].view.items[0].merge_gate.state, 'merge-failed');
// …as does a PR that is back open at another gate state without merging.
r = run([
  [T0, { items: [pr(6, 100, { state: 'merging', merging_since: iso(T0) })] }],
  [T0 + 2000, { items: [pr(6, 95, { state: 'soaking' })] }],
]);
assert.deepStrictEqual(r[1].holds, {}, 'back to soaking: no hold');

// 5. Merging_since missing: the hold runs from first sight and is kept
//    across polls; a non-PR item never holds.
r = run([
  [T0, { items: [pr(7, 100, { state: 'merging' }), { kind: 'issue', repo: 'o/r', number: 8, stage: 100, merge_gate: { state: 'merging' } }] }],
  [T0 + 1000, { items: [pr(7, 100, { state: 'merging' })] }],
]);
assert.strictEqual(r[0].holds[K(7)].since, T0);
assert.strictEqual(r[1].holds[K(7)].since, T0, 'since kept from the first sight');
assert.strictEqual(Object.keys(r[0].holds).length, 1, 'issues do not hold');

// Mask leaves state untouched for keys without a live hold and does not
// mutate its input.
const raw = { a: true, b: false };
const m = stageTrackerMergingHoldMask(raw, { a: { since: T0 - STAGE_TRACKER_MERGING_HOLD_MS - 1 } }, T0);
assert.deepStrictEqual(m, { state: { a: true, b: false }, held: [], until: 0 });
assert.deepStrictEqual(raw, { a: true, b: false });
console.log('ok');
`)
}

// TestStageTrackerMergeGateInfoMerging: the gate pill's words for a live
// merge call, a held row and a refused merge (hivecommons/hive#15245).
func TestStageTrackerMergeGateInfoMerging(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, strings.Join([]string{
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL"),
		jsConstLine(t, html, "STAGE_TRACKER_PRE_MERGE_SOAK"),
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_GATE_WORD"),
		jsConstBlock(t, html, "STAGE_TRACKER_GATE_REASON_WORDS"),
		jsFunc(t, html, "stageTrackerAgoText"),
		jsFunc(t, html, "stageTrackerMergeGateStateOf"),
		jsFunc(t, html, "stageTrackerShortLeft"),
		jsFunc(t, html, "stageTrackerGateReasonText"),
		stageTrackerMergeGateInfoJS(t, html),
	}, "\n")+`
const assert = require('assert');
const now = Date.parse('2026-10-28T21:31:00Z');
const item = mg => ({ kind: 'pr', repo: 'o/r', number: 1, stage: 100, merge_gate: mg });
let info = stageTrackerMergeGateInfoFor(item({ state: 'merging', merging_since: new Date(now - 4000).toISOString(), merge_path: 'sweep' }), now);
assert.strictEqual(info.word, 'Merging');
assert.strictEqual(info.detail, 'merge call in flight');
assert.strictEqual(info.cue, 'merging');
assert.ok(info.tip.includes('automerge sweep') && info.tip.includes('in flight'), info.tip);
info = stageTrackerMergeGateInfoFor(item({ state: 'merging', merging_since: new Date(now - 4000).toISOString(), merge_path: 'dashboard', held: true }), now);
assert.strictEqual(info.detail, 'merge landed');
assert.ok(info.tip.includes('maintainer merge from the dashboard') && info.tip.includes('✓'), info.tip);
info = stageTrackerMergeGateInfoFor(item({ state: 'merging' }), now);
assert.strictEqual(info.detail, '', 'legacy 100 record: word alone');
info = stageTrackerMergeGateInfoFor(Object.assign(item({ state: 'merge-failed', merge_failed: 'merging PR o/r#1 (abc): 405 Pull Request is not mergeable\nmore', merge_failed_at: new Date(now - 9000).toISOString(), merge_path: 'relay' }), { stage: 90 }), now);
assert.strictEqual(info.state, 'merge-failed');
assert.strictEqual(info.word, 'Merge failed');
assert.strictEqual(info.detail, 'branch protection', 'kind words, never the raw error');
assert.strictEqual(info.cue, 'waiting');
assert.ok(info.tip.includes('hive-merge relay') && info.tip.includes('GitHub refused — branch protection blocks it') && info.tip.includes('Details: merging PR o/r#1 (abc): 405 Pull Request is not mergeable'), info.tip);
assert.strictEqual(stageTrackerMergeFailedShort(''), 'GitHub refused the merge');
assert.ok(stageTrackerMergeFailedShort('x'.repeat(80)).endsWith('…'));
assert.strictEqual(stageTrackerMergeFailedShort('x'.repeat(80)).length, STAGE_TRACKER_MERGE_FAILED_SHORT_MAX);
console.log('ok');
`)
}
