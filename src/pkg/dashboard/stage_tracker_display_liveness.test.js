// Display-mode liveness (hivecommons/hive#15930): a display board dims and
// says "Stale: last update N ago" once the event stream has been silent for
// more than two poll intervals or the payload is older than two cache TTLs,
// and a display that cannot reconnect reloads itself on a capped backoff.
// The helpers are lifted by marker out of index.html so this runs against
// the shipped source.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
const m = html.match(/\/\/ tracker display liveness helpers begin\n([\s\S]*?)\/\/ tracker display liveness helpers end/);
assert.ok(m, 'tracker display liveness helpers not found');
const h = new Function(m[1] + '; return { stageTrackerDisplayStale, stageTrackerDisplayReloadDelayMs, stageTrackerDisplayRollReload, stageTrackerDisplayAgo, stageTrackerDisplayStaleText };')();

const POLL = 30000;
const TTL = 30000;
const NOW = 10_000_000;

test('fresh: a recent frame and a recent payload are not stale', () => {
  assert.deepStrictEqual(h.stageTrackerDisplayStale(NOW, NOW - 1000, POLL, NOW - 5000, TTL), { stale: false, sinceMs: 0, reason: '' });
  // Exactly two intervals is still live; the threshold is "more than".
  assert.strictEqual(h.stageTrackerDisplayStale(NOW, NOW - 2 * POLL, POLL, NOW - 2 * TTL, TTL).stale, false);
});

test('SSE silent for more than two poll intervals is stale, since the last frame', () => {
  const last = NOW - 2 * POLL - 1;
  assert.deepStrictEqual(h.stageTrackerDisplayStale(NOW, last, POLL, NOW - 1000, TTL), { stale: true, sinceMs: last, reason: 'sse' });
});

test('a payload older than two cache TTLs is stale, since generated_at', () => {
  const gen = NOW - 4 * 60 * 1000;
  assert.deepStrictEqual(h.stageTrackerDisplayStale(NOW, NOW - 1000, POLL, gen, TTL), { stale: true, sinceMs: gen, reason: 'ttl' });
  // No TTL in the payload: the payload age judges nothing.
  assert.strictEqual(h.stageTrackerDisplayStale(NOW, NOW - 1000, POLL, gen, 0).stale, false);
});

test('both quiet: stale, since the newer of the two', () => {
  const last = NOW - 5 * 60 * 1000, gen = NOW - 4 * 60 * 1000;
  assert.deepStrictEqual(h.stageTrackerDisplayStale(NOW, last, POLL, gen, TTL), { stale: true, sinceMs: gen, reason: 'both' });
});

test('never connected and no payload yet: nothing to judge, not stale', () => {
  assert.deepStrictEqual(h.stageTrackerDisplayStale(NOW, 0, POLL, 0, TTL), { stale: false, sinceMs: 0, reason: '' });
  assert.strictEqual(h.stageTrackerDisplayStale(NOW, undefined, POLL, NaN, TTL).stale, false);
  // The caller judges a never-connected stream from page load instead.
  const loaded = NOW - 3 * POLL;
  assert.strictEqual(h.stageTrackerDisplayStale(NOW, loaded, POLL, 0, TTL).reason, 'sse');
});

test('recovery: a frame after a silence clears the stale state', () => {
  assert.strictEqual(h.stageTrackerDisplayStale(NOW, NOW - 10 * POLL, POLL, NOW - 1000, TTL).stale, true);
  assert.strictEqual(h.stageTrackerDisplayStale(NOW, NOW, POLL, NOW - 1000, TTL).stale, false);
});

