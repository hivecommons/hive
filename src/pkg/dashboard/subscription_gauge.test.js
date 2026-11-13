// Run the shipped subscription-gauge view-model against recorded
// /api/providers/headroom shapes. Pure function: no DOM, timers or network.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
// Values built inside the vm realm carry its prototypes; compare plain copies.
const plain = value => JSON.parse(JSON.stringify(value));

function loadModel() {
  const src = fs.readFileSync(path.join(__dirname, 'static-src/js/subscription-gauge.js'), 'utf8');
  const found = src.match(/function subscriptionGaugeModel\([^\n]*\) \{[\s\S]*?\n    \}/);
  assert.ok(found, 'subscriptionGaugeModel');
  const ctx = vm.createContext({});
  vm.runInContext(found[0] + '\nthis.subscriptionGaugeModel = subscriptionGaugeModel;', ctx);
  return ctx.subscriptionGaugeModel;
}
const model = loadModel();

const NOW = Date.parse('2026-11-13T18:12:00Z');
const CAPTURED = '2026-11-13T18:09:00Z';
const ZERO = '0001-01-01T00:00:00Z';
function limit(id, kind, left, mins, extra) {
  return Object.assign({ id, kind, percent_used: 100 - left, pct_remaining: left, resets_at: '2026-11-16T07:00:00Z', duration_mins: mins }, extra || {});
}
function resp(anthropic, others) {
  return { providers: (others || []).concat(anthropic ? [Object.assign({ provider: 'anthropic', available: true, captured_at: CAPTURED }, anthropic)] : []), enabled: false, readings: true };
}

test('weekly + 5-hour gives two dials, weekly first, needle on the lower', () => {
  const m = model(resp({ limits: [limit('session', 'session', 30, 300), limit('weekly_all', 'weekly', 62, 10080)] }), NOW);
  assert.equal(m.state, 'reading');
  assert.deepEqual(plain(m.dials.map(d => d.label)), ['Weekly', '5-hour']);
  assert.equal(m.needle.label, '5-hour');
  assert.equal(m.needle.shortName, '5-hour');
  assert.equal(m.needle.pctLeft, 30);
});

test('weekly + 5-hour with weekly lower puts the needle on weekly with no suffix', () => {
  const m = model(resp({ limits: [limit('weekly_all', 'weekly', 40, 10080), limit('session', 'session', 90, 300)] }), NOW);
  assert.equal(m.needle.label, 'Weekly');
  assert.equal(m.needle.shortName, '');
});

test('weekly-only reading gives exactly one dial and no placeholder', () => {
  const m = model(resp({ limits: [limit('weekly_all', 'weekly', 47, 10080)] }), NOW);
  assert.equal(m.dials.length, 1);
  assert.equal(m.dials[0].label, 'Weekly');
});

test('model-specific weekly is its own labelled dial and drives the needle when lowest', () => {
  const m = model(resp({ limits: [limit('weekly_scoped', 'weekly_scoped', 22, 10080), limit('weekly_all', 'weekly', 70, 10080), limit('session', 'session', 88, 300)] }), NOW);
  assert.deepEqual(plain(m.dials.map(d => d.label)), ['Weekly', 'Weekly (model-specific)', '5-hour']);
  assert.equal(m.needle.shortName, 'model');
  assert.equal(m.needle.pctLeft, 22);
  const named = model(resp({ limits: [limit('weekly_scoped', 'weekly_scoped', 50, 10080, { scope: { model: 'Opus' } })] }), NOW);
  assert.equal(named.dials[0].label, 'Weekly (Opus)');
});

test('ties keep the needle on the longer window', () => {
  const m = model(resp({ limits: [limit('session', 'session', 50, 300), limit('weekly_all', 'weekly', 50, 10080)] }), NOW);
  assert.equal(m.needle.label, 'Weekly');
});

test('status thresholds: normal above 25, warn at 25 or less, error at 10 or less', () => {
  const st = left => model(resp({ limits: [limit('weekly_all', 'weekly', left, 10080)] }), NOW).dials[0].status;
  assert.equal(st(26), 'ok');
  assert.equal(st(25), 'warn');
  assert.equal(st(11), 'warn');
  assert.equal(st(10), 'error');
  assert.equal(st(0), 'error');
});

test('percent left matches the reading to the whole percent and is clamped', () => {
  const m = model(resp({ limits: [limit('weekly_all', 'weekly', 61.6, 10080), limit('session', 'session', 140, 300)] }), NOW);
  assert.equal(m.dials[0].pctLeft, 62);
  assert.equal(m.dials[1].pctLeft, 100);
});

test('out of date when stale, or captured more than 15 minutes ago; age from captured_at', () => {
  const fresh = model(resp({ limits: [limit('weekly_all', 'weekly', 60, 10080)] }), NOW);
  assert.equal(fresh.outOfDate, false);
  assert.equal(fresh.ageMs, 3 * 60 * 1000);
  const stale = model(resp({ stale: true, limits: [limit('weekly_all', 'weekly', 60, 10080)] }), NOW);
  assert.equal(stale.outOfDate, true);
  const old = model(resp({ captured_at: '2026-11-13T17:56:00Z', limits: [limit('weekly_all', 'weekly', 60, 10080)] }), NOW);
  assert.equal(old.outOfDate, true);
  // The response's own updated_at never decides age.
  const r = resp({ limits: [limit('weekly_all', 'weekly', 60, 10080)] });
  r.updated_at = new Date(NOW).toISOString();
  assert.equal(model(r, NOW + 20 * 60 * 1000).outOfDate, true);
});

