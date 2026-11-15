// Knowledge Integrations card for the Fathom connector: the shared sign-in
// line and buttons carry the Fathom label, the help explains what the hive
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
  ['kbAuthLabel', 'kbPlaudAuthLine', 'kbPlaudAuthButtons', 'kbFathomHelpHtml'].map(fnSrc).join('\n') +
  '\nreturn { kbAuthLabel, kbPlaudAuthLine, kbPlaudAuthButtons, kbFathomHelpHtml };')(
  { connectors }, auth || {}, escapeHtml, () => {}, () => {});

const conn = {"name": "c1", "type": "fathom", "scope": {"recorded_by": "andy@example.com"}, "auth": {"env": "FATHOM_API_KEY"}};

test('fathom sign-in line and buttons carry the Fathom label', () => {
  const h = load([conn], { c1: { connected: true, account: 'acct@example.com' } });
  assert.strictEqual(h.kbAuthLabel('c1'), "Fathom");
  assert.match(h.kbPlaudAuthLine('c1', "fathom"), /Fathom: connected as <strong>acct@example\.com<\/strong>/);
  const buttons = h.kbPlaudAuthButtons('c1', '', "fathom");
  assert.match(buttons, /Disconnect/);
  assert.match(buttons, /Re-check sign-in with Fathom/);
  const done = h.kbFathomHelpHtml('c1', conn);
  assert.match(done, /Open Inbox/);
  assert.doesNotMatch(done, /data-kb-fathom-help/);
});

test('fathom help explains what the hive reads', () => {
  const h = load([conn], { c1: { connected: false, message: 'fathom: not connected' } });
  const card = h.kbFathomHelpHtml('c1', conn);
  assert.match(card, /data-kb-fathom-help/);
  assert.match(card, /User Settings → API Access/);
  assert.match(card, /<code>andy@example\.com<\/code>/);
  assert.match(card, /GET \/meetings/);
  assert.doesNotMatch(card, /<textarea/);
  assert.match(h.kbPlaudAuthButtons('c1', '', "fathom"), /Connect Fathom/);
  assert.doesNotMatch(card, /data-kb-fathom-ratelimit/);
});

test('fathom failure hint 1', () => {
  const h = load([conn], { c1: { connected: false, message: "fathom: HTTP 429 from https://api.fathom.ai/external/v1/meetings \u2014 Fathom rate limit reached" } });
  assert.match(h.kbFathomHelpHtml('c1', conn), /data-kb-fathom-ratelimit/);
});
