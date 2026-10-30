package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A train's front still at 050 (CI running, no verdict) — hive's own
// #15577 — carries its queue slot as provisional, its gate reason says
// "once it is ready", and the siblings parked behind it at 080 do not.
func TestStageTrackerMergeTrainProvisionalFront(t *testing.T) {
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	overlap := []string{"tracker-words.js"}
	store.Observe(stagelabels.Key("acme/console", 15577), stagelabels.ReviewPending, "required-check-pending:build-and-test")
	store.Observe(stagelabels.Key("acme/console", 15578), stagelabels.AwaitingMergeAdmission,
		prstage.MergeTrain{State: prstage.MergeTrainStateQueuedBehind, Front: 15577, Position: 2, Size: 2, OverlapFiles: overlap}.Reason())
	s.statusMu.Lock()
	for n, stage := range map[int]string{15577: "stage/050-review-pending", 15578: "stage/080-awaiting-merge-admission"} {
		s.status.Repos[0].OpenPrs = append(s.status.Repos[0].OpenPrs,
			FrontendPR{PullRequest: github.PullRequest{Number: n, Title: "Tracker words", Author: "someone", BaseRef: "v5", Labels: []string{stage}}})
	}
	s.statusMu.Unlock()
	resp := s.buildStageTracker(s.status, store, now)
	var front, rider *StageTrackerItem
	for i := range resp.Items {
		switch resp.Items[i].Number {
		case 15577:
			front = &resp.Items[i]
		case 15578:
			rider = &resp.Items[i]
		}
	}
	if front == nil || front.MergeTrain == nil || rider == nil || rider.MergeTrain == nil {
		t.Fatalf("expected both rows to carry merge_train: front=%+v rider=%+v", front, rider)
	}
	if !front.MergeTrain.Provisional {
		t.Errorf("front at 050 should carry merge_train.provisional: %+v", front.MergeTrain)
	}
	if front.MergeGate == nil || !strings.Contains(front.MergeGate.Reason, "once it is ready") || strings.Contains(front.MergeGate.Reason, "merges next") {
		t.Errorf("provisional front's gate reason should say once it is ready, not merges next: %+v", front.MergeGate)
	}
	if rider.MergeTrain.Provisional {
		t.Errorf("rider at 080 is merge-ready; its slot is not provisional: %+v", rider.MergeTrain)
	}
	if !stageTrackerMergeTrainProvisional(stagelabels.Reviewed.Number()) || stageTrackerMergeTrainProvisional(stagelabels.AwaitingMergeAdmission.Number()) || stageTrackerMergeTrainProvisional(0) {
		t.Errorf("provisional is every stage between 0 and 080, exclusive")
	}
}

