// Hive activity monitor label facets (hivecommons/hive): stage/NNN chips
// first by number then every other label alphabetically, the "+N more"
// dialog data, and the synthetic Needs human facet over the payload's
// needs_human_labels set. Run: node --test src/pkg/dashboard/*.test.js
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
  constLine('STAGE_TRACKER_AGE_MS'), constLine('STAGE_TRACKER_LABEL_FAMILIES'), constLine('STAGE_TRACKER_SORTS'),
  fnSrc('stageTrackerDefaultFilter'), fnSrc('stageTrackerNormalizeFilter'), fnSrc('stageTrackerLabelFamily'),
  pick('label facet helpers begin', 'label facet helpers end'),
  pick('label facet model begin', 'label facet model end'),
].join('\n');
const h = new Function(src + '; return { STAGE_TRACKER_LABEL_CHIP_LIMIT, stageTrackerDefaultFilter, stageTrackerNeedsHumanLabels, stageTrackerLabelNeedsHuman, stageTrackerItemNeedsHuman, stageTrackerLabelCompare, stageTrackerLabelFacets, stageTrackerLabelDialogData };')();
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

test('needs-human patterns match new labels and the dialog lists covered ones', () => {
  const set = h.stageTrackerNeedsHumanLabels({ needs_human_labels: ['sentinel-alert'] });
  for (const l of ['needs-human-review', 'stage/099-needs-decision', 'ci-needs-author', 'sentinel-alert']) assert.strictEqual(h.stageTrackerLabelNeedsHuman(l, set), true, l);
  for (const l of ['stage/010-actionable', 'unneeds-humans']) assert.strictEqual(h.stageTrackerLabelNeedsHuman(l, set), false, l);
  const fac = h.stageTrackerLabelFacets([item(1, ['needs-human-review', 'stage/010-actionable']), item(2, ['sentinel-alert'])], F(), { needsHumanLabels: set });
  assert.deepStrictEqual(fac.needsHuman.covered.map(c => c.label), ['needs-human-review', 'sentinel-alert']);
});

test('needs-human set: normalised from the payload, prefix children match, count is distinct items', () => {
  const set = h.stageTrackerNeedsHumanLabels({ needs_human_labels: ['stage/045-needs-decision', 'stage/085-needs-decision', 'needs-decision', 'needs-direction', 'needs-spec', 'needs-signal', 'Needs-Human', 'hold', 'on-hold', 'hold/review', 'stage/071-needs-author', 'needs-triage', ''] });
  assert.strictEqual(set.length, 12);
  assert.ok(set.includes('needs-human') && set.includes('stage/071-needs-author') && set.includes('needs-triage'));
  assert.strictEqual(h.stageTrackerLabelNeedsHuman('needs-human/agent-ready', set), true);
  assert.strictEqual(h.stageTrackerLabelNeedsHuman('stage/010-actionable', set), false);
  assert.strictEqual(h.stageTrackerLabelNeedsHuman('holdover', set), false);
  const items = [
    item(1, ['stage/045-needs-decision', 'needs-human', 'hold']),
    item(2, ['needs-human/agent-ready']),
    item(3, ['stage/071-needs-author']),
    item(4, ['stage/010-actionable']),
  ];
  const fac = h.stageTrackerLabelFacets(items, F(), { needsHumanLabels: set });
  assert.deepStrictEqual([fac.needsHuman.count, fac.needsHuman.on], [3, false], 'distinct items, not the sum of label counts (5)');
  assert.deepStrictEqual(h.stageTrackerLabelFacets(items, F({ needsHuman: true }), { needsHumanLabels: set }).needsHuman.on, true);
  assert.strictEqual(h.stageTrackerLabelFacets(items, F()).needsHuman, null, 'no set, no chip');
  assert.strictEqual(h.stageTrackerItemNeedsHuman(items[3], set), false);
});