test('banner text reads "Stale: last update N ago"', () => {
  const st = h.stageTrackerDisplayStale(NOW, NOW - 4 * 60 * 1000, POLL, NOW - 1000, TTL);
  assert.strictEqual(h.stageTrackerDisplayStaleText(st, NOW), 'Stale: last update 4 m ago');
  assert.strictEqual(h.stageTrackerDisplayStaleText({ stale: false }, NOW), '');
  assert.strictEqual(h.stageTrackerDisplayStaleText({ stale: true, sinceMs: 0 }, NOW), 'Stale: no update yet');
  assert.strictEqual(h.stageTrackerDisplayAgo(61 * 1000), '1 m');
  assert.strictEqual(h.stageTrackerDisplayAgo(45 * 1000), '45 s');
  assert.strictEqual(h.stageTrackerDisplayAgo(2 * 3600 * 1000), '2 h');
  assert.strictEqual(h.stageTrackerDisplayAgo(3 * 86400 * 1000), '3 d');
  assert.strictEqual(h.stageTrackerDisplayAgo(-5), '0 s');
});

test('reload backoff: none before 5 failed reconnects, then 30 s doubling per attempt, capped at 10 min', () => {
  for (let f = 0; f < 5; f++) assert.strictEqual(h.stageTrackerDisplayReloadDelayMs(f, 0), 0, f + ' failures');
  const schedule = [0, 1, 2, 3, 4, 5, 6, 50].map(a => h.stageTrackerDisplayReloadDelayMs(5, a));
  assert.deepStrictEqual(schedule, [30000, 60000, 120000, 240000, 480000, 600000, 600000, 600000]);
  assert.strictEqual(h.stageTrackerDisplayReloadDelayMs(12, 0), 30000, 'more failures do not shorten the wait');
  assert.strictEqual(h.stageTrackerDisplayReloadDelayMs(5, -3), 30000);
  assert.strictEqual(h.stageTrackerDisplayReloadDelayMs(undefined, 0), 0);
});

test('roll signal: reload once for a roll that finished after the page loaded', () => {
  const loaded = NOW - 60000;
  assert.strictEqual(h.stageTrackerDisplayRollReload({ source: 'roll', completedAt: NOW }, loaded), true);
  assert.strictEqual(h.stageTrackerDisplayRollReload({ source: 'roll', completedAt: loaded - 1 }, loaded), false, 'the reloaded page does not reload again');
  assert.strictEqual(h.stageTrackerDisplayRollReload({ source: 'roll', completedAt: 0 }, loaded), false, 'still rolling');
  assert.strictEqual(h.stageTrackerDisplayRollReload({ source: 'hub', completedAt: NOW }, loaded), false, 'only the roll signal');
  assert.strictEqual(h.stageTrackerDisplayRollReload(null, loaded), false);
});

test('wiring: display mode only — the tick, banner and reload are gated on stageTrackerDisplayMode', () => {
  assert.ok(html.includes("document.addEventListener('DOMContentLoaded', function() {\n      if (!stageTrackerDisplayMode()) return;\n      stageTrackerDisplayMountHead();"));
  assert.ok(html.includes("dashboardSetInterval('display-liveness', stageTrackerDisplayTick, STAGE_TRACKER_DISPLAY_TICK_MS);"));
  assert.ok(html.includes("function stageTrackerDisplayNoteReconnectFailed() {\n      if (!stageTrackerDisplayMode()) return;"));
  assert.ok(html.includes("if (typeof stageTrackerDisplayNoteReconnectFailed === 'function') stageTrackerDisplayNoteReconnectFailed();"), 'the SSE onerror counts failures');
  assert.ok(html.includes("if (typeof stageTrackerDisplayNoteFrame === 'function') stageTrackerDisplayNoteFrame();"), 'any frame resets them');
  assert.ok(html.includes("document.body.classList.toggle('display-stale', st.stale);"));
  for (const rule of [
    'body.display-mode.display-stale .ham-display-dot { color: var(--amber); }',
    'body.display-mode.display-stale #activity-monitor-panel, body.display-mode.display-stale #activity-monitor-meta,',
    '.ham-display-head, .ham-display-stale { display: none; }',
  ]) assert.ok(html.includes(rule), 'missing CSS rule: ' + rule);
  // The "updated … ago" chip keeps its behaviour everywhere.
  assert.ok(html.includes('const stale = failing || (ttlMs > 0 && ageMs > 2 * ttlMs);'));
  assert.ok(!html.includes("localStorage.setItem('hive-display"), 'nothing persisted in localStorage');
});
