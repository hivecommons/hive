package dashboard

import (
	"encoding/json"
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
	if front.MergeGate == nil || front.MergeGate.State != MergeGateStateTrainFront || !strings.Contains(front.Caption, "Front of a merge train of 4 PRs") {
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
	if d.Options[0].Enabled || d.Options[0].ReasonIfDisabled != decisionWhyMergeNowUnwired {
		t.Fatalf("merge-now must be offered disabled with the honest reason until #15104 lands: %+v", d.Options[0])
	}
	if !strings.Contains(d.Question, "could not rebase this PR onto the merged front #15074 after 2 attempts") {
		t.Fatalf("question = %q", d.Question)
	}

	raw, err := json.Marshal(sib)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"merge_gate":{"state":"queued-behind","reason":"behind #15074 in merge train (2nd of 4, overlapping files: stage_tracker.go, index.html)"`,
		`"merge_train":{"id":"v5#15074","state":"queued-behind","front":15074,"position":2,"size":4,"overlap_files":["stage_tracker.go","index.html"],"ahead":[15074]}`} {
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
	if mergeNowWired {
		t.Fatal("merge-now is not wired until hivecommons/hive#15104 lands")
	}
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
		jsConstLine(t, html, "STAGE_TRACKER_TRAIN_GLYPH"),
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
		jsFunc(t, html, "stageTrackerTrainWord"),
		jsFunc(t, html, "stageTrackerTrainLines"),
		jsFunc(t, html, "stageTrackerTrainKey"),
		jsFunc(t, html, "stageTrackerTrainContiguous"),
		jsFunc(t, html, "stageTrackerTrainHeaderText"),
		jsFunc(t, html, "stageTrackerTrainHeaderHtml"),
		jsFunc(t, html, "stageTrackerTrainRowClass"),
		jsFunc(t, html, "stageTrackerTrainRows"),
		jsFunc(t, html, "stageTrackerSegments"),
		jsFunc(t, html, "stageTrackerTooltip"),
	}, "\n") + `
var _stageTracker = { stages: [] };
const now = Date.parse('2026-10-27T14:00:00Z');
const stages = [{ number: 50, label: 'Review pending', short: 'CI / Review' }, { number: 80, label: 'Awaiting merge admission', short: 'Merge gate' }, { number: 85, label: 'Needs human decision', short: 'Needs decision' }, { number: 100, label: 'Merge queue', short: 'Merge queue' }];
const files = ['stage_tracker.go', 'index.html'];
const mk = (n, stage, mt, extra) => Object.assign({ repo: 'hivecommons/hive', number: n, kind: 'pr', title: 'PR ' + n, stage, stage_since: '2026-10-27T13:00:00Z', base_ref: 'v5', modifiers: [], merge_train: Object.assign({ id: 'v5#15074', front: 15074, size: 4, overlap_files: files }, mt) }, extra || {});
const front = mk(15074, 80, { state: 'front', position: 1 });
const second = mk(15085, 80, { state: 'queued-behind', position: 2, ahead: [15074] });
const third = mk(15087, 80, { state: 'rebasing', position: 3, attempt: 1, max_attempts: 2, ahead: [15074, 15085] });
const fourth = mk(15097, 85, { state: 'rebase-exhausted', position: 4, attempt: 2, max_attempts: 2, ahead: [15074, 15085, 15087] }, { modifiers: ['needs-rebase'] });
const other = { repo: 'hivecommons/hive', number: 15100, kind: 'pr', title: 'Docs', stage: 80, modifiers: [] };

// Pill wording per state.
assert.strictEqual(stageTrackerTrainWord(front), 'Merging · front of train');
assert.strictEqual(stageTrackerTrainWord(second), 'In merge train · 2nd of 4');
assert.strictEqual(stageTrackerTrainWord(third), 'Rebasing · attempt 1/2');
assert.strictEqual(stageTrackerTrainWord(fourth), 'Needs human decision · rebase failed twice');
assert.strictEqual(stageTrackerTrainWord(other), '');
for (const [n, w] of [[1, '1st'], [2, '2nd'], [3, '3rd'], [4, '4th'], [11, '11th'], [12, '12th'], [13, '13th'], [21, '21st'], [22, '22nd'], [111, '111th']]) assert.strictEqual(stageTrackerOrdinal(n), w, 'ordinal ' + n);
// The lit chevron carries the word: 080 for the members, the 085 detour for the exhausted one.
const lit = it => stageTrackerSegments(it, stages, now).find(s => s.state === 'active');
assert.strictEqual(lit(front).short, 'Merging · front of train');
assert.strictEqual(lit(second).short, 'In merge train · 2nd of 4');
assert.strictEqual(lit(third).short, 'Rebasing · attempt 1/2');
assert.strictEqual(lit(fourth).detour.text, 'Needs human decision · rebase failed twice');
assert.strictEqual(lit(other).short, 'Ready for auto merge');

// Tooltip: the queue ahead and the overlapping files.
const tip = stageTrackerTooltip(third, now);
assert.ok(tip.includes('Merge train: 4 PRs to v5 touch the same files; they merge one at a time, front first (front #15074)'), tip);
assert.ok(tip.includes('Position: 3rd of 4 · ahead: #15074, #15085'), tip);
assert.ok(tip.includes('Front merged; rebase attempt 1 of 2 in progress'), tip);
assert.ok(tip.includes('Overlapping files: stage_tracker.go, index.html'), tip);
assert.ok(stageTrackerTooltip(front, now).includes('Position: front — merges first'), 'front tip');
assert.ok(stageTrackerTooltip(fourth, now).includes('Rebase failed 2 of 2 times — a maintainer rebases, then merges'));
assert.ok(!stageTrackerTooltip(other, now).includes('Merge train'));

// Contiguity: a sorted list that interleaves the train is regrouped at the
// first member's slot, in merge order; other rows keep their order.
const sorted = [fourth, other, third, { repo: 'x/y', number: 1, kind: 'issue', stage: 50 }, front, second];
const grouped = stageTrackerTrainContiguous(sorted).map(it => it.number);
assert.deepStrictEqual(grouped, [15074, 15085, 15087, 15097, 15100, 1]);
// Header line before the run, once; rail classes on members, last closes it.
const rows = stageTrackerTrainRows(stageTrackerTrainContiguous(sorted), (it) => ({ key: String(it.number), html: '' }));
assert.deepStrictEqual(rows.map(r => r.key), ['train:hivecommons/hive|v5#15074', '15074', '15085', '15087', '15097', '15100', '1']);
assert.ok(rows[0].html.includes('⛓ merge train · 4 PRs · front #15074'), rows[0].html);
assert.ok(rows[0].html.includes('class="ham-train-chip"') && rows[0].html.includes('overlapping: stage_tracker.go, index.html'), rows[0].html);
assert.strictEqual(stageTrackerTrainHeaderText(front.merge_train), '⛓ merge train · 4 PRs · front #15074');
assert.strictEqual(stageTrackerTrainRowClass(front, null, second), ' ham-train ham-train-front');
assert.strictEqual(stageTrackerTrainRowClass(second, front, third), ' ham-train');
assert.strictEqual(stageTrackerTrainRowClass(fourth, third, other), ' ham-train ham-train-last');
assert.strictEqual(stageTrackerTrainRowClass(other, fourth, null), '');
// The grouped view's {group}/{item} wrappers are read through.
const wrapped = stageTrackerTrainRows([{ group: { key: 'g' } }, { group: { key: 'g' }, item: front }, { group: { key: 'g' }, item: second }], (e) => ({ key: e.item ? String(e.item.number) : e.group.key, html: '' }));
assert.deepStrictEqual(wrapped.map(r => r.key), ['g', 'train:hivecommons/hive|v5#15074', '15074', '15085']);
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
