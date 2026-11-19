// Knowledge Integrations card for the Webex connector: the shared sign-in
// line and buttons carry the Webex label, the card offers the
// pasted-redirect fallback until connected, the connected card points at
// the Inbox, and the integration form references environment variable
// names only.
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
const blockDecl = name => {
  const start = html.indexOf('const ' + name + ' = {');
  assert.ok(start >= 0, name + ' not found');
  return html.slice(start, html.indexOf('};', start) + 2);
};

const escapeHtml = s => String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');

// Every helper the card calls is injected (KB_AUTH_TYPES, kbAuthLabel, the
// settings-key table), or the sandbox throws ReferenceError.
const load = (connectors, auth) => new Function('_kcState', '_kbPlaudAuth', 'escapeHtml', 'setTimeout', 'kbPlaudAuthLoad',
  declSrc('KB_AUTH_TYPES') + '\n' + blockDecl('KB_MEETING_AUTH_SETTING_KEYS') + '\n' +
  ['kbAuthLabel', 'kbPlaudAuthLine', 'kbPlaudAuthButtons', 'kbWebexPasteHtml', 'kbMeetingAuthFormHTML'].map(fnSrc).join('\n') +
  '\nreturn { kbAuthLabel, kbPlaudAuthLine, kbPlaudAuthButtons, kbWebexPasteHtml, kbMeetingAuthFormHTML };')(
  { connectors }, auth || {}, escapeHtml, () => {}, () => {});

const webex = { name: 'wbx', type: 'webex', scope: {}, auth: { client_id_env: 'WEBEX_CLIENT_ID', client_secret_env: 'WEBEX_CLIENT_SECRET' } };

test('webex sign-in line and buttons carry the Webex label', () => {
  const h = load([webex], { wbx: { pending: true, auth_url: 'https://webexapis.com/v1/authorize?client_id=cid&state=s' } });
  assert.strictEqual(h.kbAuthLabel('wbx'), 'Webex');
  const line = h.kbPlaudAuthLine('wbx', 'webex');
  assert.match(line, /Webex: waiting for authorization/);
  assert.match(line, /open the Webex authorization page/);
  assert.match(line, /href="https:\/\/webexapis\.com\/v1\/authorize\?client_id=cid&amp;state=s"/);
  const buttons = h.kbPlaudAuthButtons('wbx', '', 'webex');
  assert.match(buttons, /Connect Webex/);
  assert.match(buttons, /Re-check sign-in with Webex/);
});

test('webex card offers the pasted-redirect fallback until connected', () => {
  const h = load([webex], { wbx: { connected: false } });
  const card = h.kbWebexPasteHtml('wbx', webex);
  assert.match(card, /data-kb-webex-paste/);
  assert.match(card, /http:\/\/localhost:8199\/webex\/callback/);
  assert.match(card, /meeting:recordings_read/);
  assert.match(card, /meeting:transcripts_read/);
  assert.match(card, /id="kb-plaud-paste-wbx"/);
  assert.match(card, /data-action="kbPlaudPasteComplete" data-arg0="wbx"/);
  const custom = Object.assign({}, webex, { scope: { redirect_uri: 'https://hive.example/webex' } });
  assert.match(h.kbWebexPasteHtml('wbx', custom), /https:\/\/hive\.example\/webex/);

  const connected = load([webex], { wbx: { connected: true, account: 'andy@example.com' } });
  const done = connected.kbWebexPasteHtml('wbx', webex);
  assert.doesNotMatch(done, /data-kb-webex-paste/);
  assert.match(done, /Open Inbox/);
  assert.match(done, /data-kb-webex-playback/);
});

test('webex form references env var names only and carries settings keys', () => {
  const h = load([webex]);
  const form = h.kbMeetingAuthFormHTML('webex', webex);
  for (const key of ['client_id_env', 'client_secret_env']) {
    assert.match(form, new RegExp('data-kb-kc-auth-field="' + key + '"'), key + ' field');
    assert.match(form, new RegExp('data-setting-key="knowledge\\.connectors\\[\\]\\.auth\\.' + key + '"'), key + ' settings key');
  }
  assert.match(form, /value="WEBEX_CLIENT_ID"/);
  assert.doesNotMatch(form, /tenant_id_env|account_id_env|data-kb-kc-auth-field="mode"/);
  assert.match(form, /data-kb-webex-note/);
  assert.match(form, /\.webex\/tokens\.json/);
});
