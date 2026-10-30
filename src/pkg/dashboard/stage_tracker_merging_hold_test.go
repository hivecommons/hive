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
	if mg := stageTrackerMergeGateAttempt(nil, github.MergeAttempt{}, false, "", stagelabels.None, stagelabels.Record{}, false); mg != nil {
		t.Errorf("no attempt, no gate: %+v", mg)
	}
	live := github.MergeAttempt{Repo: "a/b", Number: 1, StartedAt: now, Path: github.PRAuditPathRelay}
	soak := now.Add(time.Minute)
	mg := stageTrackerMergeGateAttempt(&StageTrackerMergeGate{State: MergeGateStateSoaking, SoakUntil: &soak, SoakRemainingSeconds: 60, NextSweepAt: &soak}, live, true, "", stagelabels.PreMergeSoak, stagelabels.Record{}, false)
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
	if mg = stageTrackerMergeGateAttempt(nil, failed, true, "", stagelabels.None, stagelabels.Record{}, false); mg == nil || mg.State != MergeGateStateMergeFailed || mg.MergeFailed != "422 boom" || mg.MergeFailedAt == nil {
		t.Errorf("failed attempt off the gate: %+v", mg)
	}
	for _, c := range []struct{ path, want string }{{github.PRAuditPathDashboard, "dashboard"}, {github.PRAuditPathRelay, "relay"}, {github.PRAuditPathForge, "forge"}, {github.PRAuditPathSweep, "automerge lane"}, {"", "automerge lane"}} {
		if got := stageTrackerMergingCaption(github.MergeAttempt{Path: c.path}); !strings.Contains(got, c.want) {
			t.Errorf("caption for %q = %q, want %q", c.path, got, c.want)
		}
	}
}

// stageTrackerMergingHoldOffJS stands in for the merge-gate "Merging" hold
// (#15245, #15808) in harnesses that watch what happens the moment a
// merge is seen: no hold, no closed list. The hold itself is covered by
// TestStageTrackerMergingHold.
const stageTrackerMergingHoldOffJS = `
var _stageTrackerMergingHold = {}; var _stageTrackerPRItems = {}; var _stageTrackerPRLast = {}; var _stageTrackerClosedNoted = {}; var _stageTrackerMergingHoldMs = 8000;
function stageTrackerMergingHoldMsOf(d, cur) { return cur; }
function stageTrackerPRItemsOf() { return {}; }
function stageTrackerMergingHoldNext() { return {}; }
function stageTrackerMergingHoldUntil() { return 0; }
function stageTrackerMergingHoldSchedule() {}
function stageTrackerNewlyClosed() { return []; }
`

// stageTrackerMergingHoldJS lifts the real hold and closed-reconcile
// functions.
func stageTrackerMergingHoldJS(t *testing.T, html string) string {
	t.Helper()
	return strings.Join([]string{
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_QUEUE"),
		jsConstLine(t, html, "STAGE_TRACKER_MERGING_HOLD_DEFAULT_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_MERGING_HOLD_MIN_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_CLOSED_REASON_MAX"),
		"var _stageTrackerMergingHoldMs = STAGE_TRACKER_MERGING_HOLD_DEFAULT_MS; var _stageTrackerMergingHold = {}; var _stageTrackerPRItems = {}; var _stageTrackerPRLast = {}; var _stageTrackerClosedNoted = {}; var _stageTrackerMergingHoldTimer = 0; var _stageTracker = null;",
		jsFunc(t, html, "stageTrackerMergingHoldMsOf"),
		jsFunc(t, html, "stageTrackerMergingHoldMs"),
		jsFunc(t, html, "stageTrackerPRItemsOf"),
		jsFunc(t, html, "stageTrackerMergingHoldLive"),
		jsFunc(t, html, "stageTrackerMergingHoldNext"),
		jsFunc(t, html, "stageTrackerMergingHoldUntil"),
		jsFunc(t, html, "stageTrackerMergingHoldItem"),
		jsFunc(t, html, "stageTrackerMergingHoldView"),
		jsFunc(t, html, "stageTrackerMergingHoldSchedule"),
		jsFunc(t, html, "stageTrackerMergingHoldRelease"),
		jsFunc(t, html, "stageTrackerNewlyClosed"),
		jsFunc(t, html, "stageTrackerClosedReason"),
		jsFunc(t, html, "stageTrackerClosedToastText"),
		jsFunc(t, html, "stageTrackerClosedToast"),
	}, "\n")
}

