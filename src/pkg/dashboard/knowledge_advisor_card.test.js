// Knowledge Advisor card (#18897): the pure renderer for the
// /api/knowledge/advisor/summary payload, cut out of the generated dashboard
// so the test runs the shipped code.
// Run: node --test src/pkg/dashboard/*.test.js
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
const start = html.indexOf('// ── Advisor card (#18897) ──');
const end = html.indexOf('// ── Advisor card end ──');
assert.ok(start >= 0 && end > start, 'advisor card block not found in index.html');
const escapeHtml = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
const h = new Function('escapeHtml', 'document', html.slice(start, end) + '\nreturn {kbAdvisorCardHTML, kbAdvisorNum};')(escapeHtml, {});

const payload = {
  window: { decisions: 5, first_decision: '2026-11-01T10:00:00Z', last_decision: '2026-11-12T11:00:00Z', rank_mode_start: '2026-11-10T10:00:00Z' },
  settings: { enabled: true, mode: 'rank', model: 'small-model', holdout_percent: 20, max_calls_per_day: 300, max_calls_per_agent_hour: 6 },
  counts: { by_mode: { rank: 4, shadow: 1 }, by_arm: { deliver: 2, holdout: 2, shadow: 1 }, by_outcome: { delivered: 2, 'held-out': 2, gated: 1 } },
  arms: [{ arm: 'deliver', beads: 1, mean_fix_attempts: 1, mean_ci_failure_count: 0, mean_kicks_received: 2 }, { arm: 'holdout', beads: 1, mean_fix_attempts: 4, mean_ci_failure_count: 1.5, mean_kicks_received: 2 }],
  mixed: { arm: 'mixed', beads: 1, mean_fix_attempts: 2, mean_ci_failure_count: 0, mean_kicks_received: 2 },
  none: null,
  cost: { latency_samples: 2, mean_latency_ms: 200, token_samples: 2, mean_prompt_tokens: 300, mean_completion_tokens: 20 },
  small_n: true,
  caveat: 'Small sample: <b>few</b> beads.',
};

test('renders settings, counts, per-arm means, mixed separately, cost and the escaped caveat', () => {
  const out = h.kbAdvisorCardHTML(payload, '', false);
  assert.match(out, /mode rank · holdout 20% · model small-model · budget 300\/day, 6\/agent\/hour/);
  assert.match(out, /5 decisions from 2026-11-01T10:00:00Z to 2026-11-12T11:00:00Z · rank mode since 2026-11-10T10:00:00Z/);
  assert.match(out, /<td>holdout<\/td><td>1<\/td><td>4\.00<\/td><td>1\.50<\/td><td>2\.00<\/td>/);
  assert.match(out, /mixed <span[^>]*>in more than one arm; not comparable/);
  assert.doesNotMatch(out, /no advisor decision/);
  assert.match(out, /Mean model latency 200 ms \(2 calls\) · mean tokens 300 prompt \/ 20 completion/);
  assert.match(out, /Small sample: &lt;b&gt;few&lt;\/b&gt; beads\./);
});

test('no caveat line when the payload carries none', () => {
  const out = h.kbAdvisorCardHTML(Object.assign({}, payload, { caveat: '', small_n: false }), '', false);
  assert.doesNotMatch(out, /kb-notice/);
});

test('empty summary, error and loading states do not throw', () => {
  const empty = h.kbAdvisorCardHTML({ window: {}, settings: {}, counts: {}, arms: [], cost: {} }, '', false);
  assert.match(empty, /0 decisions/);
  assert.match(empty, /No closed beads with a closed PR yet/);
  assert.match(empty, /mode off/);
  assert.match(h.kbAdvisorCardHTML(null, 'boom <x>', false), /boom &lt;x&gt;/);
  assert.match(h.kbAdvisorCardHTML(null, '', true), /Loading/);
});
