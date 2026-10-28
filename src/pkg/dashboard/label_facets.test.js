// Hive activity monitor label facets (hivecommons/hive): stage/NNN chips
// first by number then every other label alphabetically, the "+N more"
// dialog data, and the synthetic Needs human / On hold facets (stage and
// modifier predicates, hivecommons/hive#15008). Run: node --test src/pkg/dashboard/*.test.js
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
const pick = (begin, end) => {
  const m = html.match(new RegExp('// ' + begin + '\\n([\\s\\S]*?)// ' + end));
  assert.ok(m, begin + ' not found');
  return m[1];
};
const constLine = name => {
  const m = html.match(new RegExp('^\\s*const ' + name + ' = .*$', 'm'));
  assert.ok(m, name + ' not found');
  return m[0];
};
const fnSrc = name => {
  const start = html.indexOf('function ' + name + '(');
  assert.ok(start >= 0, name + ' not found');
  let depth = 0;
  for (let i = html.indexOf('{', start); i < html.length; i++) {
    if (html[i] === '{') depth++;
    else if (html[i] === '}' && --depth === 0) return html.slice(start, i + 1);
  }
  throw new Error('unbalanced ' + name);
};
const src = [
  constLine('STAGE_TRACKER_PAGE_SIZES'), constLine('STAGE_TRACKER_DEFAULT_PAGE_SIZE'), constLine('STAGE_TRACKER_LABEL_CHIP_LIMIT'),
  constLine('STAGE_TRACKER_AGE_MS'), constLine('STAGE_TRACKER_LABEL_FAMILIES'), constLine('STAGE_TRACKER_SORTS'), constLine('STAGE_TRACKER_ACTED_FILTERS'),
  constLine('STAGE_TRACKER_NEEDS_HUMAN_STAGES'), constLine('STAGE_TRACKER_NEEDS_HUMAN_AUTHOR_STAGES'), constLine('STAGE_TRACKER_NEEDS_HUMAN_MODS'),
  fnSrc('stageTrackerDefaultFilter'), fnSrc('stageTrackerNormalizeFilter'), fnSrc('stageTrackerLabelFamily'),
  pick('label facet helpers begin', 'label facet helpers end'),
  pick('label facet model begin', 'label facet model end'),
].join('\n');
const h = new Function(src + '; return { STAGE_TRACKER_LABEL_CHIP_LIMIT, stageTrackerDefaultFilter, stageTrackerItemOnHold, stageTrackerItemNeedsHuman, stageTrackerLabelCompare, stageTrackerLabelFacets, stageTrackerLabelDialogData };')();
const F = over => Object.assign(h.stageTrackerDefaultFilter(), over);
const item = (n, labels) => ({ kind: 'issue', repo: 'a/b', number: n, labels });

test('comparator: stage/NNN numerically first, then alphabetical case-insensitive', () => {
  const labels = ['needs-kind', 'stage/100-merge-queue', 'Bug', 'stage/045-needs-decision', 'hold', 'stage/010-actionable', 'author/human', 'stage/085-needs-decision', 'needs-human'];
  assert.deepStrictEqual(labels.slice().sort(h.stageTrackerLabelCompare),
    ['stage/010-actionable', 'stage/045-needs-decision', 'stage/085-needs-decision', 'stage/100-merge-queue', 'author/human', 'Bug', 'hold', 'needs-human', 'needs-kind']);
  assert.ok(h.stageTrackerLabelCompare('stage/9-x', 'stage/10-y') < 0, 'numeric, not lexical');
  assert.ok(h.stageTrackerLabelCompare('stage/010-a', 'aaa') < 0, 'any stage label sorts before any other label');
  assert.strictEqual(h.stageTrackerLabelCompare('x', 'x'), 0);
});

test('facets are ordered, count every label and promote selected overflow labels into the row', () => {
  const items = [item(1, ['bug', 'stage/045-needs-decision']), item(2, ['stage/010-actionable', 'Bug']), item(3, ['hold'])];
  const fac = h.stageTrackerLabelFacets(items, F());
  assert.deepStrictEqual(fac.chips.map(c => [c.label, c.count]), [['stage/010-actionable', 1], ['stage/045-needs-decision', 1], ['bug', 2], ['hold', 1]]);
  assert.strictEqual(fac.hidden, 0);
  const limit = h.STAGE_TRACKER_LABEL_CHIP_LIMIT;
  const wide = Array.from({ length: limit + 10 }, (_, i) => item(100 + i, ['z-' + String(i).padStart(3, '0')]));
  const over = h.stageTrackerLabelFacets(wide, F({ labels: ['z-' + String(limit + 9).padStart(3, '0')] }));
  assert.strictEqual(over.all.length, limit + 10, 'every label is listed for the dialog');
  assert.strictEqual(over.chips.length, limit + 1, 'the selected overflow label joins the row');
  assert.strictEqual(over.hidden, 9);
  assert.ok(over.chips[over.chips.length - 1].on);
});

