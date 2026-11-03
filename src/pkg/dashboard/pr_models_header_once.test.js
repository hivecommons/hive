// "PRs by model" renders column names (MERGED / 1ST PASS / PR RUNS / FAIL /
// NO SHIP) once, in a sticky header row carrying the ? tooltips, and plain
// numbers per row. Before this test the chip-stat eyebrow pattern repeated
// the five labels above every row's values. The narrow breakpoint restores
// labels via CSS attr(data-label), which is not visible text here. Also
// pins: unattributed ("unknown" model) rows sort last and are never ranked.
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

const src = [
  'let govPRModelsSort = "effectiveness"; let govPRModelsWindow = "7d"; let govPRModelsData = null;',
  'const escapeHtml = s => String(s).replace(/[&<>"\']/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;",\'"\':"&quot;","\'":"&#39;"}[c]));',
  'const dashboardSectionHelpMark = () => "";',
  'const formatFixTime = () => "";',
  pick(/function governorPRModelUnattributed\(b\) \{[\s\S]*?\n    \}\n/, 'governorPRModelUnattributed'),
  pick(/const GOV_PR_MODEL_COLUMNS = \[[\s\S]*?\n    \];\n/, 'GOV_PR_MODEL_COLUMNS'),
  pick(/function governorPRModelRows\(data\) \{[\s\S]*?\n    \}\n/, 'governorPRModelRows'),
  pick(/function renderGovernorPRModelsTile\(\) \{[\s\S]*?\n    \}\n\n    function renderGovernor\(/, 'renderGovernorPRModelsTile').replace(/\n\n    function renderGovernor\($/, ''),
].join('\n');
const lifted = new Function(src + '; return { render: data => { govPRModelsData = data; return renderGovernorPRModelsTile(); }, rows: governorPRModelRows, columns: GOV_PR_MODEL_COLUMNS };')();

const bucket = (model, backend, prs, merged, eff) => ({
  model, backend, backends: backend.split(', '), prs, merged, open: 0, closed_unmerged: prs - merged,
  rework: { first_pass_merge_rate: eff.first_pass_merge_rate || 0 },
  effectiveness: { eligible: !!eff.rank, merged_prs: merged, ...eff },
  sources: [{ model, backend, prs, merged }],
});

const data = {
  window: '7d', total: 60, min_merged_prs: 5,
  buckets: [
    bucket('unknown', 'codex', 20, 12, { unattributed: true, first_pass_merge_rate: 0.62 }),
    bucket('gpt-6-astra', 'codex', 6, 5, { rank: 3, score: 0.22, first_pass_merged: 5, first_pass_merge_rate: 1, failure_rate: 0.51, nothing_to_ship_rate: 0.21 }),
    bucket('claude-opus-5.5', 'claude, copilot', 22, 22, { rank: 1, score: 0.78, first_pass_merged: 21, first_pass_merge_rate: 0.95 }),
    bucket('claude-fable-5.1', 'copilot', 12, 12, { rank: 2, score: 0.6, first_pass_merged: 8, first_pass_merge_rate: 0.65 }),
  ],
};

test('column labels appear exactly once, in a header row with tooltips', () => {
  const out = lifted.render(data);
  const header = out.match(/<div class="gov-pr-model-row gov-pr-model-head"[\s\S]*?<\/div>\s*<\/div>\s*<\/div>/);
  assert.ok(header, 'sticky header row rendered');
  for (const [, label, help] of lifted.columns) {
    const re = new RegExp('>' + label.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '<span class="gov-pr-model-help"', 'g');
    assert.strictEqual((out.match(re) || []).length, 1, `${label} rendered once as header text`);
    assert.ok(header[0].includes(`title="${help.replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;').replace(/>/g, '&gt;')}"`) || header[0].includes(help.slice(0, 40)), `${label} tooltip lives in the header`);
  }
  // Ratchet: no per-row eyebrow <span>label</span> inside metric cells.
  const eyebrows = out.match(/<div class="gov-pr-model-metric"[^>]*><span>[^<]+<\/span>/g) || [];
  assert.deepStrictEqual(eyebrows, [], 'metric cells carry plain values, no repeated labels');
  // Each data row carries the label only as data-label (for the narrow breakpoint).
  const perRow = (out.match(/data-label="1st pass"/g) || []).length;
  assert.strictEqual(perRow, data.buckets.length, 'data-label present on every data row');
});

test('unattributed rows are listed last, muted, never ranked', () => {
  const rows = lifted.rows(data);
  assert.strictEqual(rows[rows.length - 1].model, 'unknown');
  assert.strictEqual(rows[0].model, 'claude-opus-5.5');
  const out = lifted.render(data);
  const idxUnknown = out.indexOf('gov-pr-model-row unattributed');
  const idxAstra = out.indexOf('gpt-6-astra');
  assert.ok(idxUnknown > idxAstra, 'unattributed row rendered after ranked rows');
  assert.ok(out.includes('class="gov-pr-model-rank unattributed"'), 'unattributed chip instead of a rank');
  assert.ok(!/#\d+<\/span>unknown/.test(out), 'unknown never gets a rank number');
});

test('raw model ids and backends surface in the row tooltip and chips', () => {
  const out = lifted.render(data);
  assert.ok(out.includes('Raw model ids folded into this row:'), 'tooltip lists raw ids');
  assert.strictEqual((out.match(/<span class="gov-pr-model-backend">claude<\/span>/g) || []).length, 1);
  assert.ok(out.includes('<span class="gov-pr-model-backend">copilot</span>'), 'each backend a chip');
  assert.ok(out.includes('Wilson lower bound'), 'tooltip states the rank formula');
});
