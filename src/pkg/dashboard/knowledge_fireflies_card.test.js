// Knowledge Integrations card for the Fireflies connector: the shared
// sign-in line and buttons carry the Fireflies label, the help names the API
// key and the GraphQL API, paid-plan and rate-limit failures get their own
// hint, and the connected card points at the Inbox and Pro-plan playback.
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
  ['kbAuthLabel', 'kbPlaudAuthLine', 'kbPlaudAuthButtons', 'kbFirefliesHelpHtml'].map(fnSrc).join('\n') +
  '\nreturn { kbAuthLabel, kbPlaudAuthLine, kbPlaudAuthButtons, kbFirefliesHelpHtml };')(
  { connectors }, auth || {}, escapeHtml, () => {}, () => {});

const ff = { name: 'ff', type: 'fireflies', scope: {}, auth: { env: 'FIREFLIES_API_KEY' } };

test('fireflies sign-in line and buttons carry the Fireflies label', () => {
  const h = load([ff], { ff: { connected: true, account: 'andy@example.com' } });
  assert.strictEqual(h.kbAuthLabel('ff'), 'Fireflies');
  assert.match(h.kbPlaudAuthLine('ff', 'fireflies'), /Fireflies: connected as <strong>andy@example\.com<\/strong>/);
  const buttons = h.kbPlaudAuthButtons('ff', '', 'fireflies');
  assert.match(buttons, /Disconnect/);
  assert.match(buttons, /Re-check sign-in with Fireflies/);
  const done = h.kbFirefliesHelpHtml('ff', ff);
  assert.match(done, /Open Inbox/);
  assert.match(done, /data-kb-fireflies-playback/);
  assert.doesNotMatch(done, /data-kb-fireflies-help/);
});

test('fireflies help names the API key and the scope', () => {
  const h = load([ff], { ff: { connected: false, message: 'fireflies: not connected' } });
  const card = h.kbFirefliesHelpHtml('ff', ff);
  assert.match(card, /data-kb-fireflies-help/);
  assert.match(card, /Integrations → Fireflies API/);
  assert.match(card, /Every meeting/);
  assert.match(h.kbFirefliesHelpHtml('ff', { scope: { mine: 'true' } }), /Only meetings you organized/);
  assert.doesNotMatch(card, /data-kb-fireflies-paid|data-kb-fireflies-ratelimit|<textarea/);
  assert.match(h.kbPlaudAuthButtons('ff', '', 'fireflies'), /Connect Fireflies/);
});

test('paid plan and rate limit get their own hint', () => {
  const paid = load([ff], { ff: { connected: false, message: 'fireflies: Requires a paid plan (paid_required) — this needs a Fireflies Pro plan or higher' } });
  assert.match(paid.kbFirefliesHelpHtml('ff', ff), /data-kb-fireflies-paid/);
  const rl = load([ff], { ff: { connected: false, message: 'fireflies: Too many requests (too_many_requests)' } });
  assert.match(rl.kbFirefliesHelpHtml('ff', ff), /data-kb-fireflies-ratelimit/);
});