test('dialog data filters by case-insensitive substring and keeps selected labels', () => {
  const items = [item(1, ['stage/010-actionable', 'kind/bug']), item(2, ['kind/feature', 'hold'])];
  const fac = h.stageTrackerLabelFacets(items, F({ labels: ['hold'] }));
  const d = h.stageTrackerLabelDialogData(fac, 'KIND');
  assert.deepStrictEqual(d.rows.map(c => c.label), ['hold', 'kind/bug', 'kind/feature']);
  assert.deepStrictEqual([d.total, d.selected, d.query], [4, 1, 'kind']);
  assert.strictEqual(h.stageTrackerLabelDialogData(fac, '').rows.length, 4);
});

test('needs-human: stage/modifier predicate, labels alone never qualify (hivecommons/hive#15008)', () => {
  const it = (n, stage, mods, extra) => Object.assign({ kind: 'issue', repo: 'a/b', number: n, stage, modifiers: mods, labels: [] }, extra || {});
  assert.strictEqual(h.stageTrackerItemNeedsHuman(it(1, 40, [], { labels: ['needs-signal'] })), false, '040 PR in progress (#14976)');
  assert.strictEqual(h.stageTrackerItemNeedsHuman(it(2, 80, ['hold'])), false, 'ready for auto merge + hold (#7563)');
  assert.strictEqual(h.stageTrackerItemOnHold(it(2, 80, ['hold'])), true);
  assert.strictEqual(h.stageTrackerItemNeedsHuman(it(3, 45, ['hold'])), false, 'hold wins over a needs-decision stage');
  for (const st of [15, 45, 85, 90]) assert.strictEqual(h.stageTrackerItemNeedsHuman(it(4, st, [])), true, 'stage ' + st);
  assert.strictEqual(h.stageTrackerItemNeedsHuman(it(5, 71, [], { author_kind: 'human' })), true);
  assert.strictEqual(h.stageTrackerItemNeedsHuman(it(6, 71, [], { author_kind: 'clanker' })), false);
  assert.strictEqual(h.stageTrackerItemNeedsHuman(it(7, 72, [], { author_kind: 'human' })), true);
  for (const m of ['needs-human', 'needs-decision', 'unauthorized']) assert.strictEqual(h.stageTrackerItemNeedsHuman(it(8, 50, [m])), true, m);
  for (const m of ['stalled', 'blocked-on-pr', 'regressed', 'draft']) assert.strictEqual(h.stageTrackerItemNeedsHuman(it(9, 60, [m])), false, m);
  assert.strictEqual(h.stageTrackerItemNeedsHuman({ kind: 'pr', number: 10, labels: ['needs-human'] }), false, 'a label without the modifier is not enough');
  assert.strictEqual(h.stageTrackerItemNeedsHuman(null), false);
});

test('needs-human / on-hold facets: count equals what the filter predicate accepts', () => {
  const it = (n, stage, mods, extra) => Object.assign({ kind: 'issue', repo: 'a/b', number: n, stage, modifiers: mods, labels: [] }, extra || {});
  const items = [it(1, 45, []), it(2, 20, ['needs-human']), it(3, 71, [], { author_kind: 'human' }), it(4, 10, []), it(5, 80, ['hold']), it(6, 45, ['hold'])];
  const fac = h.stageTrackerLabelFacets(items, F());
  assert.deepStrictEqual([fac.needsHuman.count, fac.needsHuman.on, fac.onHold.count, fac.onHold.on], [3, false, 2, false]);
  assert.strictEqual(fac.needsHuman.count, items.filter(h.stageTrackerItemNeedsHuman).length);
  assert.deepStrictEqual(h.stageTrackerLabelFacets(items, F({ needsHuman: true })).needsHuman.on, true);
  assert.deepStrictEqual(h.stageTrackerLabelFacets(items, F({ onHold: true })).onHold.on, true);
  assert.deepStrictEqual([h.stageTrackerLabelFacets([], F()).needsHuman.count, h.stageTrackerLabelFacets([], F()).onHold.count], [0, 0], 'chips always exist');
  assert.strictEqual(h.stageTrackerLabelDialogData(h.stageTrackerLabelFacets(items, F({ needsHuman: true, onHold: true })), '').selected, 2);
});