// TestStageTrackerMergingHold runs the SPA's merge-gate hold under node
// with an explicit clock (hivecommons/hive#15245, #15808): a row the
// payload dropped mid-merge stays at "Merging" only until the merged fact
// lands — then the merge is fresh at once — or the hold window
// (merged_hold_ms, default 8 s, floor 3 s) passes; a merge whose fact
// arrives with the poll is never masked; a refused merge drops the hold.
func TestStageTrackerMergingHold(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"_stageTrackerMergingHoldMs = stageTrackerMergingHoldMsOf(d, _stageTrackerMergingHoldMs);",
		"_stageTrackerMergingHold = stageTrackerMergingHoldNext(_stageTrackerMergingHold, d, raw, nowMs);",
		"stageTrackerMergingHoldView(_stageTracker, _stageTrackerMergingHold, nowMs)",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	if strings.Contains(html, "STAGE_TRACKER_MERGING_HOLD_MS") || strings.Contains(html, "_stageTrackerMergedHeld") {
		t.Error("the fixed 30 s hold mask is gone: no STAGE_TRACKER_MERGING_HOLD_MS / _stageTrackerMergedHeld")
	}
	runStageTrackerRowActionsScript(t, strings.Join([]string{
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL"),
		jsFunc(t, html, "stageTrackerPRKey"),
		jsFunc(t, html, "stageTrackerMergeState"),
		jsFunc(t, html, "stageTrackerCarryOpen"),
		jsFunc(t, html, "stageTrackerNewlyMerged"),
		stageTrackerMergingHoldJS(t, html),
	}, "\n")+`
const assert = require('assert');
assert.strictEqual(STAGE_TRACKER_MERGING_HOLD_DEFAULT_MS, 8000, 'default hold 8 s');
assert.strictEqual(STAGE_TRACKER_MERGING_HOLD_MIN_MS, 3000, 'floor 3 s');
assert.strictEqual(stageTrackerMergingHoldMsOf({ merged_hold_ms: 12000 }, 8000), 12000);
assert.strictEqual(stageTrackerMergingHoldMsOf({ merged_hold_ms: 1000 }, 8000), 3000, 'clamped to the floor');
assert.strictEqual(stageTrackerMergingHoldMsOf({}, 5000), 5000, 'absent: value in force');
assert.strictEqual(stageTrackerMergingHoldMsOf(null, 0), 8000, 'absent, nothing in force: default');
const HOLD = stageTrackerMergingHoldMs();
const T0 = Date.parse('2026-10-28T21:31:00Z');
const iso = ms => new Date(ms).toISOString();
const pr = (n, stage, mg) => Object.assign({ kind: 'pr', repo: 'o/r', number: n, stage, title: 'feat ' + n }, mg ? { merge_gate: mg } : {});
const K = n => stageTrackerPRKey('o/r', n);

// One poll loop: feed payloads in order with an explicit clock, the way
// stageTrackerNoteMerges does, and return what each poll read as fresh.
function run(polls) {
  let holds = {}, state = null;
  const out = [];
  for (const [nowMs, d] of polls) {
    const raw = stageTrackerCarryOpen(state, stageTrackerMergeState(d));
    holds = stageTrackerMergingHoldNext(holds, d, raw, nowMs);
    const until = stageTrackerMergingHoldUntil(holds, nowMs);
    const fresh = stageTrackerNewlyMerged(state, raw);
    state = raw;
    out.push({ fresh, until, holds, view: stageTrackerMergingHoldView(d, holds, nowMs) });
  }
  return out;
}

// 1. The sweep's merge call is seen in flight (merging_since) at T0; the
//    next poll drops the PR but the merged fact has not landed yet: the
//    board keeps a stage-100 "Merging" row for #1, the gate waiting for
//    GitHub. The poll that brings the merged fact reads it fresh AT ONCE
//    (no 30 s wait) and the synthesised row is gone.
let r = run([
  [T0 - 30000, { items: [pr(1, 60), pr(2, 50)] }],
  [T0 + 1000, { items: [pr(1, 100, { state: 'merging', merging_since: iso(T0), merge_path: 'sweep' }), pr(2, 50)] }],
  [T0 + 3000, { items: [pr(2, 50)] }],
  [T0 + 5000, { items: [pr(2, 50)], recently_merged: [{ repo: 'o/r', number: 1 }] }],
]);
assert.deepStrictEqual(r[1].fresh, [], 'in flight: nothing fresh');
assert.strictEqual(r[1].holds[K(1)].since, T0, 'hold opens from merging_since');
assert.strictEqual(r[1].holds[K(1)].path, 'sweep');
assert.strictEqual(r[1].view.items.length, 2, 'payload lists the PR: no synthesised row');
assert.deepStrictEqual(r[2].fresh, [], 'dropped without the merged fact: nothing fresh yet');
assert.strictEqual(r[2].until, T0 + HOLD, 'release scheduled for the hold end');
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
assert.ok(held.caption.includes('waiting for GitHub'), held.caption);
assert.deepStrictEqual(r[3].fresh, [K(1)], 'the merged fact makes it fresh at once, 5 s into an 8 s window');
assert.deepStrictEqual(r[3].holds, {}, 'hold dropped with the merged fact');
assert.strictEqual(r[3].until, 0);
assert.strictEqual(r[3].view.items.length, 1, 'no synthesised row once nothing is held');

// 2. A merge never seen in flight (060 at one poll, merged at the next):
//    fresh on that poll — no hold, no mask (the fixed "Merging" dwell is
//    gone, #15808).
r = run([
  [T0, { items: [pr(3, 60)] }],
  [T0 + 30000, { items: [], recently_merged: [{ repo: 'o/r', number: 3 }] }],
]);
assert.deepStrictEqual(r[1].fresh, [K(3)], 'surprise merge: fresh at once');
assert.deepStrictEqual(r[1].holds, {});
assert.strictEqual(r[1].view.items.length, 0);

// 3. The merged fact arrives with the very poll that drops the PR
//    (webhook-style patch, the common case now): fresh at once.
r = run([
  [T0, { items: [pr(4, 100, { state: 'merging', merging_since: iso(T0) })] }],
  [T0 + 2000, { items: [], recently_merged: [{ repo: 'o/r', number: 4 }] }],
]);
assert.deepStrictEqual(r[1].fresh, [K(4)]);
assert.deepStrictEqual(r[1].holds, {});

// 4. The merged fact never arrives: the hold expires at its window and
//    the row leaves; nothing is read as fresh (a later recently_merged
//    entry still settles it through stageTrackerCarryOpen).
r = run([
  [T0, { items: [pr(5, 100, { state: 'merging', merging_since: iso(T0) })] }],
  [T0 + 2000, { items: [] }],
  [T0 + HOLD - 1, { items: [] }],
  [T0 + HOLD, { items: [] }],
  [T0 + HOLD + 60000, { items: [], recently_merged: [{ repo: 'o/r', number: 5 }] }],
]);
assert.strictEqual(r[1].view.items.length, 1, 'bridged');
assert.strictEqual(r[2].view.items.length, 1, 'still bridged one ms before the end');
assert.deepStrictEqual(r[3].holds, {}, 'window passed: hold dropped');
assert.strictEqual(r[3].view.items.length, 0);
assert.deepStrictEqual(r[3].fresh, []);
assert.deepStrictEqual(r[4].fresh, [K(5)], 'the late fact still settles it');

// 5. A refused merge drops the hold at once; the row is the payload's
//    (merge-failed at the gate), not a synthesised one.
r = run([
  [T0, { items: [pr(6, 100, { state: 'merging', merging_since: iso(T0) })] }],
  [T0 + 2000, { items: [pr(6, 90, { state: 'merge-failed', merge_failed: '405 not mergeable' })] }],
]);
assert.ok(r[0].holds[K(6)]);
assert.deepStrictEqual(r[1].holds, {}, 'merge-failed drops the hold');
assert.strictEqual(r[1].view.items[0].merge_gate.state, 'merge-failed');
// …as does a PR that is back open at another gate state without merging.
r = run([
  [T0, { items: [pr(7, 100, { state: 'merging', merging_since: iso(T0) })] }],
  [T0 + 2000, { items: [pr(7, 95, { state: 'soaking' })] }],
]);
assert.deepStrictEqual(r[1].holds, {}, 'back to soaking: no hold');

// 6. merging_since missing: the hold runs from first sight and is kept
//    across polls; a non-PR item never holds.
r = run([
  [T0, { items: [pr(8, 100, { state: 'merging' }), { kind: 'issue', repo: 'o/r', number: 9, stage: 100, merge_gate: { state: 'merging' } }] }],
  [T0 + 1000, { items: [pr(8, 100, { state: 'merging' })] }],
]);
assert.strictEqual(r[0].holds[K(8)].since, T0);
assert.strictEqual(r[1].holds[K(8)].since, T0, 'since kept from the first sight');
assert.strictEqual(Object.keys(r[0].holds).length, 1, 'issues do not hold');

// 7. The knob: a payload's merged_hold_ms stretches the bridge.
_stageTrackerMergingHoldMs = stageTrackerMergingHoldMsOf({ merged_hold_ms: 20000 }, _stageTrackerMergingHoldMs);
r = run([
  [T0, { items: [pr(10, 100, { state: 'merging', merging_since: iso(T0) })] }],
  [T0 + 15000, { items: [] }],
]);
assert.strictEqual(r[1].view.items.length, 1, 'still bridged at 15 s under a 20 s hold');
_stageTrackerMergingHoldMs = STAGE_TRACKER_MERGING_HOLD_DEFAULT_MS;

// ---- Closed, not merged (#15808) ----
// The tab saw #11 and #12 open; the next payload drops #11 and lists it
// in recently_closed: one key, once. #12 (still open) and #13 (never
// seen) are not reported; a merged PR in both lists reads as merged.
let state = stageTrackerCarryOpen(null, stageTrackerMergeState({ items: [pr(11, 50, {}), pr(12, 50)] }));
let d = { items: [pr(12, 50)], recently_closed: [{ repo: 'o/r', number: 11 }, { repo: 'o/r', number: 12 }, { repo: 'o/r', number: 13 }] };
let raw = stageTrackerCarryOpen(state, stageTrackerMergeState(d));
const noted = {};
let closed = stageTrackerNewlyClosed(d, state, raw, stageTrackerPRItemsOf(d), noted);
assert.deepStrictEqual(closed, [K(11)]);
closed.forEach(k => { noted[k] = true; });
assert.deepStrictEqual(stageTrackerNewlyClosed(d, state, raw, stageTrackerPRItemsOf(d), noted), [], 'noted once');
d = { items: [], recently_closed: [{ repo: 'o/r', number: 12 }], recently_merged: [{ repo: 'o/r', number: 12 }] };
raw = stageTrackerCarryOpen(state, stageTrackerMergeState(d));
assert.deepStrictEqual(stageTrackerNewlyClosed(d, state, raw, {}, noted), [], 'merged outranks closed');
assert.deepStrictEqual(stageTrackerNewlyClosed({ recently_closed: [{ repo: 'o/r', number: 11 }] }, null, {}, {}, {}), [], 'first poll: nothing to compare');
// The reason comes from the row's last item.
assert.strictEqual(stageTrackerClosedReason({ caption: 'Superseded by #15783 — hive carries the fix there', superseded_by: 15783 }), 'superseded by #15783');
assert.strictEqual(stageTrackerClosedReason({ caption: 'closed as superseded by #15783 after adoption' }), 'superseded by #15783');
assert.strictEqual(stageTrackerClosedReason({ caption: 'Duplicate of #40; held out of the queue' }), 'duplicate of #40');
assert.strictEqual(stageTrackerClosedReason({ caption: 'Awaiting review; reviewer verdict pending' }), 'Awaiting review');
assert.strictEqual(stageTrackerClosedReason({ caption: 'x'.repeat(100) }).length, STAGE_TRACKER_CLOSED_REASON_MAX);
assert.strictEqual(stageTrackerClosedReason(null), 'closed without merging');
assert.strictEqual(stageTrackerClosedReason({ caption: '' }), 'closed without merging');
assert.strictEqual(stageTrackerClosedToastText(K(15793), { superseded_by: 15783 }), '#15793 closed · superseded by #15783');
const toasts = [];
global.showToast = (msg, type) => { toasts.push([type, msg]); return {}; };
stageTrackerClosedToast(K(15793), { caption: 'superseded by #15783' });
assert.deepStrictEqual(toasts, [['info', '#15793 closed · superseded by #15783']], 'neutral info toast, standard countdown');
console.log('ok');
`)
}

