package dashboard

import "testing"

// Two trains on one page (owner, hive v5: "2 PRs · 1st: #15567 ·
// overlapping: main.go" over "2 PRs · 1st: #15537 · overlapping:
// stage_tracker.go" — "how are there 2 'merge queued 1st' and 2 'merge
// queued 2nd'?"): the queue word always carries the queue length ("1st of
// 2"), and once more than one train is drawn it ends with the train's tag
// ("· train main.go") while the hover names the train; one train carries
// no tag. The header and every member row of a train share one rail
// accent class so the trains tell apart at a glance.
func TestStageTrackerTrainTagOnTwoTrains(t *testing.T) {
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
		jsFunc(t, html, "stageTrackerMergeGateWordHtml")+"\n"+
		jsFunc(t, html, "stageTrackerGateCueHtml")+"\n"+
		jsFunc(t, html, "stageTrackerRowTrain")+"\n"+
		jsFunc(t, html, "stageTrackerTrainKey")+"\n"+
		jsFunc(t, html, "stageTrackerTrainMissing")+"\n"+
		jsFunc(t, html, "stageTrackerTrainHeaderText")+"\n"+
		jsFunc(t, html, "stageTrackerTrainHeaderHtml")+"\n"+
		jsFunc(t, html, "stageTrackerTrainRowClass")+"\n"+
		jsFunc(t, html, "stageTrackerTrainRows")+"\n"+`
const now = Date.parse('2026-10-30T00:30:00Z');
const mk = (n, train, pos, size, files, head, stage) => ({ repo: 'hivecommons/hive', number: n, kind: 'pr', title: 'PR ' + n, stage: stage || 80, modifiers: [],
  merge_gate: { state: pos === 1 ? 'front' : 'queued-behind', reason: 'in the merge queue', last_sweep_at: '2026-10-30T00:29:30Z' },
  merge_train: { id: train, front: head, head_number: head, size, length: size, position: pos, state: pos === 1 ? 'front' : 'queued-behind', ahead: pos === 1 ? [] : [head], overlap_files: files, head_state: 'ready', head_reason: '' } });
// The owner's page: two trains of two.
const a1 = mk(15567, 'v5#15567', 1, 2, ['cmd/hive/main.go'], 15567);
const a2 = mk(15582, 'v5#15567', 2, 2, ['cmd/hive/main.go'], 15567);
const b1 = mk(15537, 'v5#15537', 1, 2, ['pkg/dashboard/stage_tracker.go'], 15537);
const b2 = mk(15574, 'v5#15537', 2, 2, ['pkg/dashboard/stage_tracker.go'], 15537);
const two = stageTrackerTrainIndex([a1, a2, b1, b2]);
assert.strictEqual(two.count, 2);
assert.deepStrictEqual(two.index, { 'hivecommons/hive|v5#15567': 0, 'hivecommons/hive|v5#15537': 1 });
const one = stageTrackerTrainIndex([a1, a2, { repo: 'x/y', number: 1, kind: 'issue', stage: 50 }]);
assert.strictEqual(one.count, 1);
assert.strictEqual(stageTrackerTrainIndex([]).count, 0);
// The grouped view's wrappers are read through.
assert.strictEqual(stageTrackerTrainIndex([{ group: { key: 'g' } }, { group: { key: 'g' }, item: a1 }, { item: b2 }]).count, 2);

// "1st of 2" always; the tag only past one train.
assert.strictEqual(stageTrackerTrainWord(a1), 'Merge queued · 1st of 2');
assert.strictEqual(stageTrackerTrainWord(a1, 1), 'Merge queued · 1st of 2');
assert.strictEqual(stageTrackerTrainWord(a1, 2), 'Merge queued · 1st of 2 · train main.go');
assert.strictEqual(stageTrackerTrainWord(a2, 2), 'Merge queued · 2nd of 2 · train main.go');
assert.strictEqual(stageTrackerTrainWord(b1, 2), 'Merge queued · 1st of 2 · train stage_tracker.go');
assert.strictEqual(stageTrackerTrainWord(b2, 2), 'Merge queued · 2nd of 2 · train stage_tracker.go');
// Two trains never yield the same chevron word for the same place.
assert.notStrictEqual(stageTrackerTrainWord(a1, two.count), stageTrackerTrainWord(b1, two.count));
assert.notStrictEqual(stageTrackerTrainWord(a2, two.count), stageTrackerTrainWord(b2, two.count));
// No overlap list: the head names the train.
const bare = mk(15600, 'v5#15600', 1, 3, [], 15600);
assert.strictEqual(stageTrackerTrainTag(bare.merge_train), '#15600');
assert.strictEqual(stageTrackerTrainWord(bare, 2), 'Merge queued · 1st of 3 · train #15600');
assert.strictEqual(stageTrackerTrainTag({}), '');
// The gate chip reads the same word, and its hover names the train.
const chipA2 = stageTrackerMergeGateInfo(a2, now, 2);
assert.strictEqual(chipA2.word, 'Merge queued · 2nd of 2 · train main.go');
assert.ok(chipA2.tip.startsWith('Train of 2 PRs overlapping main.go; merges after #15567. 2nd of 2 in the merge queue · behind #15567'), chipA2.tip);
const chipB1 = stageTrackerMergeGateInfo(b1, now, 2);
assert.strictEqual(chipB1.word, 'Merge queued · 1st of 2 · train stage_tracker.go');
assert.ok(chipB1.tip.startsWith('Train of 2 PRs overlapping stage_tracker.go; merges first. 1st of 2 in the merge queue · merges next'), chipB1.tip);
assert.ok(!stageTrackerMergeGateInfo(a2, now, 1).tip.includes('Train of'), 'one train: no train sentence');
assert.ok(!stageTrackerMergeGateInfo(a2, now).tip.includes('Train of'), 'no count: no train sentence');
assert.strictEqual(stageTrackerMergeGateWordHtml(a2, now, 2).replace(/<[^>]+>/g, ''), 'Merge queued · 2nd of 2 · train main.go');
assert.ok(stageTrackerGateCueHtml(a2, now, 2).includes('Train of 2 PRs overlapping main.go'), stageTrackerGateCueHtml(a2, now, 2));
// A provisional slot (#15581) keeps blocker-first and still names its train.
const p1 = mk(15577, 'v5#15577', 1, 2, ['tracker-words.js'], 15577, 50);
p1.reason = 'required-check-pending:build-and-test';
assert.strictEqual(stageTrackerTrainWord(p1), 'CI / Review · queued 1st of 2 once ready');
assert.strictEqual(stageTrackerTrainWord(p1, 2), 'CI / Review · queued 1st of 2 once ready · train tracker-words.js');
const chipP1 = stageTrackerMergeGateInfo(p1, now, 2);
assert.strictEqual(chipP1.word, 'CI / Review');
assert.strictEqual(chipP1.detail, 'queued 1st of 2 once ready · train tracker-words.js');
assert.ok(chipP1.tip.includes('Train of 2 PRs overlapping tracker-words.js; merges first.'), chipP1.tip);
assert.ok(chipP1.tip.includes('merges first once it is ready'), chipP1.tip);

// Rail accent: header and member rows of one train share a class; the
// second train takes the next; the cycle wraps.
assert.strictEqual(stageTrackerTrainAccentClass(0), ' ham-train-k0');
assert.strictEqual(stageTrackerTrainAccentClass(1), ' ham-train-k1');
assert.strictEqual(stageTrackerTrainAccentClass(STAGE_TRACKER_TRAIN_ACCENTS), ' ham-train-k0');
assert.strictEqual(stageTrackerTrainAccentClass(undefined), '');
assert.strictEqual(stageTrackerTrainRowClass(a1, null, a2, 0), ' ham-train ham-train-k0 ham-train-front');
assert.strictEqual(stageTrackerTrainRowClass(a2, a1, b1, 0), ' ham-train ham-train-k0 ham-train-last');
assert.strictEqual(stageTrackerTrainRowClass(b1, a2, b2, 1), ' ham-train ham-train-k1 ham-train-front');
assert.strictEqual(stageTrackerTrainRowClass(b1, a2, b2), ' ham-train ham-train-front', 'no index: no accent class');
const rows = stageTrackerTrainRows([a1, a2, b1, b2], (it) => ({ key: String(it.number), html: '' }));
assert.deepStrictEqual(rows.map(r => r.key), ['train:hivecommons/hive|v5#15567', '15567', '15582', 'train:hivecommons/hive|v5#15537', '15537', '15574']);
assert.ok(rows[0].html.includes('class="ham-train-head ham-train-k0"'), rows[0].html);
assert.ok(rows[3].html.includes('class="ham-train-head ham-train-k1"'), rows[3].html);
assert.ok(rows[0].html.includes('overlapping: cmd/hive/main.go'), rows[0].html);
`)
}
