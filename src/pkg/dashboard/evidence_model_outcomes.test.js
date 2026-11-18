// Evidence card: change failures per author model (hivecommons/hive#18720).
// Runs evOutcomesByModelHtml from the built index.html against a fixture
// shaped like GET /api/evidence/summary's outcomes_by_author_model.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');

function fn(name) {
  const re = new RegExp('\\n    (?:async )?function ' + name + '\\([^)]*\\) \\{\\n[\\s\\S]*?\\n    \\}\\n');
  const m = html.match(re);
  assert.ok(m, name + ' not found');
  return m[0];
}

function oneLine(name) {
  const m = html.match(new RegExp('\\n    function ' + name + '\\([^)]*\\) \\{[^\\n]*\\}\\n'));
  assert.ok(m, name + ' not found');
  return m[0];
}

function load() {
  const src = [
    fn('escapeHtml'),
    fn('evRate'),
    oneLine('evPct'),
    fn('evOutcomesByModelHtml'),
    'return evOutcomesByModelHtml;',
  ].join('\n');
  return new Function(src)();
}

const row = (n, merged, reverted, reopened, rate) => ({ n, merged, reverted_14d: reverted, issue_reopened_14d: reopened, change_failures: reverted + reopened, change_failure_rate: rate });

test('renders one row per model with n, merged, reverted, re-opened and rate', () => {
  const render = load();
  const out = render({
    'claude-opus-5.5': row(3, 2, 1, 0, 0.5),
    'claude-sonnet-5.5': row(1, 1, 0, 1, 1),
    human: row(1, 1, 1, 0, 1),
    unknown: row(1, 0, 0, 0, null),
    'gpt-6.1-terra': row(2, 2, 0, 0, 0),
  });
  assert.match(out, /<table class="ev-table ev-model-outcomes">/);
  assert.match(out, /<th>Author model<\/th><th>n<\/th><th>Merged<\/th><th>Reverted<\/th><th>Re-opened<\/th><th>Change failure<\/th>/);
  assert.match(out, /<tr><td>claude-opus-5\.5<\/td><td>3<\/td><td>2<\/td><td>1<\/td><td>0<\/td><td>50%<\/td><\/tr>/);
  assert.match(out, /<tr><td>claude-sonnet-5\.5<\/td><td>1<\/td><td>1<\/td><td>0<\/td><td>1<\/td><td>100%<\/td><\/tr>/);
  assert.match(out, /<tr><td>gpt-6\.1-terra<\/td><td>2<\/td><td>2<\/td><td>0<\/td><td>0<\/td><td>0%<\/td><\/tr>/);
  // No merges: no rate, a dash rather than 0%.
  assert.match(out, /<tr><td>unknown \(model not recorded\)<\/td><td>1<\/td><td>0<\/td><td>0<\/td><td>0<\/td><td>—<\/td><\/tr>/);
  assert.match(out, /<tr><td>human \(no model\)<\/td>/);
  // Biggest n first; human and unknown trail.
  const order = [...out.matchAll(/<tr><td>([^<]+)<\/td>/g)].map((m) => m[1]);
  assert.deepStrictEqual(order, ['claude-opus-5.5', 'gpt-6.1-terra', 'claude-sonnet-5.5', 'human (no model)', 'unknown (model not recorded)']);
  // Coverage caveat is visible next to the table.
  assert.match(out, /n counts only PRs with an evidence record/);
  assert.doesNotMatch(out, /NaN|undefined/);
});

test('escapes model ids and renders nothing for an absent or empty map', () => {
  const render = load();
  const out = render({ '<img src=x>': row(1, 1, 0, 0, 0) });
  assert.doesNotMatch(out, /<img/);
  assert.match(out, /&lt;img src=x&gt;/);
  assert.strictEqual(render(undefined), '');
  assert.strictEqual(render(null), '');
  assert.strictEqual(render({}), '');
  assert.strictEqual(render({ 'claude-opus-5.5': row(0, 0, 0, 0, null) }), '');
});