// TestStageTrackerMergeGateInfoMerging: the gate pill's words for a live
// merge call (and a slow one), a held row and a refused merge
// (hivecommons/hive#15245, #15808).
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
let info = stageTrackerMergeGateInfoFor(item({ state: 'merging', merging_since: new Date(now - 4000).toISOString(), merge_path: 'sweep' }), null, now);
assert.strictEqual(info.word, 'Merging');
assert.strictEqual(info.detail, 'merge call in flight');
assert.strictEqual(info.cue, 'merging');
assert.ok(info.tip.includes('automerge sweep') && info.tip.includes('in flight'), info.tip);
info = stageTrackerMergeGateInfoFor(item({ state: 'merging', merging_since: new Date(now - 4000).toISOString(), merge_path: 'dashboard', held: true }), null, now);
assert.strictEqual(info.detail, 'waiting for GitHub', 'a held row waits for the merged fact (#15808)');
assert.ok(info.tip.includes('maintainer merge from the dashboard') && info.tip.includes('✓'), info.tip);
// A merge call in flight longer than STAGE_TRACKER_MERGING_SLOW_MS says so
// rather than spinning without a reason (#15808).
info = stageTrackerMergeGateInfoFor(item({ state: 'merging', merging_since: new Date(now - STAGE_TRACKER_MERGING_SLOW_MS - 1000).toISOString(), merge_path: 'sweep' }), null, now);
assert.strictEqual(info.detail, 'merging · waiting for GitHub…');
assert.ok(info.tip.includes('longer than usual'), info.tip);
assert.strictEqual(info.cue, 'merging');
info = stageTrackerMergeGateInfoFor(item({ state: 'merging' }), null, now);
assert.strictEqual(info.detail, '', 'legacy 100 record: word alone');
info = stageTrackerMergeGateInfoFor(Object.assign(item({ state: 'merge-failed', merge_failed: 'merging PR o/r#1 (abc): 405 Pull Request is not mergeable\nmore', merge_failed_at: new Date(now - 9000).toISOString(), merge_path: 'relay' }), { stage: 90 }), null, now);
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