test('zero reset and capture times are treated as unknown', () => {
  const m = model(resp({ captured_at: ZERO, limits: [limit('weekly_all', 'weekly', 60, 10080, { resets_at: ZERO })] }), NOW);
  assert.equal(m.dials[0].resetsAt, '');
  assert.equal(m.capturedAt, '');
  assert.equal(m.ageMs, null);
  assert.equal(m.outOfDate, false);
});

test('every probe cause gives a no-reading state with a reason and no percentage', () => {
  for (const cause of ['not_installed', 'no_credentials', 'rate_limited', 'timeout', 'unrecognized_schema', 'probe_failed']) {
    const m = model(resp({ available: true, pct_remaining: 0, probe_error_cause: cause, probe_error: 'x' }), NOW);
    assert.equal(m.state, 'no-reading', cause);
    assert.equal(m.cause, cause);
    assert.ok(m.reason.length > 0, cause);
    assert.equal(m.dials, undefined, cause);
    assert.equal(m.needle, undefined, cause);
  }
  assert.equal(model(resp({ probe_error_cause: 'no_credentials' }), NOW).usesClaude, false);
  assert.equal(model(resp({ probe_error_cause: 'not_installed' }), NOW).usesClaude, false);
  assert.equal(model(resp({ probe_error_cause: 'rate_limited' }), NOW).usesClaude, true);
});

test('no anthropic reading means measurement is not running and Claude is not used', () => {
  const m = model(resp(null, [{ provider: 'google', limits: [limit('gemini-weekly', 'weekly', 54, 10080)] }]), NOW);
  assert.equal(m.state, 'no-reading');
  assert.equal(m.usesClaude, false);
  assert.match(m.reason, /not running/);
  assert.equal(model(null, NOW).state, 'no-reading');
  assert.equal(model({}, NOW).state, 'no-reading');
});

test('other providers never leak into the Claude dials', () => {
  const m = model(resp({ limits: [limit('weekly_all', 'weekly', 76, 10080)] }, [
    { provider: 'google', limits: [limit('gemini-argon-wtu-weekly', 'weekly', 1, 10080)] },
    { provider: 'openai', probe_error_cause: 'no_credentials' },
  ]), NOW);
  assert.equal(m.dials.length, 1);
  assert.equal(m.needle.pctLeft, 76);
});

test('plan name only when the reading carries one', () => {
  assert.equal(model(resp({ limits: [limit('weekly_all', 'weekly', 60, 10080)] }), NOW).planName, '');
  assert.equal(model(resp({ plan_type: 'max', limits: [limit('weekly_all', 'weekly', 60, 10080)] }), NOW).planName, 'max');
});

test('live shape from a v6 hive with rotation off (weekly + model-scoped, no 5-hour)', () => {
  const m = model(resp({ pct_remaining: 70, limits: [
    { id: 'weekly_all', kind: 'weekly', percent_used: 24, pct_remaining: 76, resets_at: '2026-11-16T07:00:00.183473Z', duration_mins: 10080 },
    { id: 'weekly_scoped', kind: 'weekly_scoped', percent_used: 30, pct_remaining: 70, resets_at: '2026-11-16T07:00:00.18362Z', duration_mins: 10080 },
  ] }), NOW);
  assert.deepEqual(plain(m.dials.map(d => [d.label, d.pctLeft])), [['Weekly', 76], ['Weekly (model-specific)', 70]]);
  assert.equal(m.needle.shortName, 'model');
  assert.equal(m.worstStatus, 'ok');
});

test('ccleft source: model-specific weekly arrives as kind weekly with id seven_day_<model>', () => {
  const m = model(resp({ limits: [
    { id: 'seven_day', kind: 'weekly', percent_used: 20, pct_remaining: 80, resets_at: '2026-11-16T07:00:00Z', duration_mins: 10080 },
    { id: 'seven_day_opus', kind: 'weekly', percent_used: 85, pct_remaining: 15, resets_at: '2026-11-16T07:00:00Z', duration_mins: 10080, scope: { scope: 'opus', binding: 'false' } },
    { id: 'five_hour', kind: 'five_hour', percent_used: 10, pct_remaining: 90, resets_at: '2026-11-13T23:00:00Z', duration_mins: 300 },
  ] }), NOW);
  assert.deepEqual(plain(m.dials.map(d => d.label)), ['Weekly', 'Weekly (opus)', '5-hour']);
  assert.equal(m.needle.shortName, 'model');
  assert.equal(m.needle.pctLeft, 15);
  assert.equal(m.worstStatus, 'warn');
  const bare = model(resp({ limits: [{ id: 'seven_day_scoped', kind: 'weekly', pct_remaining: 40, duration_mins: 10080, scope: { scope: 'scoped', binding: 'false' } }] }), NOW);
  assert.equal(bare.dials[0].label, 'Weekly (model-specific)');
});

test('credit pools and balances are not drawn as plan limits', () => {
  const m = model(resp({ limits: [
    limit('seven_day', 'weekly', 60, 10080),
    { id: 'extra_usage', kind: 'credits', pct_remaining: 5, duration_mins: 0 },
  ] }), NOW);
  assert.equal(m.dials.length, 1);
  assert.equal(m.needle.pctLeft, 60);
});
