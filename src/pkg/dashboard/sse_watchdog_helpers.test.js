const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
const m = html.match(/\/\/ sse watchdog helpers begin\n([\s\S]*?)\/\/ sse watchdog helpers end/);
assert.ok(m, 'sse watchdog helpers not found');
const h = new Function(m[1] + '; return { sseStreamIdle };')();

const IDLE = 90000;

test('not idle before the stream has ever connected', () => {
  assert.strictEqual(h.sseStreamIdle(1_000_000, 0, 0, IDLE), false);
  assert.strictEqual(h.sseStreamIdle(1_000_000, undefined, null, IDLE), false);
});

test('a recent frame keeps the stream live', () => {
  assert.strictEqual(h.sseStreamIdle(100_000, 100_000 - IDLE, 0, IDLE), false);
  assert.strictEqual(h.sseStreamIdle(100_000, 100_000 - 1000, 0, IDLE), false);
});

test('silence past the idle window is idle', () => {
  assert.strictEqual(h.sseStreamIdle(200_000, 200_000 - IDLE - 1, 0, IDLE), true);
  // The frozen-tab incident: last frame hours ago.
  assert.strictEqual(h.sseStreamIdle(Date.now(), Date.now() - 3 * 3600 * 1000, 0, IDLE), true);
});

test('a connection that never produced a frame is judged from when it opened', () => {
  const opened = 500_000;
  assert.strictEqual(h.sseStreamIdle(opened + IDLE, 0, opened, IDLE), false);
  assert.strictEqual(h.sseStreamIdle(opened + IDLE + 1, 0, opened, IDLE), true);
});

test('reconnecting resets the clock even if the last frame is old', () => {
  const now = 900_000;
  assert.strictEqual(h.sseStreamIdle(now, now - 10 * IDLE, now - 1000, IDLE), false);
});