// TestStageTrackerRemovedRowReconcileNode (hivecommons/hive#15808): a row
// that leaves the payload is reconciled on removal with the real hold and
// closed-list functions in the celebration harness — gone with the merged
// fact (here only the durable ledger's recent list carries it) → exactly
// one celebration (banner, bee, count on landing), never a second on the
// next poll; gone and listed in recently_closed → one neutral info toast
// with the reason from the row's last caption, no bee, no banner, no
// count.
func TestStageTrackerRemovedRowReconcileNode(t *testing.T) {
	html := indexHTML(t)
	js := strings.Replace(stageTrackerCelebrateJS(t, html), stageTrackerMergingHoldOffJS, stageTrackerMergingHoldJS(t, html), 1)
	runStageTrackerRowActionsScript(t, js+stageTrackerCelebrateFixture+`
const iso = ms => new Date(ms).toISOString();
const rec = (...nums) => nums.map(x => ({ repo: 'o/r', number: x, title: 'fix ' + x, merged_at: iso(clock), merged_by: 'hive', lane: 'hive' }));
const srv = (all, today, recent) => ({ all_time: all, today, total_7d: today, total_30d: today, recent, tz: 'UTC', day_start: iso(stageTrackerLocalMidnight(clock)) });
const pr = (num, stage, extra) => Object.assign({ repo: 'o/r', kind: 'pr', number: num, stage, title: 'fix ' + num }, extra || {});
const poll = d => { _stageTracker = d; stageTrackerNoteMerges(d); };
panel.appendChild(row('o/r/pr#5')); panel.appendChild(row('o/r/pr#6'));
poll({ items: [pr(5, 50), pr(6, 50, { caption: 'Superseded by #7 — hive carries the fix there', superseded_by: 7 })], merged: srv(10, 1, rec(3)) });
assert.strictEqual(odoText(n), '10');
// #5 vanishes; the SPA never saw merging or a terminal stage, only the
// ledger's recent list says merged → celebrate once.
tick(10000);
poll({ items: [pr(6, 50, { caption: 'Superseded by #7 — hive carries the fix there', superseded_by: 7 })], merged: srv(11, 2, rec(5, 3)) });
tick(0);
assert.strictEqual(banners.children.length, 1, 'one banner'); assert.ok(banners.children[0].innerHTML.includes('#5'));
assert.strictEqual(flights().length, 1, 'one bee');
assert.strictEqual(odoText(n), '10', 'count waits for the landing');
assert.deepStrictEqual(toasts, [], 'a merge is not a closed toast');
flights()[0]._ev.animationend();
assert.strictEqual(odoText(n), '11');
tick(10000);
poll({ items: [pr(6, 50, { caption: 'Superseded by #7 — hive carries the fix there', superseded_by: 7 })], merged: srv(11, 2, rec(5, 3)) });
tick(0);
assert.strictEqual(banners.children.length, 1, 'never a second celebration'); assert.strictEqual(flights().length, 0);
// #6 vanishes closed, not merged: one info toast naming the reason.
tick(10000);
poll({ items: [], recently_closed: [{ repo: 'o/r', number: 6, closed_at: iso(clock) }], merged: srv(11, 2, rec(5, 3)) });
tick(0);
assert.deepStrictEqual(toasts, [['info', '#6 closed · superseded by #7']]);
assert.strictEqual(banners.children.length, 1, 'no merge banner for a close'); assert.strictEqual(flights().length, 0, 'no bee');
assert.strictEqual(odoText(n), '11', 'no count');
tick(10000);
poll({ items: [], recently_closed: [{ repo: 'o/r', number: 6, closed_at: iso(clock) }], merged: srv(11, 2, rec(5, 3)) });
assert.strictEqual(toasts.length, 1, 'toasted once');
// A PR this tab never saw open and that closes is not news.
poll({ items: [], recently_closed: [{ repo: 'o/r', number: 6 }, { repo: 'o/r', number: 8 }], merged: srv(11, 2, rec(5, 3)) });
assert.strictEqual(toasts.length, 1);
console.log('ok');
`)
}
