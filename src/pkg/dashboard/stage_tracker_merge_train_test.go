package dashboard

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// TestStageTrackerMergeTrainPayload: a train of four PRs to v5 whose lane
// records are the merge-train reasons (hivecommons/hive#15117). The front
// carries no train reason of its own (the lane evaluates it on its normal
// path) and is marked from its siblings; siblings read queued-behind with
// the PRs ahead; a rebasing sibling and an exhausted one carry the attempt
// counters, and only the exhausted one is a person's row.
func TestStageTrackerMergeTrainPayload(t *testing.T) {
	now := time.Date(2026, 10, 27, 14, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	overlap := []string{"stage_tracker.go", "index.html"}
	queued := func(pos int) string {
		return prstage.MergeTrain{State: prstage.MergeTrainStateQueuedBehind, Front: 15074, Position: pos, Size: 4, OverlapFiles: overlap}.Reason()
	}
	store.Observe(stagelabels.Key("acme/console", 15074), stagelabels.AwaitingMergeAdmission, "")
	store.Observe(stagelabels.Key("acme/console", 15085), stagelabels.AwaitingMergeAdmission, queued(2))
	store.Observe(stagelabels.Key("acme/console", 15087), stagelabels.AwaitingMergeAdmission, prstage.MergeTrain{State: prstage.MergeTrainStateRebasing, Front: 15074, Position: 3, Size: 4, Attempt: 1, MaxAttempts: 2, OverlapFiles: overlap}.Reason())
	store.Observe(stagelabels.Key("acme/console", 15097), stagelabels.NeedsDecision, prstage.MergeTrain{State: prstage.MergeTrainStateRebaseExhausted, Front: 15074, Position: 4, Size: 4, Attempt: 2, MaxAttempts: 2, OverlapFiles: overlap}.Reason())
	s.statusMu.Lock()
	for _, n := range []int{15074, 15085, 15087, 15097} {
		stage := "stage/080-awaiting-merge-admission"
		if n == 15097 {
			stage = "stage/085-needs-decision"
		}
		labels := []string{stage, "author/hive"}
		if n != 15074 {
			labels = append(labels, "needs-rebase")
		}
		s.status.Repos[0].OpenPrs = append(s.status.Repos[0].OpenPrs,
			FrontendPR{PullRequest: github.PullRequest{Number: n, Title: "Tracker change", Author: "hive-app[bot]", AppAuthored: true, BaseRef: "v5", Labels: labels}})
	}
	s.statusMu.Unlock()
	resp := s.buildStageTracker(s.status, store, now)

	front := trackerItem(t, resp, "pr", 15074)
	if front.MergeTrain == nil || front.MergeTrain.State != MergeGateStateTrainFront || front.MergeTrain.Position != 1 || front.MergeTrain.Size != 4 || front.MergeTrain.ID != "v5#15074" {
		t.Fatalf("front merge_train = %+v", front.MergeTrain)
	}
	if front.MergeGate == nil || front.MergeGate.State != MergeGateStateTrainFront || !strings.Contains(front.Caption, "1st in a merge queue of 4 PRs") {
		t.Fatalf("front gate %+v caption %q", front.MergeGate, front.Caption)
	}
	if hasMod(front, modNeedsRebase) || len(front.MergeTrain.Ahead) != 0 {
		t.Fatalf("front must not wear needs-rebase nor have PRs ahead: %+v", front)
	}

	sib := trackerItem(t, resp, "pr", 15085)
	if sib.Stage != 80 || sib.MergeTrain == nil || sib.MergeTrain.State != MergeGateStateQueuedBehind || sib.MergeTrain.Position != 2 || sib.MergeTrain.Front != 15074 {
		t.Fatalf("sibling = stage %d train %+v", sib.Stage, sib.MergeTrain)
	}
	if sib.MergeGate == nil || sib.MergeGate.State != "queued-behind" || sib.MergeGate.Reason != "behind #15074 in merge train (2nd of 4, overlapping files: stage_tracker.go, index.html)" {
		t.Fatalf("sibling merge_gate = %+v", sib.MergeGate)
	}
	if hasMod(sib, modNeedsRebase) || hasMod(sib, modNeedsHuman) || !hasStale(sib, modNeedsRebase) {
		t.Fatalf("sibling modifiers %v stale %v: needs-rebase is the train's to repair, not a human flag", sib.Modifiers, sib.StaleLabels)
	}
	if got := sib.MergeTrain.Ahead; len(got) != 1 || got[0] != 15074 {
		t.Fatalf("ahead = %v, want [15074]", got)
	}
	if !strings.HasPrefix(sib.Caption, "Approved; waiting its turn in the merge train — behind #15074") || sib.Actor != "automerge lane" {
		t.Fatalf("sibling caption %q actor %q", sib.Caption, sib.Actor)
	}
	if sib.DecisionNeeded != nil {
		t.Fatalf("a queued sibling owes no decision: %+v", sib.DecisionNeeded)
	}

	rebasing := trackerItem(t, resp, "pr", 15087)
	if rebasing.MergeTrain.State != MergeGateStateRebasing || rebasing.MergeTrain.Attempt != 1 || rebasing.MergeTrain.MaxAttempts != 2 || !strings.Contains(rebasing.Caption, "rebasing this PR (attempt 1 of 2)") {
		t.Fatalf("rebasing = %+v caption %q", rebasing.MergeTrain, rebasing.Caption)
	}
	if got := rebasing.MergeTrain.Ahead; len(got) != 2 || got[0] != 15074 || got[1] != 15085 {
		t.Fatalf("rebasing ahead = %v", got)
	}
	// Every member carries the whole roster in merge order, so a view that
	// draws only some of them can name the rest in the train header.
	for _, it := range []StageTrackerItem{front, sib, rebasing} {
		if got := it.MergeTrain.Members; fmt.Sprint(got) != fmt.Sprint([]int{15074, 15085, 15087, 15097}) {
			t.Fatalf("#%d members = %v", it.Number, got)
		}
	}

	exhausted := trackerItem(t, resp, "pr", 15097)
	if exhausted.Stage != 85 || exhausted.MergeTrain.State != MergeGateStateRebaseExhausted || !hasMod(exhausted, modNeedsRebase) {
		t.Fatalf("exhausted = stage %d train %+v mods %v", exhausted.Stage, exhausted.MergeTrain, exhausted.Modifiers)
	}
	if !strings.Contains(exhausted.Caption, "failed 2 times; a maintainer rebases and merges") || exhausted.Actor != "maintainer" {
		t.Fatalf("exhausted caption %q actor %q", exhausted.Caption, exhausted.Actor)
	}
	d := exhausted.DecisionNeeded
	if d == nil || len(d.Options) == 0 || d.Options[0].Action != decisionActionMergeNow || d.Options[0].Label != rebaseMergeFollowThroughLabel {
		t.Fatalf("exhausted decision = %+v", d)
	}
	if !d.Options[0].Enabled || d.Options[0].Consequence != rebaseMergeFollowThroughConsequence(15097, "v5") {
		t.Fatalf("merge-now leads as the rebase-then-merge follow-through: %+v", d.Options[0])
	}
	if !strings.Contains(d.Question, "could not rebase this PR onto the merged front #15074 after 2 attempts") {
		t.Fatalf("question = %q", d.Question)
	}

	raw, err := json.Marshal(sib)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"merge_gate":{"state":"queued-behind","reason":"behind #15074 in merge train (2nd of 4, overlapping files: stage_tracker.go, index.html)"`,
		`"merge_train":{"id":"v5#15074","state":"queued-behind","front":15074,"position":2,"size":4,"overlap_files":["stage_tracker.go","index.html"],"ahead":[15074],"members":[15074,15085,15087,15097],"length":4,"head_number":15074,"head_state":"ready","head_reason":"approved and green; merges on hive's next pass","head_since":"2026-10-27T14:00:00Z","eta_s":300,"eta_at":"2026-10-27T14:05:00Z","eta_basis":"1 ahead"}`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("payload lacks %s:\n%s", want, raw)
		}
	}
	// Rows outside the train are untouched.
	if plain := trackerItem(t, resp, "pr", 102); plain.MergeTrain != nil {
		t.Fatalf("PR 102 is not in a train: %+v", plain.MergeTrain)
	}
	// A dirty front the train is rebasing carries its own reason and is not
	// re-marked.
	store.Observe(stagelabels.Key("acme/console", 15074), stagelabels.AwaitingMergeAdmission, prstage.MergeTrain{State: prstage.MergeTrainStateRebasing, Front: 15074, Position: 1, Size: 4, Attempt: 1, MaxAttempts: 2, OverlapFiles: overlap}.Reason())
	resp = s.buildStageTracker(s.status, store, now)
	if f := trackerItem(t, resp, "pr", 15074); f.MergeTrain.State != MergeGateStateRebasing || f.MergeTrain.Position != 1 {
		t.Fatalf("rebasing front = %+v", f.MergeTrain)
	}
}

func hasStale(it StageTrackerItem, want string) bool {
	for _, x := range it.StaleLabels {
		if x == want {
			return true
		}
	}
	return false
}

// TestStageTrackerMergeTrainCaptions pins the sentence per state.
func TestStageTrackerMergeTrainCaptions(t *testing.T) {
	overlap := []string{"a.go"}
	cases := []struct {
		reason string
		want   string
	}{
		{prstage.MergeTrain{State: prstage.MergeTrainStateQueuedBehind, Front: 7, Position: 3, Size: 4, OverlapFiles: overlap}.Reason(), "Approved; waiting its turn in the merge train — behind #7 in merge train (3rd of 4, overlapping files: a.go)"},
		{prstage.MergeTrain{State: prstage.MergeTrainStateRebasing, Front: 0, Position: 1, Size: 2, Attempt: 2, MaxAttempts: 2}.Reason(), "Conflicts with the base; the hive is rebasing this PR (attempt 2 of 2)"},
		{prstage.MergeTrain{State: prstage.MergeTrainStateRebaseExhausted, Front: 7, Position: 2, Size: 2, Attempt: 2, MaxAttempts: 2}.Reason(), "The rebase onto the merged front #7 failed 2 times; a maintainer rebases and merges"},
		{"check-pending", ""},
	}
	for _, tc := range cases {
		if got := stageTrackerMergeTrainCaption(tc.reason); got != tc.want {
			t.Errorf("caption(%q) = %q, want %q", tc.reason, got, tc.want)
		}
	}
	if stageTrackerMergeTrainFrom("held") != nil {
		t.Fatal("a plain reason is no train")
	}
	var nilItem *StageTrackerItem
	stageTrackerApplyMergeTrain(nilItem)
	stageTrackerApplyMergeTrain(&StageTrackerItem{Kind: "issue", Reason: cases[0].reason})
}

// TestStageTrackerMergeTrainSPA runs the SPA's train helpers under node:
// the pill wording per state, the contiguous ordering with the header line
// and rail classes, and the tooltip's queue + files.
func TestStageTrackerMergeTrainSPA(t *testing.T) {
	html := indexHTML(t)
	for _, css := range []string{".ham-train-head {", ".ham-row.ham-train {", ".ham-row.ham-train-last::before {", ".ham-train-chip {"} {
		if !strings.Contains(html, css) {
			t.Fatalf("stylesheet lacks %q", css)
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the merge-train rendering was NOT executed by this run")
	}
	script := strings.Join([]string{
		"const assert = require('assert');",
		"function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/\"/g, '&quot;'); }",
		"function agentNowClockTime(ms) { return 'T' + ms; }",
		"function agentNowAgo(ms) { return Math.round(ms / 60000) + ' min ago'; }",
		"const STAGE_TRACKER_MOTION_ACTIVE = 'active', STAGE_TRACKER_MOTION_STATIC = 'static';",
		"function stageTrackerMotion() { return STAGE_TRACKER_MOTION_STATIC; }",
		jsConstLine(t, html, "STAGE_TRACKER_PATH"),
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL"),
		jsConstLine(t, html, "STAGE_TRACKER_TRAIL_HOPS"),
		jsConstLine(t, html, "STAGE_TRACKER_PR_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_DECISION"),
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_NEEDS_DECISION"),
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_DECISION_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_TRIAGE_NEEDED"),
		jsConstLine(t, html, "STAGE_TRACKER_POSSIBLY_EXISTS"),
		jsConstLine(t, html, "STAGE_TRACKER_PRE_MERGE_SOAK"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTIVE_WORD"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTOR"),
		jsConstLine(t, html, "STAGE_TRACKER_MOD_TEXT"),
		jsConstLine(t, html, "STAGE_TRACKER_DRAFT_TIP"),
		jsConstLine(t, html, "STAGE_TRACKER_OPEN_STALE_DAYS"),
		jsConstLine(t, html, "STAGE_TRACKER_NEW_GATE_MARK"),
		jsConstLine(t, html, "STAGE_TRACKER_TRAIN_STATE_FRONT"),
		jsConstLine(t, html, "STAGE_TRACKER_TRAIN_STATE_QUEUED"),
		jsConstLine(t, html, "STAGE_TRACKER_TRAIN_STATE_REBASING"),
		jsConstLine(t, html, "STAGE_TRACKER_TRAIN_STATE_EXHAUSTED"),
		jsConstLine(t, html, "STAGE_TRACKER_TRAIN_STATE_WORD"),
		jsConstLine(t, html, "STAGE_TRACKER_GATE_DUPLICATE_PREFIX"),
		jsFunc(t, html, "stageTrackerGateDuplicateOf"),
		jsFunc(t, html, "stageTrackerMergeGateStateWord"),
		jsConstLine(t, html, "STAGE_TRACKER_TRAIN_ICON_PATH"),
		jsConstLine(t, html, "STAGE_TRACKER_TRAIN_ICON_TITLE"),
		jsFunc(t, html, "stageTrackerTrainIconHtml"),
		jsConstBlock(t, html, "STAGE_TRACKER_ICONS"),
		stageTrackerManualMergeJS(t, html),
		stageTrackerSentinelJS(t, html),
		jsFunc(t, html, "stageTrackerIcon"),
		jsFunc(t, html, "stageTrackerHoldLabels"),
		jsFunc(t, html, "stageTrackerRegressionHandled"),
		jsFunc(t, html, "stageTrackerAnchor"),
		jsFunc(t, html, "stageTrackerSoakLeft"),
		jsFunc(t, html, "stageTrackerBlockedTip"),
		jsFunc(t, html, "stageTrackerBlockedOnPR"),
		jsFunc(t, html, "stageTrackerStaleLabelsText"),
		jsFunc(t, html, "stageTrackerRegressionText"),
		jsFunc(t, html, "stageTrackerTrailText"),
		jsFunc(t, html, "stageTrackerHopName"),
		jsFunc(t, html, "stageTrackerOpenSince"),
		jsFunc(t, html, "stageTrackerShortAgo"),
		jsFunc(t, html, "stageTrackerNewState"),
		jsFunc(t, html, "stageTrackerNewLines"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTOR_NONE"),
		jsConstLine(t, html, "STAGE_TRACKER_NOBODY_LEAD"),
		jsFunc(t, html, "stageTrackerStageActor"),
		jsFunc(t, html, "stageTrackerActorKind"),
		jsFunc(t, html, "stageTrackerActorTip"),
		jsFunc(t, html, "stageTrackerHeadline"),
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
		jsFunc(t, html, "stageTrackerTrainLines"),
		jsFunc(t, html, "stageTrackerRowTrain"),
		jsFunc(t, html, "stageTrackerTrainKey"),
		jsFunc(t, html, "stageTrackerTrainOrder"),
		jsFunc(t, html, "stageTrackerTrainContiguous"),
		jsFunc(t, html, "stageTrackerTrainMissing"),
		jsFunc(t, html, "stageTrackerTrainHeaderText"),
		jsFunc(t, html, "stageTrackerTrainHeaderHtml"),
		jsFunc(t, html, "stageTrackerTrainRowClass"),
		jsFunc(t, html, "stageTrackerTrainRows"),
		jsFunc(t, html, "stageTrackerSegments"),
		jsFunc(t, html, "stageTrackerTooltip"),
	}, "\n") + `
var _stageTracker = { stages: [] };
const now = Date.parse('2026-10-27T14:00:00Z');
const stages = [{ number: 50, label: 'Review pending', short: 'CI / Review' }, { number: 80, label: 'Awaiting merge admission', short: 'Merge queue' }, { number: 85, label: 'Needs human decision', short: 'Needs decision' }, { number: 100, label: 'Merge queue', short: 'Merge queue' }];
const files = ['stage_tracker.go', 'index.html'];
const mk = (n, stage, mt, extra) => Object.assign({ repo: 'hivecommons/hive', number: n, kind: 'pr', title: 'PR ' + n, stage, stage_since: '2026-10-27T13:00:00Z', base_ref: 'v5', modifiers: [], merge_train: Object.assign({ id: 'v5#15074', front: 15074, size: 4, overlap_files: files }, mt) }, extra || {});
const front = mk(15074, 80, { state: 'front', position: 1 });
const second = mk(15085, 80, { state: 'queued-behind', position: 2, ahead: [15074] });
const third = mk(15087, 80, { state: 'rebasing', position: 3, attempt: 1, max_attempts: 2, ahead: [15074, 15085] });
const fourth = mk(15097, 85, { state: 'rebase-exhausted', position: 4, attempt: 2, max_attempts: 2, ahead: [15074, 15085, 15087] }, { modifiers: ['needs-rebase'] });
const other = { repo: 'hivecommons/hive', number: 15100, kind: 'pr', title: 'Docs', stage: 80, modifiers: [] };

// Pill wording per state: the place in the owner's words (#15395 —
// "'front' should be 'merge queued - 1st' and so on").
assert.strictEqual(stageTrackerTrainWord(front), 'Merge queued · 1st of 4');
assert.strictEqual(stageTrackerTrainWord(second), 'Merge queued · 2nd of 4');
assert.strictEqual(stageTrackerTrainWord(third), 'Rebasing · attempt 1/2');
assert.strictEqual(stageTrackerTrainWord(fourth), 'Needs human decision · rebase failed twice');
assert.strictEqual(stageTrackerTrainWord(other), '');
for (const [n, w] of [[1, '1st'], [2, '2nd'], [3, '3rd'], [4, '4th'], [11, '11th'], [12, '12th'], [13, '13th'], [21, '21st'], [22, '22nd'], [111, '111th']]) assert.strictEqual(stageTrackerOrdinal(n), w, 'ordinal ' + n);
// The lit chevron carries the word: 080 for the members, the 085 detour for the exhausted one.
const lit = it => stageTrackerSegments(it, stages, now).find(s => s.state === 'active');
assert.strictEqual(lit(front).short, 'Merge queued · 1st of 4');
assert.strictEqual(lit(second).short, 'Merge queued · 2nd of 4');
assert.strictEqual(lit(third).short, 'Rebasing · attempt 1/2');
assert.strictEqual(lit(fourth).detour.text, 'Needs human decision · rebase failed twice');
assert.strictEqual(lit(other).short, 'Ready for auto merge');

// Tooltip: the queue ahead and the overlapping files.
const tip = stageTrackerTooltip(third, now);
assert.ok(tip.includes('Merge train: 4 PRs to v5 touch the same files; they merge one at a time, in order (1st: #15074)'), tip);
assert.ok(tip.includes('Position: 3rd of 4 · ahead: #15074, #15085'), tip);
assert.ok(tip.includes('The PR ahead merged; rebase attempt 1 of 2 in progress'), tip);
assert.ok(tip.includes('Overlapping files: stage_tracker.go, index.html'), tip);
assert.ok(stageTrackerTooltip(front, now).includes('Position: 1st — merges next'), 'front tip');
assert.ok(stageTrackerTooltip(fourth, now).includes('Rebase failed 2 of 2 times — a maintainer rebases, then merges'));
assert.ok(!stageTrackerTooltip(other, now).includes('Merge train'));

// Contiguity: a sorted list that interleaves the train is regrouped at the
// first member's slot, in merge order; other rows keep their order.
const sorted = [fourth, other, third, { repo: 'x/y', number: 1, kind: 'issue', stage: 50 }, front, second];
// Queue facts (#15395): the chip is the position, never the slug; the
// hover sentence names the head, what it waits on and the estimate.
const queued = mk(15319, 80, { state: 'queued-behind', position: 2, ahead: [15315], length: 3, head_number: 15315, head_state: 'soaking', head_reason: 'soaking, 12m left', eta_s: 1500 });
assert.strictEqual(stageTrackerTrainWord(queued), 'Merge queued · 2nd of 3');
assert.strictEqual(stageTrackerTrainQueueTip(queued.merge_train), '2nd of 3 in the merge queue · behind #15315 (soaking, 12m left) · ETA ~25m. Hive merges one at a time, in order.');
const firstQ = mk(15315, 80, { state: 'front', position: 1, length: 3, head_number: 15315, head_state: 'soaking', head_reason: 'soaking, 12m left', eta_s: 780 });
assert.strictEqual(stageTrackerTrainQueueTip(firstQ.merge_train), '1st of 3 in the merge queue · merges next (soaking, 12m left) · ETA ~13m. Hive merges one at a time, in order.');
const thirdQ = mk(15374, 80, { state: 'queued-behind', position: 3, ahead: [15315, 15319], length: 3, head_number: 15315, head_state: 'needs-human', head_reason: 'waiting for a maintainer (untrusted author)', eta_s: 0 });
assert.strictEqual(stageTrackerTrainWord(thirdQ), 'Merge queued · 3rd of 3');
assert.strictEqual(stageTrackerTrainQueueTip(thirdQ.merge_train), '3rd of 3 in the merge queue · behind #15315 (waiting for a maintainer (untrusted author)) · no estimate until #15315 moves. Hive merges one at a time, in order.');
const stalledQ = mk(15374, 80, { state: 'queued-behind', position: 3, ahead: [15315, 15319], length: 3, head_number: 15315, head_state: 'ci', head_reason: 'waiting for CI', stalled: true, stalled_s: 3 * 3600 });
assert.strictEqual(stageTrackerTrainWord(stalledQ), 'Merge queue · stalled — waiting on #15315 (waiting for CI) · 3h');
assert.ok(stageTrackerTrainQueueTip(stalledQ.merge_train).includes('#15315 has not moved for 3h'), stageTrackerTrainQueueTip(stalledQ.merge_train));
// Neither slug nor the word "front" reaches a person: chevron word, hover,
// tooltip lines, header chip and the ⛓-era header text (owner: "i dont
// know what queued - behind means at all").
for (const it of [queued, firstQ, thirdQ, stalledQ, front, second, third, fourth]) {
  const seen = [stageTrackerTrainWord(it), stageTrackerTrainQueueTip(it.merge_train), stageTrackerTrainLines(it).join(' '), stageTrackerTrainHeaderText(it.merge_train), stageTrackerTooltip(it, now)].join(' | ');
  assert.ok(!/front|queued-behind|rebase-exhausted/i.test(seen), 'no slug or "front" for #' + it.number + ': ' + seen);
}
// Ordinal places beyond the first few.
for (const [pos, w] of [[11, '11th'], [12, '12th'], [13, '13th'], [21, '21st'], [22, '22nd'], [23, '23rd']]) assert.strictEqual(stageTrackerTrainWord(mk(1, 80, { state: 'queued-behind', position: pos, length: 30 })), 'Merge queued · ' + w + ' of 30');
// Every train state — the 1st included — lights the Merge queue column,
// whatever stage the lane holds the head at (hive #15400 at 050 lit CI / Review).
for (const st of ['front', 'queued-behind', 'rebasing', 'rebase-exhausted']) for (const stage of [50, 60, 70, 74, 80, 95]) assert.strictEqual(stageTrackerAnchor(stage, stages, 'pr', mk(2, stage, { state: st, position: 1 })), 80, st + ' at ' + stage);
assert.strictEqual(stageTrackerAnchor(50, stages, 'pr', other), 50, 'no train: stage rules as before');
// The 1st-in-train head at 050 lights the Merge queue column, but its
// word leads with the stage's blocker — the slot is provisional until
// CI and the verdict are in (hive #15577 read "Merge queued · 1st" over
// a caption saying build-and-test was still running).
const headAt50 = mk(15400, 50, { state: 'front', position: 1 });
assert.strictEqual(lit(headAt50).number, 80);
assert.strictEqual(lit(headAt50).short, 'CI / Review · queued 1st of 4 once ready');
const grouped = stageTrackerTrainContiguous(sorted).map(it => it.number);
assert.deepStrictEqual(grouped, [15074, 15085, 15087, 15097, 15100, 1]);
// Header line before the run, once; rail classes on members, last closes it.
const rows = stageTrackerTrainRows(stageTrackerTrainContiguous(sorted), (it) => ({ key: String(it.number), html: '' }));
assert.deepStrictEqual(rows.map(r => r.key), ['train:hivecommons/hive|v5#15074', '15074', '15085', '15087', '15097', '15100', '1']);
assert.ok(rows[0].html.includes('</svg> merge train · 4 PRs · 1st: #15074'), rows[0].html);
assert.ok(rows[0].html.includes('class="ham-train-chip"') && rows[0].html.includes('overlapping: stage_tracker.go, index.html'), rows[0].html);
assert.strictEqual(stageTrackerTrainHeaderText(front.merge_train), 'merge train · 4 PRs · 1st: #15074');
// The train icon: an inline SVG (currentColor, 16-unit box) titled "Merge
// queue", one path shared with the Merge queue column / 080 chevron.
const ico = stageTrackerTrainIconHtml('ham-train-icon');
assert.ok(ico.startsWith('<svg class="ham-train-icon" viewBox="0 0 16 16" fill="none" stroke="currentColor"'), ico);
assert.ok(ico.includes('<title>Merge queue</title>') && ico.includes('aria-label="Merge queue"'), ico);
assert.ok(ico.includes(STAGE_TRACKER_TRAIN_ICON_PATH) && (ico.match(/<circle/g) || []).length === 2, 'two wheels: ' + ico);
assert.strictEqual(STAGE_TRACKER_ICONS[80], STAGE_TRACKER_TRAIN_ICON_PATH);
assert.ok(rows[0].html.includes(ico), rows[0].html);
assert.strictEqual(stageTrackerTrainRowClass(front, null, second), ' ham-train ham-train-front');
assert.strictEqual(stageTrackerTrainRowClass(second, front, third), ' ham-train');
assert.strictEqual(stageTrackerTrainRowClass(fourth, third, other), ' ham-train ham-train-last');
assert.strictEqual(stageTrackerTrainRowClass(other, fourth, null), '');
// The grouped view's {group}/{item} wrappers are read through.
const wrapped = stageTrackerTrainRows([{ group: { key: 'g' } }, { group: { key: 'g' }, item: front }, { group: { key: 'g' }, item: second }], (e) => ({ key: e.item ? String(e.item.number) : e.group.key, html: '' }));
assert.deepStrictEqual(wrapped.map(r => r.key), ['g', 'train:hivecommons/hive|v5#15074', '15074', '15085']);

// Header count == rows drawn under it (owner, hive v5#15425: "this says
// there are 3 PRs in the merge train but only 1 is shown"). The members
// #15529 / #15530 were drawn under their issues #15493 / #15523; an issue
// row whose open linked PR is a member now rides the train run, in the
// PR's merge order, and the header counts the run.
const tmk = (n, pos, extra) => Object.assign({ id: 'v5#15425', front: 15425, size: 3, members: [15425, 15529, 15530], overlap_files: ['tracker-row.js', 'index.html'], state: pos === 1 ? 'front' : 'queued-behind', position: pos, ahead: [15425, 15529].slice(0, pos - 1) }, extra || {});
const t1 = mk(15425, 80, tmk(15425, 1));
const lp2 = { number: 15529, kind: 'pr', stage: 80, state: 'open', modifiers: [], merge_train: tmk(15529, 2) };
const lp3 = { number: 15530, kind: 'pr', stage: 80, state: 'open', modifiers: [], merge_train: tmk(15530, 3) };
const issue2 = { repo: 'hivecommons/hive', number: 15493, kind: 'issue', stage: 80, modifiers: [], linked_prs: [lp2] };
const issue3 = { repo: 'hivecommons/hive', number: 15523, kind: 'issue', stage: 80, modifiers: [], linked_prs: [lp3] };
const mergedIssue = { repo: 'hivecommons/hive', number: 15000, kind: 'issue', stage: 80, modifiers: [], linked_prs: [{ number: 14999, stage: 110, state: 'open', modifiers: ['merged'], merge_train: tmk(14999, 2) }] };
const plainIssue = { repo: 'hivecommons/hive', number: 15499, kind: 'issue', stage: 85, modifiers: [] };
assert.strictEqual(stageTrackerTrainKey(issue2), 'hivecommons/hive|v5#15425', 'an issue row rides its nested PR\'s train');
assert.strictEqual(stageTrackerTrainKey(mergedIssue), '', 'a merged nested PR is no train');
assert.strictEqual(stageTrackerTrainKey(plainIssue), '');
const run = stageTrackerTrainContiguous([plainIssue, issue3, t1, mergedIssue, issue2]).map(it => it.number);
assert.deepStrictEqual(run, [15499, 15425, 15493, 15523, 15000], 'issue rows join the run in the PR\'s merge order');
const fullRows = stageTrackerTrainRows(stageTrackerTrainContiguous([issue3, t1, issue2, plainIssue]), (it) => ({ key: String(it.number), html: '' }));
assert.deepStrictEqual(fullRows.map(r => r.key), ['train:hivecommons/hive|v5#15425', '15425', '15493', '15523', '15499']);
assert.ok(fullRows[0].html.includes('</svg> merge train · 3 PRs · 1st: #15425'), fullRows[0].html);
assert.strictEqual(stageTrackerTrainRowClass(issue2, t1, issue3), ' ham-train');
assert.strictEqual(stageTrackerTrainRowClass(issue3, issue2, plainIssue), ' ham-train ham-train-last');
// A view that carries only the head (a filter hid the other two) says so
// and names them — never "3 PRs" over one row.
const partRows = stageTrackerTrainRows([t1, plainIssue], (it) => ({ key: String(it.number), html: '' }));
assert.deepStrictEqual(partRows.map(r => r.key), ['train:hivecommons/hive|v5#15425', '15425', '15499']);
assert.ok(partRows[0].html.includes('</svg> merge train · 1 of 3 PRs here · 1st: #15425 · not in this view: #15529, #15530'), partRows[0].html);
assert.ok(!partRows[0].html.includes('3 PRs ·'), partRows[0].html);
assert.strictEqual(stageTrackerTrainHeaderText(t1.merge_train, [15425, 15530]), 'merge train · 2 of 3 PRs here · 1st: #15425 · not in this view: #15529');
// Without a member list the header still counts honestly.
assert.strictEqual(stageTrackerTrainHeaderText({ id: 'v5#1', front: 1, size: 3 }, [1]), 'merge train · 1 of 3 PRs here · 1st: #1 · not in this view: 2 more');
assert.strictEqual(stageTrackerTrainHeaderText(t1.merge_train, [15425, 15529, 15530]), 'merge train · 3 PRs · 1st: #15425');
assert.strictEqual(stageTrackerTrainHeaderText(t1.merge_train), 'merge train · 3 PRs · 1st: #15425');
// No members on the page, no header.
assert.deepStrictEqual(stageTrackerTrainRows([plainIssue, mergedIssue], (it) => ({ key: String(it.number), html: '' })).map(r => r.key), ['15499', '15000']);
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
