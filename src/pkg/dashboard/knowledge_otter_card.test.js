// Knowledge Integrations card for the Otter connector: the shared sign-in
// line and buttons carry the Otter label, the help explains what the hive
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
  ['kbAuthLabel', 'kbPlaudAuthLine', 'kbPlaudAuthButtons', 'kbOtterHelpHtml'].map(fnSrc).join('\n') +
  '\nreturn { kbAuthLabel, kbPlaudAuthLine, kbPlaudAuthButtons, kbOtterHelpHtml };')(
  { connectors }, auth || {}, escapeHtml, () => {}, () => {});

const conn = {"name": "c1", "type": "otter", "scope": {"channel": "channel_1"}, "auth": {"env": "OTTER_API_KEY"}};

test('otter sign-in line and buttons carry the Otter label', () => {
  const h = load([conn], { c1: { connected: true, account: 'acct@example.com' } });
  assert.strictEqual(h.kbAuthLabel('c1'), "Otter");
  assert.match(h.kbPlaudAuthLine('c1', "otter"), /Otter: connected as <strong>acct@example\.com<\/strong>/);
  const buttons = h.kbPlaudAuthButtons('c1', '', "otter");
  assert.match(buttons, /Disconnect/);
  assert.match(buttons, /Re-check sign-in with Otter/);
  const done = h.kbOtterHelpHtml('c1', conn);
  assert.match(done, /Open Inbox/);
  assert.doesNotMatch(done, /data-kb-otter-help/);
});

test('otter help explains what the hive reads', () => {
  const h = load([conn], { c1: { connected: false, message: 'otter: not connected' } });
  const card = h.kbOtterHelpHtml('c1', conn);
  assert.match(card, /data-kb-otter-help/);
  assert.match(card, /Integrations → Developer/);
  assert.match(card, /<code>channel_1<\/code>/);
  assert.match(card, /Enterprise workspaces/);
  assert.doesNotMatch(card, /<textarea/);
  assert.match(h.kbPlaudAuthButtons('c1', '', "otter"), /Connect Otter/);
  assert.doesNotMatch(card, /data-kb-otter-enterprise/);
});

test('otter failure hint 1', () => {
  const h = load([conn], { c1: { connected: false, message: "otter: not connected \u2014 use Connect (HTTP 401 (a wrong or deleted key, or a workspace without the Public API))" } });
  assert.match(h.kbOtterHelpHtml('c1', conn), /data-kb-otter-enterprise/);
});
