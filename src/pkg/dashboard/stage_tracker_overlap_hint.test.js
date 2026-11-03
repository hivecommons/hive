// hive's own page, 2026-10-29 20:00 EDT: "⛁ merge train · 2 PRs · 1st:
// #15637 — overlapping: sidebar.html, index.html" under #15637 and #15633
// while both chevrons read CI / Review. The merge train is a merge-queue
// concept: a PR short of 080 carries `overlap: {behind, with, files, note}`
// and no `merge_train` — one muted "overlaps #15637" badge on the row, no
// header, no rail, no ordinal. The helpers are lifted out of index.html so
// this runs against the shipped source.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');

function pick(re, what) {
  const m = html.match(re);
  assert.ok(m, what + ' not found in index.html');
  return m[0];
}
const fn = name => pick(new RegExp('function ' + name + '\\([^)]*\\) \\{[\\s\\S]*?\\n    \\}\\n'), name);

const src = [
  "function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/\"/g, '&quot;'); }",
  "const STAGE_TRACKER_LEFT_TRAIN_NOTE = 'left the merge train while being fixed; rejoins when green';",
  "const STAGE_TRACKER_TRAIN_STATE_FRONT = 'front', STAGE_TRACKER_TRAIN_STATE_REBASING = 'rebasing', STAGE_TRACKER_TRAIN_STATE_EXHAUSTED = 'rebase-exhausted';",
  "const STAGE_TRACKER_TERMINAL = 110;",
  pick(/const STAGE_TRACKER_OVERLAP_CHIP_LEAD = '[^']*';/, 'STAGE_TRACKER_OVERLAP_CHIP_LEAD'),
  "function stageTrackerOrdinal(n) { return String(n); }",
  fn('stageTrackerTrain'),
  fn('stageTrackerLeftTrainNote'),
  fn('stageTrackerOverlap'),
  fn('stageTrackerOverlapBadgeText'),
  fn('stageTrackerOverlapTip'),
  fn('stageTrackerOverlapBadgeHtml'),
  fn('stageTrackerTrainLines'),
  fn('stageTrackerRowTrain'),
  fn('stageTrackerTrainKey'),
].join('\n');

const api = new Function(src + '; return { badgeText: stageTrackerOverlapBadgeText, tip: stageTrackerOverlapTip, badgeHtml: stageTrackerOverlapBadgeHtml, lines: stageTrackerTrainLines, trainKey: stageTrackerTrainKey };')();

// The two records as /api/stage-tracker now emits them. The chip names the
// train and the condition (#15761: a CI / Review member joins behind the
// ready front when it goes green; a pair with no ready member has a
// provisional front) but never a Merge-queue ordinal or rail.
const files = ['sidebar.html', 'index.html'];
const rider15633 = { repo: 'hivecommons/hive', number: 15633, kind: 'pr', stage: 50, modifiers: [], caption: 'Review swarm requested; waiting for a verdict on the current head',
  overlap: { behind: 15637, with: [15637], files, note: 'overlaps #15637 (sidebar.html, index.html) — will queue behind it' } };
const front15637 = { repo: 'hivecommons/hive', number: 15637, kind: 'pr', stage: 50, modifiers: [], caption: 'Review swarm requested; waiting for a verdict on the current head',
  overlap: { with: [15633], files, note: 'overlaps #15633 (sidebar.html, index.html) — merges ahead of it once both are ready' } };

test('a CI / Review pair with overlapping files draws no train, only the hint', () => {
  for (const it of [rider15633, front15637]) {
    assert.strictEqual(api.trainKey(it), '', '#' + it.number + ' must group into no train');
  }
  assert.strictEqual(api.badgeText(rider15633), 'train · joins behind #15637 when green');
  assert.strictEqual(api.badgeText(front15637), 'train · 1st once green');
  assert.strictEqual(api.tip(rider15633), 'overlaps #15637 (sidebar.html, index.html) — will queue behind it');
  const badge = api.badgeHtml(rider15633);
  assert.match(badge, /class="ham-badge ham-badge-overlap"/);
  assert.match(badge, />train · joins behind #15637 when green</);
  assert.match(badge, /title="Not in the merge queue yet: overlaps #15637 \(sidebar.html, index.html\) — will queue behind it"/);
  assert.ok(!/merge train|1st|2nd|rail/.test(badge), badge);
  assert.deepStrictEqual(api.lines(rider15633), ['Not in the merge queue yet: overlaps #15637 (sidebar.html, index.html) — will queue behind it']);
});

test('the hint yields to a train, a left-train note, and nothing', () => {
  const riding = Object.assign({}, rider15633, { stage: 80, merge_train: { id: 'v5#15637', state: 'queued-behind', front: 15637, position: 2, size: 2 } });
  assert.strictEqual(api.badgeText(riding), '');
  assert.strictEqual(api.badgeHtml(riding), '');
  const left = { repo: 'hivecommons/hive', number: 15405, kind: 'pr', stage: 70, left_train: { why: 'fixing', note: 'left the merge train while being fixed; rejoins when green' } };
  assert.strictEqual(api.badgeText(left), '');
  assert.deepStrictEqual(api.lines(left), ['Merge train: left the merge train while being fixed; rejoins when green']);
  assert.strictEqual(api.badgeText({ kind: 'pr', number: 1, stage: 50 }), '');
  assert.deepStrictEqual(api.lines({ kind: 'pr', number: 1, stage: 50 }), []);
  // An older payload without the note still reads the same sentence.
  const bare = { kind: 'pr', number: 2, stage: 50, overlap: { behind: 7, with: [7], files: ['a.go'] } };
  assert.strictEqual(api.tip(bare), 'overlaps #7 (a.go) — joins the merge train behind it when green');
  const bareFront = { kind: 'pr', number: 7, stage: 50, overlap: { with: [2, 3], files: ['a.go'] } };
  assert.strictEqual(api.badgeText(bareFront), 'train · 1st once green');
  assert.strictEqual(api.tip(bareFront), 'overlaps #2, #3 (a.go) — no member of the train is merge-ready yet; provisionally 1st, merges ahead of them if it is green first');
});