// Ratchet (hive's own #15577: the chevron read "Merge queued · 1st" while
// the caption said "check build-and-test still running"): for every
// stage × queue state the chevron headline — the gate chip's word, else
// the train word — names the same blocker class as the stage: a row
// short of the merge gate reads its stage's blocker first with the slot
// as a trailing suffix; a merge-ready row reads its place in the queue.
func TestStageTrackerQueueSlotHeadlineAgreesWithStage(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, "function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/\"/g, '&quot;'); }\nconst assert = require('assert');\n"+
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_PRE_MERGE_SOAK")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_ACTIVE_WORD")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_GATE_WORD")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_TRAIN_ICON_PATH")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_TRAIN_ICON_TITLE")+"\n"+
		jsFunc(t, html, "stageTrackerTrainIconHtml")+"\n"+
		jsConstBlock(t, html, "STAGE_TRACKER_GATE_REASON_WORDS")+"\n"+
		jsConstBlock(t, html, "STAGE_TRACKER_ICONS")+"\n"+
		jsFunc(t, html, "stageTrackerIcon")+"\n"+
		jsFunc(t, html, "stageTrackerAgoText")+"\n"+
		jsFunc(t, html, "stageTrackerSoakLeft")+"\n"+
		jsFunc(t, html, "stageTrackerMergeGateStateOf")+"\n"+
		jsFunc(t, html, "stageTrackerGateReasonText")+"\n"+
		stageTrackerMergeGateInfoJS(t, html)+"\n"+
		jsFunc(t, html, "stageTrackerMergeGateWordHtml")+"\n"+`
const now = Date.parse('2026-10-30T00:01:00Z');
const stages = [10, 20, 30, 50, 60, 70, 71, 72, 74, 80, 85, 90, 95, 100];
const caption = 'Review swarm requested; waiting for a verdict on the current head — check build-and-test still running';
// The blocker class a headline names: 'queue' when it leads with the merge
// queue, 'stage' otherwise. A stage short of the gate must read 'stage'.
const classOf = s => /^Merge queue/.test(String(s || '')) ? 'queue' : 'stage';
for (const stage of stages) for (const state of ['front', 'queued-behind']) for (const flag of [true, false, undefined]) {
  const pos = state === 'front' ? 1 : 2;
  const mt = { id: 'v5#15577', state, front: 15577, head_number: 15577, position: pos, size: 2, length: 2, head_state: stage < 80 ? 'ci' : 'ready', head_reason: stage < 80 ? 'waiting for CI' : '' };
  if (flag !== undefined) mt.provisional = flag;
  const item = { repo: 'hivecommons/hive', number: 15577, kind: 'pr', title: 't', stage, modifiers: [], reason: stage === 50 ? 'required-check-pending:build-and-test' : '', caption, merge_train: mt, merge_gate: { state, reason: '1st in the merge queue', last_sweep_at: '2026-10-30T00:00:30Z' } };
  const label = 'stage ' + stage + ' / ' + state + ' / provisional=' + flag;
  // The server's flag wins; without it the stage decides.
  const provisional = flag === undefined ? stage < 80 : flag;
  const want = provisional ? 'stage' : 'queue';
  const word = stageTrackerTrainWord(item);
  assert.strictEqual(classOf(word), want, label + ' train word: ' + JSON.stringify(word));
  const info = stageTrackerMergeGateInfo(item, now);
  assert.ok(info && info.word, label + ' chip: ' + JSON.stringify(info));
  assert.strictEqual(classOf(info.word), want, label + ' chip word: ' + JSON.stringify(info.word));
  const chip = stageTrackerMergeGateWordHtml(item, now).replace(/<[^>]+>/g, '');
  assert.strictEqual(classOf(chip), want, label + ' chip html: ' + JSON.stringify(chip));
  if (provisional) {
    // Blocker first, slot as a suffix — never the slot alone.
    assert.ok(/queued (1st|2nd) of 2 once ready$/.test(word), label + ' suffix: ' + JSON.stringify(word));
    assert.strictEqual(info.detail, 'queued ' + (pos === 1 ? '1st' : '2nd') + ' of 2 once ready', label + ' detail');
    assert.ok(!/merges next/.test(info.tip), label + ' hover promises merges next: ' + info.tip);
    assert.ok(info.until === 0, label + ' no ETA countdown ahead of readiness');
  } else {
    assert.ok(/^Merge queued · (1st|2nd)( of \d+)?$/.test(word), label + ' place word: ' + JSON.stringify(word));
  }
}
// hive's own #15577 exactly: 050, front, CI running — the chevron leads with CI / Review.
const own = { repo: 'hivecommons/hive', number: 15577, kind: 'pr', stage: 50, modifiers: [], reason: 'required-check-pending:build-and-test', caption,
  merge_train: { id: 'v5#15577', state: 'front', front: 15577, position: 1, size: 2, head_number: 15577, head_state: 'ci', head_reason: 'waiting for CI', provisional: true },
  merge_gate: { state: 'front', reason: '1st in the merge queue; merges first once it is ready (1st of 2, overlapping files: tracker-words.js)' } };
assert.strictEqual(stageTrackerTrainWord(own), 'CI / Review · queued 1st of 2 once ready');
const ownInfo = stageTrackerMergeGateInfo(own, now);
assert.strictEqual(ownInfo.word, 'CI / Review');
assert.strictEqual(ownInfo.detail, 'queued 1st of 2 once ready');
assert.ok(/CI in progress/.test(ownInfo.tip), ownInfo.tip);
assert.ok(/merges first once it is ready/.test(ownInfo.tip), ownInfo.tip);
`)
}
