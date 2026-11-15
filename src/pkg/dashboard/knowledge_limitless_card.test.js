// Knowledge Integrations card for the Limitless connector: the shared sign-in
// line and buttons carry the Limitless label, the help explains what the hive
// reads, failures get their own hint, and the connected card points at the
// Inbox.
// Run: node --test src/pkg/dashboard/*.test.js
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
const fnSrc = name => {
  const start = html.indexOf('function ' + name + '(');
  assert.ok(start >= 0, name + ' not found');
  let depth = 0;
  for (let i = html.indexOf('{', start); i < html.length; i++) {
    if (html[i] === '{') depth++;
    else if (html[i] === '}' && --depth === 0) return html.slice(start, i + 1);
  }
  throw new Error('unbalanced ' + name);
};
const declSrc = name => {
  const m = html.match(new RegExp('const ' + name + ' = [^\\n]+;'));
  assert.ok(m, name + ' not found');
  return m[0];
};

const escapeHtml = s => String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');

// Every helper the card calls is injected (KB_AUTH_TYPES, kbAuthLabel), or
// the sandbox throws ReferenceError.
const load = (connectors, auth) => new Function('_kcState', '_kbPlaudAuth', 'escapeHtml', 'setTimeout', 'kbPlaudAuthLoad',
  declSrc('KB_AUTH_TYPES') + '\n' +
  ['kbAuthLabel', 'kbPlaudAuthLine', 'kbPlaudAuthButtons', 'kbLimitlessHelpHtml'].map(fnSrc).join('\n') +
  '\nreturn { kbAuthLabel, kbPlaudAuthLine, kbPlaudAuthButtons, kbLimitlessHelpHtml };')(
  { connectors }, auth || {}, escapeHtml, () => {}, () => {});

const conn = {"name": "c1", "type": "limitless", "scope": {"starred_only": "true"}, "auth": {"env": "LIMITLESS_API_KEY"}};

test('limitless sign-in line and buttons carry the Limitless label', () => {
  const h = load([conn], { c1: { connected: true, account: 'acct@example.com' } });
  assert.strictEqual(h.kbAuthLabel('c1'), "Limitless");
  assert.match(h.kbPlaudAuthLine('c1', "limitless"), /Limitless: connected as <strong>acct@example\.com<\/strong>/);
  const buttons = h.kbPlaudAuthButtons('c1', '', "limitless");
  assert.match(buttons, /Disconnect/);
  assert.match(buttons, /Re-check sign-in with Limitless/);
  const done = h.kbLimitlessHelpHtml('c1', conn);
  assert.match(done, /Open Inbox/);
  assert.doesNotMatch(done, /data-kb-limitless-help/);
});

test('limitless help explains what the hive reads', () => {
  const h = load([conn], { c1: { connected: false, message: 'limitless: not connected' } });
  const card = h.kbLimitlessHelpHtml('c1', conn);
  assert.match(card, /data-kb-limitless-help/);
  assert.match(card, /lifelogs/);
  assert.match(card, /starred lifelogs only/);
  assert.match(card, /no playback/);
  assert.doesNotMatch(card, /<textarea/);
  assert.match(h.kbPlaudAuthButtons('c1', '', "limitless"), /Connect Limitless/);
  assert.doesNotMatch(card, /data-kb-limitless-ratelimit/);
});

test('limitless failure hint 1', () => {
  const h = load([conn], { c1: { connected: false, message: "limitless: HTTP 429 \u2014 Limitless rate limit reached (180 requests a minute per key)" } });
  assert.match(h.kbLimitlessHelpHtml('c1', conn), /data-kb-limitless-ratelimit/);
});
