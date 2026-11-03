package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// TestStageTrackerMergeGatePayload: the merge_gate narration a PR row
// carries at 060 / 080 / 095 / 100 (hivecommons/hive#15073) — state, the
// gate's reason, the lane's last look, the next floor pass, whether a
// sweep is on it now, and the soak countdown — and nothing off the gate.
func TestStageTrackerMergeGatePayload(t *testing.T) {
	now := time.Date(2026, 10, 28, 19, 23, 0, 0, time.UTC)
	seen := now.Add(-2 * time.Minute)
	next := now.Add(3 * time.Minute)
	idle := automerge.SweepState{NextFloorAt: next, FinishedAt: seen}
	rec := func(stage stagelabels.Stage, reason string) stagelabels.Record {
		return stagelabels.Record{Stage: stage, Reason: reason, LastSeen: seen, EnteredAt: seen}
	}

	// 060 reviewed, CI pending (hivecommons/hive#15054 at 15:11 ET).
	mg := stageTrackerMergeGate("hivecommons/hive", 15054, stagelabels.Reviewed, rec(stagelabels.Reviewed, "required-check-pending:build-and-test"), true, idle, now)
	if mg == nil || mg.State != MergeGateStatePending || mg.Reason != "required-check-pending:build-and-test" {
		t.Fatalf("060: %+v", mg)
	}
	if mg.SweepRunning || mg.LastSweepAt == nil || !mg.LastSweepAt.Equal(seen) || mg.NextSweepAt == nil || !mg.NextSweepAt.Equal(next) {
		t.Errorf("060 timing: %+v", mg)
	}
	if mg.SoakUntil != nil || mg.SoakRemainingSeconds != 0 {
		t.Errorf("060 carries no soak: %+v", mg)
	}

	// The same PR while a pass that names it is in flight
	// (hivecommons/hive#15042): sweep_running, no next pass.
	running := automerge.SweepState{Running: true, Cause: "event", StartedAt: now, NextFloorAt: next, Targets: automerge.SweepTargets{"hivecommons/hive": {15042: true}}}
	mg = stageTrackerMergeGate("hivecommons/hive", 15042, stagelabels.Reviewed, rec(stagelabels.Reviewed, "check-pending"), true, running, now)
	if mg == nil || !mg.SweepRunning || mg.NextSweepAt != nil {
		t.Errorf("060 mid-sweep: %+v", mg)
	}
	// A targeted pass on another PR is not on this one.
	mg = stageTrackerMergeGate("hivecommons/hive", 15054, stagelabels.Reviewed, rec(stagelabels.Reviewed, "check-pending"), true, running, now)
	if mg == nil || mg.SweepRunning || mg.NextSweepAt == nil {
		t.Errorf("060 beside a targeted pass: %+v", mg)
	}
	// A full pass (nil targets) is on every PR.
	full := automerge.SweepState{Running: true, Cause: "floor"}
	if mg = stageTrackerMergeGate("hivecommons/hive", 15054, stagelabels.AwaitingMergeAdmission, rec(stagelabels.AwaitingMergeAdmission, ""), true, full, now); mg == nil || !mg.SweepRunning {
		t.Errorf("080 during a full pass: %+v", mg)
	}

	// 080 ready: waiting for the next pass.
	mg = stageTrackerMergeGate("hivecommons/hive", 1, stagelabels.AwaitingMergeAdmission, rec(stagelabels.AwaitingMergeAdmission, ""), true, idle, now)
	if mg == nil || mg.State != MergeGateStateReady || mg.NextSweepAt == nil {
		t.Errorf("080: %+v", mg)
	}

	// 095 soaking (hivecommons/hive#15044): the reason's remaining aged by
	// the time since the lane last looked.
	mg = stageTrackerMergeGate("hivecommons/hive", 15044, stagelabels.PreMergeSoak, rec(stagelabels.PreMergeSoak, "contributor-lane:soaking(remaining=10m0s)"), true, idle, now)
	if mg == nil || mg.State != MergeGateStateSoaking || mg.SoakRemainingSeconds != 8*60 || mg.SoakUntil == nil || !mg.SoakUntil.Equal(now.Add(8*time.Minute)) {
		t.Errorf("095: %+v", mg)
	}

	// 100 merging.
	if mg = stageTrackerMergeGate("hivecommons/hive", 2, stagelabels.MergeQueue, rec(stagelabels.MergeQueue, ""), true, idle, now); mg == nil || mg.State != MergeGateStateMerging {
		t.Errorf("100: %+v", mg)
	}

	// Off the gate: nothing. A past floor time is not a next pass.
	for _, st := range []stagelabels.Stage{stagelabels.None, stagelabels.ReviewPending, stagelabels.Fixing, stagelabels.NeedsDecision, stagelabels.ReadyForHumanMerge} {
		if mg = stageTrackerMergeGate("hivecommons/hive", 3, st, rec(st, "x"), true, idle, now); mg != nil {
			t.Errorf("%v must carry no merge_gate: %+v", st, mg)
		}
	}
	stale := automerge.SweepState{NextFloorAt: now.Add(-time.Second)}
	if mg = stageTrackerMergeGate("hivecommons/hive", 4, stagelabels.AwaitingMergeAdmission, stagelabels.Record{}, false, stale, now); mg == nil || mg.NextSweepAt != nil || mg.LastSweepAt != nil {
		t.Errorf("no record, stale floor: %+v", mg)
	}
}

