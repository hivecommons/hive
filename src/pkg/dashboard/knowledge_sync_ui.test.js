// Knowledge connector live sync progress: the pure helpers that turn one
// status row (`progress` block from /api/config/knowledge/connectors/status)
// into the Integrations card bar and the Knowledge header indicator. Helpers
// are cut out of the generated dashboard so the tests run the shipped code.
// Run: node --test src/pkg/dashboard/*.test.js
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
const start = html.indexOf('// ── Connector sync progress (live) ──');
const end = html.indexOf('// ── Connector sync progress end ──');
assert.ok(start >= 0 && end > start, 'connector sync progress helper block not found in index.html');
const names = ['KC_SYNC_POLL_MS', 'kcProgressActive', 'kcProgressStartMs', 'kcElapsedText', 'kcProgressCount', 'kcProgressLabel',
  'kcProgressHtml', 'kcProgressFailedHtml', 'kcSyncIndicatorText'];
const escapeHtml = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
const h = new Function('escapeHtml', html.slice(start, end) + '\nreturn {' + names.join(',') + '};')(escapeHtml);

const T0 = Date.parse('2026-11-14T09:00:00Z');
const row = (progress, extra) => Object.assign({ name: 'plaud', status: 'syncing', progress }, extra || {});

test('poll cadence is 2-3 seconds', () => {
  assert.ok(h.KC_SYNC_POLL_MS >= 2000 && h.KC_SYNC_POLL_MS <= 3000);
});

test('a row is active from the running flag or an active phase, never from done/failed/idle', () => {
  assert.strictEqual(h.kcProgressActive(null), false);
  assert.strictEqual(h.kcProgressActive({}), false);
  assert.strictEqual(h.kcProgressActive({ running: true }), true);
  for (const phase of ['listing', 'fetching', 'writing']) assert.strictEqual(h.kcProgressActive({ progress: { phase } }), true, phase);
  for (const phase of ['idle', 'done', 'failed', '']) assert.strictEqual(h.kcProgressActive({ progress: { phase } }), false, phase);
});

test('determinate bar with "fetching 12 of 200 · title" and elapsed time', () => {
  const out = h.kcProgressHtml(row({ phase: 'fetching', items_total: 200, items_done: 12, current: 'Drive <home>', started_at: '2026-11-14T09:00:00Z' }), T0 + 95000);
  assert.match(out, /<progress class="kc-progress-bar" max="200" value="12"><\/progress>/);
  assert.match(out, /fetching 12 of 200 · Drive &lt;home&gt;/);
  assert.match(out, /kc-progress-elapsed[^>]*>1m 35s</);
  assert.match(out, /data-phase="fetching"/);
});

test('indeterminate bar while listing, before the total is known', () => {
  const out = h.kcProgressHtml(row({ phase: 'listing', started_at: '2026-11-14T09:00:00Z' }), T0 + 4000);
  assert.match(out, /<progress class="kc-progress-bar"><\/progress>/);
  assert.doesNotMatch(out, /max=/);
  assert.match(out, /listing items…/);
  assert.match(out, />4s</);
});

test('done items are clamped to the total and a Go zero time never yields an elapsed clock', () => {
  const out = h.kcProgressHtml(row({ phase: 'writing', items_total: 3, items_done: 5, started_at: '0001-01-01T00:00:00Z' }), T0);
  assert.match(out, /max="3" value="3"/);
  assert.doesNotMatch(out, /kc-progress-elapsed/);
  assert.strictEqual(h.kcProgressStartMs({ started_at: '0001-01-01T00:00:00Z' }), 0);
  assert.strictEqual(h.kcProgressStartMs({}), 0);
  assert.strictEqual(h.kcElapsedText(0, T0), '');
  assert.strictEqual(h.kcElapsedText(T0, T0 + 2 * 3600 * 1000 + 5 * 60 * 1000), '2h 5m');
});

test('nothing is drawn once the sync is done, and a failed sync shows its error inline', () => {
  assert.strictEqual(h.kcProgressHtml(row({ phase: 'done', items_total: 200, items_done: 200 })), '');
  assert.strictEqual(h.kcProgressHtml(row({ phase: 'idle' }, { status: 'ok' })), '');
  const failed = h.kcProgressFailedHtml(row({ phase: 'failed', last_error: 'plaud: not connected' }, { status: 'error' }));
  assert.match(failed, /kc-error/);
  assert.match(failed, /sync failed: plaud: not connected/);
  // Same text already on the row's last_error line: not repeated.
  assert.strictEqual(h.kcProgressFailedHtml(row({ phase: 'failed', last_error: 'boom' }, { last_error: 'boom' })), '');
  assert.strictEqual(h.kcProgressFailedHtml(row({ phase: 'fetching' })), '');
});

test('long current titles are truncated on the card', () => {
  const title = 'x'.repeat(120);
  const label = h.kcProgressLabel({ phase: 'fetching', items_done: 1, items_total: 2, current: title });
  assert.ok(label.length < title.length, label);
  assert.match(label, /…$/);
});

test('header indicator names the syncing connector with its count and hides when idle', () => {
  assert.strictEqual(h.kcSyncIndicatorText([]), '');
  assert.strictEqual(h.kcSyncIndicatorText(null), '');
  assert.strictEqual(h.kcSyncIndicatorText([row({ phase: 'done' }, { status: 'ok' })]), '');
  assert.strictEqual(h.kcSyncIndicatorText([row({ phase: 'fetching', items_total: 200, items_done: 12 })]), 'syncing: plaud 12/200');
  assert.strictEqual(h.kcSyncIndicatorText([row({ phase: 'listing' })]), 'syncing: plaud');
  assert.strictEqual(h.kcSyncIndicatorText([{ name: 'wiki', running: true, progress: { phase: 'fetching', items_done: 7 } }]), 'syncing: wiki 7');
  assert.strictEqual(h.kcSyncIndicatorText([row({ phase: 'fetching', items_total: 10, items_done: 1 }), { name: 'wiki', running: true }]), 'syncing: plaud 1/10 +1');
});
