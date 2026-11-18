'use strict';

// Governor PRs by model: hive review cost per model (#18717).

const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const indexHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'src', 'pkg', 'dashboard', 'static', 'index.html'), 'utf8');

function extractFunction(name) {
  const start = indexHtml.indexOf('function ' + name + '(');
  assert.notEqual(start, -1, `${name} function exists`);
  const brace = indexHtml.indexOf('{', start);
  let depth = 0;
  for (let i = brace; i < indexHtml.length; i++) {
    const ch = indexHtml[i];
    if (ch === '{') depth++;
    if (ch === '}') depth--;
    if (depth === 0) return indexHtml.slice(start, i + 1);
  }
  throw new Error(`unterminated ${name}`);
}

function load() {
  const context = { exports: {} };
  vm.runInNewContext([
    "function fmtUSD(n) { return n == null ? '—' : '$' + Number(n).toFixed(2); }",
    "function escapeHtml(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;'); }",
    extractFunction('governorPRModelHiveCostTip'),
    extractFunction('governorPRModelsHiveCostNote'),
    'exports.tip = governorPRModelHiveCostTip;',
    'exports.note = governorPRModelsHiveCostNote;',
  ].join('\n'), context);
  return context.exports;
}

const disclaimer = 'Estimated from token counts × published list prices. Not a bill.';
const data = {
  total: 4,
  hive_cost_coverage: { ready: true, window_hours: 336, unattributed_usd: 7.25, backend_unsupported_usd: null, disclaimer },
};

test('row tooltip shows the hive review cost and the disclaimer', () => {
  const { tip } = load();
  const out = tip({ hive_cost: { usd: 2, launches: 3, prs_with_cost: 2, usd_per_merged_pr: 1 } }, data);
  assert.match(out, /Hive review cost: \$2\.00 est\. over 2 PRs with launch records, 3 launches · \$1\.00 per merged PR/);
  assert.match(out, /launch-cost window: last 14d/);
  assert.ok(out.includes(disclaimer), 'disclaimer travels with the figure');
  assert.ok(!out.includes('not $0'), 'no null note for a measured figure');
});

test('null usd renders as a dash, never $0', () => {
  const { tip } = load();
  const out = tip({ hive_cost: { usd: null, launches: 1, prs_with_cost: 1, usd_per_merged_pr: null } }, data);
  assert.match(out, /Hive review cost: — est\./);
  assert.ok(out.includes('no attributed usage, not $0'));
  assert.ok(!out.includes('per merged PR'));
  assert.ok(!out.includes('$0.00'));
});

test('nothing renders when the snapshot is not ready or hive_cost is absent', () => {
  const { tip, note } = load();
  assert.equal(tip({}, data), '');
  assert.equal(tip({ hive_cost: { usd: 1 } }, { total: 1, hive_cost_coverage: { ready: false } }), '');
  assert.equal(note({ total: 1, hive_cost_coverage: { ready: false, disclaimer } }), '');
  assert.equal(note({ total: 1 }), '');
});

test('coverage note names the window, the unattributed spend and the disclaimer', () => {
  const { note } = load();
  const out = note(data);
  assert.match(out, /estimate over the last 14d of launch records/);
  assert.match(out, /not tied to any PR: \$7\.25 unattributed/);
  assert.ok(!out.includes('backends without a usage timeline'), 'null bucket is not shown as $0');
  assert.ok(out.includes(disclaimer));
});

test('the tile wires the hive cost into the row tooltip and the coverage notes', () => {
  for (const expected of [
    '${governorPRModelHiveCostTip(b, data)}',
    'const hiveCostNote = governorPRModelsHiveCostNote(data);',
    '${hiveCostNote}',
  ]) {
    assert.ok(indexHtml.includes(expected), `index.html missing ${expected}`);
  }
});
