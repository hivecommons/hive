// Knowledge Integrations card for the Granola connector: the shared sign-in
// line and buttons carry the Granola label, the help explains what the hive
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
  ['kbAuthLabel', 'kbPlaudAuthLine', 'kbPlaudAuthButtons', 'kbGranolaHelpHtml'].map(fnSrc).join('\n') +
  '\nreturn { kbAuthLabel, kbPlaudAuthLine, kbPlaudAuthButtons, kbGranolaHelpHtml };')(
  { connectors }, auth || {}, escapeHtml, () => {}, () => {});

const conn = {"name": "c1", "type": "granola", "scope": {"folder": "fol_1"}, "auth": {"env": "GRANOLA_API_KEY"}};

test('granola sign-in line and buttons carry the Granola label', () => {
  const h = load([conn], { c1: { connected: true, account: 'acct@example.com' } });
  assert.strictEqual(h.kbAuthLabel('c1'), "Granola");
  assert.match(h.kbPlaudAuthLine('c1', "granola"), /Granola: connected as <strong>acct@example\.com<\/strong>/);
  const buttons = h.kbPlaudAuthButtons('c1', '', "granola");
  assert.match(buttons, /Disconnect/);
  assert.match(buttons, /Re-check sign-in with Granola/);
  const done = h.kbGranolaHelpHtml('c1', conn);
  assert.match(done, /Open Inbox/);
  assert.doesNotMatch(done, /data-kb-granola-help/);
});

test('granola help explains what the hive reads', () => {
  const h = load([conn], { c1: { connected: false, message: 'granola: not connected' } });
  const card = h.kbGranolaHelpHtml('c1', conn);
  assert.match(card, /data-kb-granola-help/);
  assert.match(card, /grn_…/);
  assert.match(card, /<code>fol_1<\/code>/);
  assert.match(card, /keeps no audio/);
  assert.doesNotMatch(card, /<textarea/);
  assert.match(h.kbPlaudAuthButtons('c1', '', "granola"), /Connect Granola/);
  assert.doesNotMatch(card, /data-kb-granola-ratelimit/);
});

test('granola failure hint 1', () => {
  const h = load([conn], { c1: { connected: false, message: "granola: HTTP 429 \u2014 Granola rate limit reached; the next sync retries" } });
  assert.match(h.kbGranolaHelpHtml('c1', conn), /data-kb-granola-ratelimit/);
});
