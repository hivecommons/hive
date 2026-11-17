// The kick failure toast carries a docs link, and the health hover names it.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const src = fs.readFileSync(path.join(__dirname, 'static-src', 'js', 'tooltips-dialogs.js'), 'utf8');
const m = src.match(/const KICK_REFUSAL_DOCS_LABEL = [^\n]*\n\s*function kickFailureToast\([\s\S]*?\n    \}\n/);
assert.ok(m, 'kickFailureToast not found');

function build() {
  const calls = { dismissed: [], shown: [], opened: [] };
  const dismissToast = (...a) => calls.dismissed.push(a);
  const showToast = (msg, type, opts) => { calls.shown.push({ msg, type, opts }); return {}; };
  const window = { open: (...a) => calls.opened.push(a) };
  const fn = new Function('dismissToast', 'showToast', 'window', m[0] + '; return kickFailureToast;')(dismissToast, showToast, window);
  return { fn, calls };
}

test('renders a What does this mean? action that opens the docs anchor', () => {
  const { fn, calls } = build();
  const url = 'https://github.com/hivecommons/hive/blob/v5/src/docs/kick-refusals.md#paused';
  fn({}, 'agent a cannot be kicked: it is paused', url);
  assert.strictEqual(calls.shown.length, 1);
  assert.strictEqual(calls.shown[0].type, 'error');
  assert.strictEqual(calls.shown[0].opts.actionLabel, 'What does this mean?');
  assert.ok(calls.shown[0].msg.includes('it is paused'));
  calls.shown[0].opts.onAction();
  assert.strictEqual(calls.opened[0][0], url);
});

test('no link without a docs url, and non-https urls are ignored', () => {
  for (const bad of [undefined, '', 'javascript:alert(1)']) {
    const { fn, calls } = build();
    fn({}, 'boom', bad);
    assert.strictEqual(calls.shown.length, 0);
    assert.strictEqual(calls.dismissed[0][1], 'Kick failed: boom');
  }
});

test('shipped index.html carries the toast helper and the health hover link', () => {
  const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
  assert.ok(html.includes('function kickFailureToast('));
  assert.ok(html.includes("what this means: ' + c.docs_url"));
});
