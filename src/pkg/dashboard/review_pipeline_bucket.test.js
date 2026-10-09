const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
const m = html.match(/function reviewPipelineBucket\([\s\S]*?\n    }\n/);
assert.ok(m, 'reviewPipelineBucket not found');
const bucket = new Function(m[0] + '; return reviewPipelineBucket;')();
const stages = [['unreviewed', 'Unreviewed'], ['approved', 'Approved']];

test('buckets cards per stage and keeps empty columns', () => {
  const cols = bucket([{ repo: 'a/b', stage: 'approved' }], {}, stages);
  assert.deepStrictEqual(cols.map(c => [c.stage, c.cards.length]), [['unreviewed', 0], ['approved', 1]]);
});

test('filters by repo, author kind and stage', () => {
  const cards = [
    { repo: 'a/b', stage: 'unreviewed', hive_authored: true },
    { repo: 'a/c', stage: 'unreviewed', hive_authored: false },
  ];
  assert.strictEqual(bucket(cards, { repo: 'a/c' }, stages)[0].cards.length, 1);
  assert.strictEqual(bucket(cards, { author: 'agent' }, stages)[0].cards[0].repo, 'a/b');
  assert.strictEqual(bucket(cards, { stage: 'approved' }, stages).length, 1);
});

test('unknown stages get their own column', () => {
  const cols = bucket([{ repo: 'a/b', stage: 'novel' }], {}, stages);
  assert.strictEqual(cols[cols.length - 1].stage, 'novel');
});
