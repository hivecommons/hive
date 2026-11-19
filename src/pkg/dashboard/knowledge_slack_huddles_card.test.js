// Knowledge Integrations card for the Slack huddles connector: the shared
// sign-in line and buttons carry the Slack label, the help says Slack keeps
// the AI notes canvas but never audio, the missing-scope and not-in-channel
// failures get their own hint, and the connected card points at the Inbox.
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
  ['kbAuthLabel', 'kbPlaudAuthLine', 'kbPlaudAuthButtons', 'kbSlackHuddlesHelpHtml'].map(fnSrc).join('\n') +
  '\nreturn { kbAuthLabel, kbPlaudAuthLine, kbPlaudAuthButtons, kbSlackHuddlesHelpHtml };')(
  { connectors }, auth || {}, escapeHtml, () => {}, () => {});

const slack = { name: 'huddles', type: 'slack-huddles', scope: { channels: 'C0123ABC' }, auth: { env: 'SLACK_HUDDLES_TOKEN' } };

test('slack huddles sign-in line and buttons carry the Slack label', () => {
  const h = load([slack], { huddles: { connected: true, account: 'hive-bot @ Acme' } });
  assert.strictEqual(h.kbAuthLabel('huddles'), 'Slack');
  assert.match(h.kbPlaudAuthLine('huddles', 'slack-huddles'), /Slack: connected as <strong>hive-bot @ Acme<\/strong>/);
  const buttons = h.kbPlaudAuthButtons('huddles', '', 'slack-huddles');
  assert.match(buttons, /Disconnect/);
  assert.match(buttons, /Re-check sign-in with Slack/);
  const done = h.kbSlackHuddlesHelpHtml('huddles', slack);
  assert.match(done, /Open Inbox/);
  assert.doesNotMatch(done, /data-kb-slack-huddles-help/);
});

test('slack huddles help says notes only, never audio', () => {
  const h = load([slack], { huddles: { connected: false, message: 'slack-huddles: not connected' } });
  const card = h.kbSlackHuddlesHelpHtml('huddles', slack);
  assert.match(card, /data-kb-slack-huddles-help/);
  assert.match(card, /never records huddle audio/);
  assert.match(card, /canvases\.getContent/);
  assert.match(card, /<code>C0123ABC<\/code>/);
  assert.doesNotMatch(card, /data-kb-slack-huddles-scope|data-kb-slack-huddles-invite|<textarea/);
});

test('missing scope and missing channel membership get their own hint', () => {
  const scope = load([slack], { huddles: { connected: false, message: 'conversations.history: missing_scope (needs scope channels:history; the token has users:read)' } });
  assert.match(scope.kbSlackHuddlesHelpHtml('huddles', slack), /data-kb-slack-huddles-scope/);
  const invite = load([slack], { huddles: { connected: false, message: 'channel C0123ABC: conversations.history: not_in_channel — add the app to the channel (/invite @app) or check scope.channels' } });
  const card = invite.kbSlackHuddlesHelpHtml('huddles', slack);
  assert.match(card, /data-kb-slack-huddles-invite/);
  assert.match(card, /\/invite @your-app/);
});