// TestStageTrackerMergeGateInPayload: buildStageTracker stamps merge_gate
// on the PR item and mirrors it (with soak_remaining_seconds) onto the
// issue's nested linked_prs, reading the sweep state through the Server's
// seam.
func TestStageTrackerMergeGateInPayload(t *testing.T) {
	now := time.Date(2026, 10, 28, 19, 23, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	s.sweepState = func() automerge.SweepState { return automerge.SweepState{NextFloorAt: now.Add(90 * time.Second)} }
	resp := s.buildStageTracker(s.status, store, now)
	// PR 102: soaking, 12m left as of a sweep 5m ago → 7m.
	pr := trackerItem(t, resp, "pr", 102)
	if pr.MergeGate == nil || pr.MergeGate.State != MergeGateStateSoaking || pr.MergeGate.SoakRemainingSeconds != 7*60 || pr.MergeGate.NextSweepAt == nil || !pr.MergeGate.NextSweepAt.Equal(now.Add(90*time.Second)) || pr.MergeGate.SweepRunning {
		t.Fatalf("PR 102 merge_gate: %+v", pr.MergeGate)
	}
	// Off the gate: no merge_gate.
	if off := trackerItem(t, resp, "pr", 103); off.MergeGate != nil {
		t.Errorf("PR 103 (050) must carry no merge_gate: %+v", off.MergeGate)
	}
	if issue := trackerItem(t, resp, "issue", 8); issue.MergeGate != nil {
		t.Errorf("issue rows carry no merge_gate: %+v", issue.MergeGate)
	}
	// Issue 7 mirrors PR 102: the nested copy carries the gate and the soak.
	issue := trackerItem(t, resp, "issue", 7)
	var lp *StageTrackerLinkedPR
	for i := range issue.LinkedPRs {
		if issue.LinkedPRs[i].Number == 102 {
			lp = &issue.LinkedPRs[i]
		}
	}
	if lp == nil || lp.MergeGate == nil || lp.MergeGate.State != MergeGateStateSoaking || lp.SoakRemainingSeconds != 7*60 {
		t.Fatalf("nested PR 102 must mirror merge_gate + soak_remaining_seconds: %+v", issue.LinkedPRs)
	}
	raw, err := json.Marshal(pr)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"merge_gate":{`, `"state":"soaking"`, `"soak_remaining_seconds":420`, `"next_sweep_at":`, `"soak_until":`, `"last_sweep_at":`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("PR 102 json missing %s: %s", want, raw)
		}
	}
	// A pass naming the PR: sweep_running, next_sweep_at withheld.
	s.sweepState = func() automerge.SweepState {
		return automerge.SweepState{Running: true, Cause: "event", StartedAt: now, NextFloorAt: now.Add(90 * time.Second), Targets: automerge.SweepTargets{"acme/console": {102: true}}}
	}
	pr = trackerItem(t, s.buildStageTracker(s.status, store, now), "pr", 102)
	if pr.MergeGate == nil || !pr.MergeGate.SweepRunning || pr.MergeGate.NextSweepAt != nil {
		t.Fatalf("PR 102 mid-sweep: %+v", pr.MergeGate)
	}
}

// TestStageTrackerMergeGateNode: the SPA's pure helpers over the two rows
// the operator compared (hivecommons/hive#15042 mid-sweep, #15054 waiting)
// and the soaking sub-row (#15044): every row on the gate gets the leading
// glyph with the same sentence as title and aria-label, the pill word is
// the stage's own with its live detail, countdowns tick in place, and the
// agent-working spinner carries its own sentence.
func TestStageTrackerMergeGateNode(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`const STAGE_TRACKER_MERGE_GATE_WORD = { 'gate-pending': 'Review passed', ready: 'Ready for auto merge', soaking: 'Soaking', merging: 'Merging', merged: 'Merged', 'merge-failed': 'Merge failed' };`,
		`60: 'Review passed', 80: 'Ready for auto merge', 95: 'Soaking', 100: 'Merging'`,
		`const gateCue = gateWord && typeof stageTrackerGateCueHtml === 'function' ? stageTrackerGateCueHtml(item, nowMs, trains) : '';`,
		`else if (active) inner = gateCue + workIco + (decision ? stageTrackerDecisionTriggerHtml(decision, word, tip) : word) + (actorIco || holderIco);`,
		`if (gateInfo) tip = sg.label + ' — ' + gateInfo.tip;`,
		`if (typeof stageTrackerGateCountdownSync === 'function') stageTrackerGateCountdownSync(el, nowMs);`,
		`.ham-gate-ico { display: inline-flex;`, `.ham-gate-detail {`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing merge-gate wiring %q", want)
		}
	}
	// The legend lives on the help page the (?) mark links to.
	docs, err := os.ReadFile(filepath.Join("..", "..", "docs", "dashboard-sections", "activity-monitor.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"**Merge queue pills — legend**", "Awaiting merge gate", "Ready for auto merge", "**Soaking**", "**Merging**", "next pass in"} {
		if !strings.Contains(string(docs), want) {
			t.Errorf("docs/dashboard-sections/activity-monitor.md missing merge-gate legend %q", want)
		}
	}
	runStageTrackerRowActionsScript(t, "function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/\"/g, '&quot;'); }\n"+
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_PRE_MERGE_SOAK")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_ACTIVE_WORD")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_GATE_WORD")+"\n"+
		jsConstBlock(t, html, "STAGE_TRACKER_GATE_REASON_WORDS")+"\n"+
		jsConstBlock(t, html, "STAGE_TRACKER_ICONS")+"\n"+
		jsFunc(t, html, "stageTrackerIcon")+"\n"+
		jsFunc(t, html, "stageTrackerAgoText")+"\n"+
		jsFunc(t, html, "stageTrackerSoakLeft")+"\n"+
		jsFunc(t, html, "stageTrackerMergeGateStateOf")+"\n"+
		jsFunc(t, html, "stageTrackerShortLeft")+"\n"+
		jsFunc(t, html, "stageTrackerGateReasonText")+"\n"+
		stageTrackerMergeGateInfoJS(t, html)+"\n"+
		jsFunc(t, html, "stageTrackerMergeGateWordHtml")+"\n"+
		jsFunc(t, html, "stageTrackerGateCueHtml")+"\n"+
		jsFunc(t, html, "stageTrackerGateCountdownSync")+"\n"+`
const assert = require('assert');
const now = Date.parse('2026-10-28T19:11:00Z');
const iso = ms => new Date(ms).toISOString();
// Vocabulary pinned once: the column words and the gate words agree.
for (const [n, k] of [[60, 'gate-pending'], [80, 'ready'], [95, 'soaking'], [100, 'merging']]) assert.strictEqual(STAGE_TRACKER_ACTIVE_WORD[n], STAGE_TRACKER_MERGE_GATE_WORD[k], 'stage ' + n);

// Captured 15:11 ET: both rows at 060 reviewed, CI pending. #15042 had a
// targeted sweep pass on it; #15054 was waiting for the next pass.
const pr15042 = { repo: 'hivecommons/hive', number: 15042, kind: 'pr', base_ref: 'v6', author_kind: 'hive', stage: 60, stage_slug: 'reviewed', reason: 'check-pending',
  caption: 'Review swarm approved; a gate is still pending — CI running',
  merge_gate: { state: 'gate-pending', reason: 'check-pending', sweep_running: true, last_sweep_at: iso(now - 30000) } };
const pr15054 = { repo: 'hivecommons/hive', number: 15054, kind: 'pr', base_ref: 'v5', author_kind: 'clanker', stage: 60, stage_slug: 'reviewed', reason: 'required-check-pending:build-and-test',
  caption: 'Review swarm approved; a gate is still pending — check build-and-test still running',
  merge_gate: { state: 'gate-pending', reason: 'required-check-pending:build-and-test', last_sweep_at: iso(now - 120000), next_sweep_at: iso(now + 42000) } };
const pr15044 = { repo: 'hivecommons/hive', number: 15044, kind: 'pr', base_ref: 'v5', author_kind: 'clanker', stage: 95, stage_slug: 'pre-merge-soak',
  merge_gate: { state: 'soaking', reason: 'contributor-lane:soaking(remaining=10m0s)', last_sweep_at: iso(now - 60000), next_sweep_at: iso(now + 200000), soak_until: iso(now + 452000), soak_remaining_seconds: 452 } };
const ready = { repo: 'hivecommons/hive', number: 15060, kind: 'pr', stage: 80, merge_gate: { state: 'ready', last_sweep_at: iso(now - 10000), next_sweep_at: iso(now + 42000) } };
const merging = { repo: 'hivecommons/hive', number: 15061, kind: 'pr', stage: 100, merge_gate: { state: 'merging', last_sweep_at: iso(now - 1000) } };

// Both 060 rows read the same word — neither is "Ready for auto merge" with CI pending.
let a = stageTrackerMergeGateInfo(pr15042, now), b = stageTrackerMergeGateInfo(pr15054, now);
assert.strictEqual(a.word, 'Review passed'); assert.strictEqual(b.word, 'Review passed');
assert.strictEqual(a.cue, 'running'); assert.strictEqual(a.detail, 'CI in progress');
assert.strictEqual(a.tip, "Review passed, not yet admitted to auto merge — CI in progress; hive is checking this PR now.");
// The pill uses the operator's wording; the tooltip keeps the lane's full reason.
assert.strictEqual(b.cue, 'waiting'); assert.strictEqual(b.detail, 'CI in progress');
assert.ok(b.tip.startsWith('Review passed, not yet admitted to auto merge — CI running (build-and-test). hive re-checks the gate on its next pass in 42s.'), b.tip);
assert.ok(b.tip.includes('A GitHub event on the PR (check run, review, push) triggers a pass sooner.') && b.tip.endsWith('Last checked 2m ago.'), b.tip);

// Every row on the gate gets the glyph; the sentence is its title AND aria-label.
for (const [it, cue, spin] of [[pr15042, 'running', true], [pr15054, 'waiting', false], [pr15044, 'soaking', false], [ready, 'waiting', false], [merging, 'merging', true]]) {
  const g = stageTrackerGateCueHtml(it, now);
  const tip = stageTrackerMergeGateInfo(it, now).tip;
  assert.ok(g.startsWith('<span class="ham-gate-ico' + (spin ? ' cue-spin' : '') + '" data-gate-cue="' + cue + '" role="img" title="' + esc(tip) + '" aria-label="' + esc(tip) + '">'), '#' + it.number + ': ' + g);
  assert.ok(g.includes(spin ? 'class="ham-ico" viewBox' : 'class="ham-ico" viewBox') && g.includes(spin ? STAGE_TRACKER_ICONS.sweep : STAGE_TRACKER_ICONS.wait), '#' + it.number + ' glyph body');
}
assert.strictEqual(stageTrackerGateCueHtml({ repo: 'r', number: 1, kind: 'pr', stage: 50 }, now), '', 'off the gate: no glyph');
assert.strictEqual(stageTrackerGateCueHtml({ repo: 'r', number: 1, kind: 'pr', stage: STAGE_TRACKER_TERMINAL }, now), '', 'merged: no glyph');

// The pill word with the live detail; the waiting row says what it waits for.
assert.strictEqual(stageTrackerMergeGateWordHtml(ready, now), '<span class="ham-seg-t">Ready for auto merge <span class="ham-gate-detail" data-ham-until="' + (now + 42000) + '" data-ham-until-kind="ready">· next pass in 42s</span></span>');
// The soak detail carries its clock (data-ham-soak…) so the tick redoes the words and the hover.
const soakHtml = stageTrackerMergeGateWordHtml(pr15044, now);
assert.ok(soakHtml.startsWith('<span class="ham-seg-t">Soaking <span class="ham-gate-detail" data-ham-soak="1" data-ham-until="' + (now + 452000) + '" data-ham-until-kind="soaking"'), soakHtml);
assert.ok(soakHtml.endsWith('>· 8m left</span></span>'), soakHtml);
assert.strictEqual(stageTrackerMergeGateWordHtml(pr15042, now), '<span class="ham-seg-t">Review passed <span class="ham-gate-detail">· CI in progress</span></span>');
assert.strictEqual(stageTrackerMergeGateWordHtml(merging, now), '<span class="ham-seg-t">Merging</span>');
assert.strictEqual(stageTrackerMergeGateWordHtml({ stage: 50 }, now), '');
const soak = stageTrackerMergeGateInfo(pr15044, now);
assert.strictEqual(soak.tip, 'The head must sit the soak window without new commits before the hive merges it · 7m 32s left · soak ends ' + stageTrackerClockText(now + 452000) + ' Last checked 1m ago.');
assert.strictEqual(stageTrackerMergeGateInfo(ready, now).tip, "Approved and green; waiting for hive's next merge pass — next pass in 42s. A GitHub event on the PR (check run, review, push) triggers a pass sooner. Last checked 10s ago.");
assert.strictEqual(stageTrackerMergeGateInfo(merging, now).tip, 'Admitted and soaked; hive is merging it now (GitHub merge queue / merge window). Last checked 1s ago.');

// Without merge_gate (older server) the stage alone picks the word and a plain sentence.
const old = stageTrackerMergeGateInfo({ stage: 80 }, now);
assert.deepStrictEqual([old.word, old.detail, old.cue], ['Ready for auto merge', '', 'waiting']);
assert.strictEqual(old.tip, "Approved and green; waiting for hive's next merge pass. A GitHub event on the PR (check run, review, push) triggers a pass sooner.");
assert.strictEqual(stageTrackerMergeGateInfo({ stage: 95, soak_remaining_seconds: 90 }, now).detail, '2m left', 'legacy soak_remaining_seconds still counts');
assert.strictEqual(stageTrackerSoakLeft({ merge_gate: { soak_remaining_seconds: 452 } }), ' — 8m left', 'the 095 chevron tooltip reads the mirrored field too');

// Reason words.
assert.strictEqual(stageTrackerGateReasonText('check-pending'), 'CI running');
assert.strictEqual(stageTrackerGateReasonText('required-check-failing:lint'), 'CI failed: lint');
assert.strictEqual(stageTrackerGateReasonText('conflicting'), 'branch has conflicts');
assert.strictEqual(stageTrackerGateReasonText('something-else'), 'something-else', 'unknown tokens pass through');
assert.strictEqual(stageTrackerGateReasonText(''), '');
assert.deepStrictEqual([stageTrackerShortLeft(42000), stageTrackerShortLeft(170000), stageTrackerShortLeft(3900000), stageTrackerShortLeft(0)], ['42s', '3m', '1h05m', '']);

// Countdowns tick in place and flip to the "due" words once past.
const mk = (until, kind) => ({ attrs: { 'data-ham-until': String(until), 'data-ham-until-kind': kind }, textContent: '', getAttribute(n) { return this.attrs[n] === undefined ? null : this.attrs[n]; } });
const n1 = mk(now + 42000, 'ready'), n2 = mk(now + 452000, 'soaking');
const el = { querySelectorAll: () => [n1, n2] };
stageTrackerGateCountdownSync(el, now + 30000);
assert.deepStrictEqual([n1.textContent, n2.textContent], ['· next pass in 12s', '· 8m left']);
stageTrackerGateCountdownSync(el, now + 500000);
assert.deepStrictEqual([n1.textContent, n2.textContent], ['· pass due now', '· complete']);

// hivecommons/hive#15103: the fixer's rebase loop guard is the pill while
// the hive rebases a conflicting PR, spinning on attempt n/2 and asking
// for a person once exhausted; a live rebase-then-merge follow-through
// keeps its own caption instead.
const rebasing = { stage: 90, number: 15087, rebase: { attempt: 1, max: 2, exhausted: false, head: 'aaa1111', text: 'Rebasing · attempt 1/2' } };
const rbi = stageTrackerMergeGateInfo(rebasing, now);
assert.deepStrictEqual([rbi.state, rbi.word, rbi.detail, rbi.cue], ['rebasing', 'Rebasing · attempt 1/2', '', 'running']);
assert.ok(rbi.tip.includes('attempt 1 of 2'), rbi.tip);
assert.ok(stageTrackerGateCueHtml(rebasing, now).includes('cue-spin'), 'rebasing spins');
const exhausted = { stage: 90, rebase: { attempt: 2, max: 2, exhausted: true, head: 'ccc3333', text: 'Rebase failed · 2/2 — needs a person' } };
const exi = stageTrackerMergeGateInfo(exhausted, now);
assert.deepStrictEqual([exi.state, exi.word, exi.cue], ['rebase-failed', 'Rebase failed · 2/2 — needs a person', 'waiting']);
assert.ok(exi.tip.includes('2 times') && exi.tip.includes('ccc3333') && exi.tip.includes('Adopt into fresh branch'), exi.tip);
assert.ok(stageTrackerMergeGateWordHtml(exhausted, now).includes('Rebase failed · 2/2 — needs a person'));
const withFollowThrough = { stage: 90, rebase: rebasing.rebase, rebase_merge: { state: 'rebasing' } };
assert.strictEqual(stageTrackerMergeGateInfo(withFollowThrough, now), null, 'a live follow-through owns the row caption');
assert.strictEqual(stageTrackerMergeGateInfo({ stage: 90, rebase: rebasing.rebase, rebase_merge: { state: 'failed' } }, now).state, 'rebasing', 'a settled follow-through hands the pill back');
console.log('merge-gate helpers ok');
`)
}

// stageTrackerMergeGateInfoJS lifts stageTrackerMergeGateInfo with the
// helpers it reads: the issue row's cover reason parser and the nested PR
// it mirrors (hivecommons/hive#15101).
func stageTrackerMergeGateInfoJS(t *testing.T, html string) string {
	t.Helper()
	return jsConstLine(t, html, "STAGE_TRACKER_COVER_REASON_RE") + "\n" +
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_PATH_WORDS") + "\n" +
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_FAILED_SHORT_MAX") + "\n" +
		jsFunc(t, html, "stageTrackerMergeFailedShort") + "\n" +
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_REFUSAL_DETAIL") + "\n" +
		jsFunc(t, html, "stageTrackerMergeRefusalFromRaw") + "\n" +
		jsFunc(t, html, "stageTrackerMergeFailedInfo") + "\n" +
		jsConstLine(t, html, "STAGE_TRACKER_STAGE_PHRASE") + "\n" +
		jsFunc(t, html, "stageTrackerStagePhrase") + "\n" +
		jsFunc(t, html, "stageTrackerCoverReasonText") + "\n" +
		jsFunc(t, html, "stageTrackerReviewPassedDetail") + "\n" +
		jsFunc(t, html, "stageTrackerCoveringPR") + "\n" +
		jsFunc(t, html, "stageTrackerMergeGateInfo") + "\n" +
		jsFunc(t, html, "stageTrackerMergeGateInfoFor") + "\n" +
		stageTrackerTrainQueueJS(t, html) + "\n" +
		stageTrackerSoakClockJS(t, html)
}

// stageTrackerTrainQueueJS lifts the merge-queue words the gate pill reads
// for a train member (hivecommons/hive#15395).
func stageTrackerTrainQueueJS(t *testing.T, html string) string {
	t.Helper()
	return strings.Join([]string{
		jsConstLine(t, html, "STAGE_TRACKER_TRAIN_STATE_FRONT"),
		jsConstLine(t, html, "STAGE_TRACKER_TRAIN_STATE_QUEUED"),
		jsConstLine(t, html, "STAGE_TRACKER_TRAIN_STATE_REBASING"),
		jsConstLine(t, html, "STAGE_TRACKER_TRAIN_STATE_EXHAUSTED"),
		jsConstLine(t, html, "STAGE_TRACKER_TRAIN_STATE_WORD"),
		jsConstLine(t, html, "STAGE_TRACKER_GATE_DUPLICATE_PREFIX"),
		jsFunc(t, html, "stageTrackerGateDuplicateOf"),
		jsFunc(t, html, "stageTrackerMergeGateStateWord"),
		jsFunc(t, html, "stageTrackerTrain"),
		jsFunc(t, html, "stageTrackerOrdinal"),
		jsFunc(t, html, "stageTrackerShortLeft"),
		jsFunc(t, html, "stageTrackerTrainPlace"),
		stageTrackerTrainProvisionalJS(t, html),
		jsConstLine(t, html, "STAGE_TRACKER_TRAIN_STALLED_LEAD"),
		jsConstLine(t, html, "STAGE_TRACKER_TRAIN_HEAD_STALL_WORD"),
		jsFunc(t, html, "stageTrackerTrainStallWhy"),
		jsFunc(t, html, "stageTrackerTrainStalledWord"),
		jsFunc(t, html, "stageTrackerTrainWord"),
		jsFunc(t, html, "stageTrackerTrainEtaAt"),
		jsFunc(t, html, "stageTrackerTrainEtaWords"),
		jsFunc(t, html, "stageTrackerTrainEtaDetail"),
		jsFunc(t, html, "stageTrackerTrainQueueTip"),
		jsConstLine(t, html, "STAGE_TRACKER_LEFT_TRAIN_NOTE"),
		jsFunc(t, html, "stageTrackerLeftTrainNote"),
		jsFunc(t, html, "stageTrackerTrainLines"),
	}, "\n")
}

// stageTrackerTrainProvisionalJS lifts the provisional-slot words: a train
// member whose own stage is short of the merge gate reads its blocker
// first and its slot as a suffix.
func stageTrackerTrainProvisionalJS(t *testing.T, html string) string {
	t.Helper()
	return strings.Join([]string{
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_READY"),
		jsConstLine(t, html, "STAGE_TRACKER_PROVISIONAL_STAGE_WORD"),
		jsFunc(t, html, "stageTrackerTrainProvisional"),
		jsFunc(t, html, "stageTrackerTrainProvisionalHeadline"),
		jsFunc(t, html, "stageTrackerTrainProvisionalSuffix"),
		jsFunc(t, html, "stageTrackerTrainOrdinalOfN"),
		jsFunc(t, html, "stageTrackerTrainTag"),
		jsConstLine(t, html, "STAGE_TRACKER_TRAIN_TAG_LEAD"),
		jsFunc(t, html, "stageTrackerTrainTagSuffix"),
		jsFunc(t, html, "stageTrackerTrainTagTip"),
		jsFunc(t, html, "stageTrackerTrainIndex"),
		jsConstLine(t, html, "STAGE_TRACKER_TRAIN_ACCENTS"),
		jsFunc(t, html, "stageTrackerTrainAccentClass"),
	}, "\n")
}

// stageTrackerSoakClockJS lifts the soak-clock helpers the gate pill reads.
func stageTrackerSoakClockJS(t *testing.T, html string) string {
	t.Helper()
	return jsFunc(t, html, "stageTrackerSoakClock") + "\n" +
		jsFunc(t, html, "stageTrackerSoakLeftText") + "\n" +
		jsFunc(t, html, "stageTrackerClockText") + "\n" +
		jsFunc(t, html, "stageTrackerNextPassWords") + "\n" +
		jsFunc(t, html, "stageTrackerSoakWords") + "\n" +
		jsFunc(t, html, "stageTrackerSoakTick")
}

// TestStageTrackerMergeGateIssueRowPillNode: the regression #15084 shipped
// (hivecommons/hive#15101). An issue row mirrors its covering PR's stage,
// and its lane reason is "open PR #15090 covers it (PR at 060-reviewed)".
// The gate pill read that lane reason as a gate reason and drew it as the
// pill's detail — a two-line, clipped "Awaiting merge gate · open PR #15090
// covers it (PR at 060-reviewe…". The pill is the word alone (plus a gate
// fact when there is one); the cover reason moves to the tooltip, and the
// issue row reads the gate from the nested PR so both rows agree.
func TestStageTrackerMergeGateIssueRowPillNode(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, "function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/\"/g, '&quot;'); }\n"+
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_PRE_MERGE_SOAK")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_GATE_WORD")+"\n"+
		jsConstBlock(t, html, "STAGE_TRACKER_GATE_REASON_WORDS")+"\n"+
		jsConstBlock(t, html, "STAGE_TRACKER_ICONS")+"\n"+
		jsFunc(t, html, "stageTrackerIcon")+"\n"+
		jsFunc(t, html, "stageTrackerAgoText")+"\n"+
		jsFunc(t, html, "stageTrackerMergeGateStateOf")+"\n"+
		jsFunc(t, html, "stageTrackerShortLeft")+"\n"+
		jsFunc(t, html, "stageTrackerGateReasonText")+"\n"+
		stageTrackerMergeGateInfoJS(t, html)+"\n"+
		jsFunc(t, html, "stageTrackerMergeGateWordHtml")+"\n"+
		jsFunc(t, html, "stageTrackerGateCueHtml")+"\n"+`
const assert = require('assert');
const now = Date.parse('2026-10-28T21:20:00Z');
const iso = ms => new Date(ms).toISOString();
const leak = /covers it|\(PR at/;

// Captured 17:17 ET after roll dd412e4: #15088 → PR #15090, both at 060.
const pr15090 = { repo: 'hivecommons/hive', number: 15090, kind: 'pr', stage: 60, stage_slug: 'reviewed', reason: 'required-check-pending:build-and-test',
  merge_gate: { state: 'gate-pending', reason: 'required-check-pending:build-and-test', last_sweep_at: iso(now - 120000), next_sweep_at: iso(now + 42000) } };
const issue15088 = { repo: 'hivecommons/hive', number: 15088, kind: 'issue', stage: 60, stage_slug: 'reviewed', mirrored: true,
  reason: 'open PR #15090 covers it (PR at 060-reviewed)', caption: 'PR in progress · open PR #15090 covers it (PR at 060-reviewed)',
  linked_prs: [{ number: 15090, stage: 60, merge_gate: pr15090.merge_gate }] };

// The pill is one line: the word plus the PR's gate fact, never the lane reason.
const pill = stageTrackerMergeGateWordHtml(issue15088, now);
assert.strictEqual(pill, '<span class="ham-seg-t">Review passed <span class="ham-gate-detail">· CI in progress</span></span>', pill);
assert.ok(!leak.test(pill), 'pill label must not carry the cover reason: ' + pill);
// The issue row and its PR sub-row agree on word, detail and cue.
const a = stageTrackerMergeGateInfo(issue15088, now), b = stageTrackerMergeGateInfo(pr15090, now);
assert.deepStrictEqual([a.word, a.detail, a.cue, a.state], [b.word, b.detail, b.cue, b.state]);
// The cover reason leads the tooltip — "· at <stage>", not "(PR at <stage>)".
assert.ok(a.tip.startsWith('Open PR #15090 covers it · its PR is review passed · waiting for merge admission — Review passed, not yet admitted to auto merge — CI running (build-and-test).'), a.tip);
assert.ok(!/\(PR at/.test(a.tip), a.tip);
const glyph = stageTrackerGateCueHtml(issue15088, now);
assert.ok(glyph.includes('title="' + esc(a.tip) + '"') && glyph.includes('aria-label="' + esc(a.tip) + '"'), glyph);
assert.ok(!/title="[^"]*covers it/.test(pill), 'the pill itself has no title');
// The PR row's tooltip does not grow the cover lead.
assert.ok(b.tip.startsWith('Review passed, not yet admitted'), b.tip);

// Older payload: the nested PR carries no merge_gate. The stage alone picks
// the word and the lane reason still stays out of the pill.
const bare = Object.assign({}, issue15088, { linked_prs: [{ number: 15090, stage: 60 }] });
const bareInfo = stageTrackerMergeGateInfo(bare, now);
assert.deepStrictEqual([bareInfo.word, bareInfo.detail], ['Review passed', 'waiting for merge admission']);
assert.strictEqual(stageTrackerMergeGateWordHtml(bare, now), '<span class="ham-seg-t">Review passed <span class="ham-gate-detail">· waiting for merge admission</span></span>');
assert.ok(bareInfo.tip.startsWith('Open PR #15090 covers it · its PR is review passed · waiting for merge admission — Review passed, not yet admitted to auto merge.'), bareInfo.tip);
// No linked_prs at all (issue row alone): same.
const alone = Object.assign({}, issue15088, { linked_prs: undefined });
assert.ok(!leak.test(stageTrackerMergeGateWordHtml(alone, now)));
// A reason that names no PR (older "open PR covers it" token) is not a gate reason either.
const tok = Object.assign({}, alone, { reason: 'open PR covers it' });
assert.strictEqual(stageTrackerMergeGateInfo(tok, now).detail, 'waiting for merge admission');

// The issue row follows the PR's live gate state when the mirrored stage lags it
// (soaking PR, issue still labelled 060): both rows say Soaking.
const soakPR = { number: 15029, kind: 'pr', stage: 95, merge_gate: { state: 'soaking', soak_until: iso(now + 300000), soak_remaining_seconds: 300 } };
const soakIssue = { number: 15023, kind: 'issue', stage: 60, reason: 'open PR #15029 covers it (PR at 095-pre-merge-soak)', linked_prs: [{ number: 15029, stage: 95, merge_gate: soakPR.merge_gate }] };
assert.strictEqual(stageTrackerMergeGateInfo(soakIssue, now).word, stageTrackerMergeGateInfo(soakPR, now).word);
assert.strictEqual(stageTrackerMergeGateInfo(soakIssue, now).detail, '5m left');
// Two linked PRs: the one the reason names wins.
const two = Object.assign({}, issue15088, { linked_prs: [{ number: 15001, stage: 60, merge_gate: { state: 'ready' } }, { number: 15090, stage: 60, merge_gate: pr15090.merge_gate }] });
assert.strictEqual(stageTrackerMergeGateInfo(two, now).word, 'Review passed');
assert.strictEqual(stageTrackerCoveringPR(two).number, 15090);
assert.strictEqual(stageTrackerCoveringPR(pr15090), null, 'PR rows have no covering PR');

// The cover reason parser.
assert.strictEqual(stageTrackerCoverReasonText({ reason: 'open PR #15090 covers it (PR at 060-reviewed)' }), 'open PR #15090 covers it · its PR is review passed · waiting for merge admission');
assert.strictEqual(stageTrackerCoverReasonText({ reason: 'open PR #15090 covers it (PR at stage/080-awaiting-merge-admission)' }), 'open PR #15090 covers it · its PR is at the merge queue');
assert.strictEqual(stageTrackerCoverReasonText({ reason: 'open PR #7 covers it' }), 'open PR #7 covers it');
assert.strictEqual(stageTrackerCoverReasonText({ reason: 'check-pending' }), '');
assert.strictEqual(stageTrackerCoverReasonText({}), '');
console.log('merge-gate issue-row pill ok');
`)
}
