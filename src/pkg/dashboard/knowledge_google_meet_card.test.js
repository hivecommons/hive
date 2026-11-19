// Knowledge Integrations card for the Google Meet connector: the shared
// sign-in line and buttons carry the Google Meet label, the user-level card
// offers the pasted-redirect fallback, the service-account card has no
// browser step, the connected card points at the Inbox and explains why Meet
// recordings link Drive instead of playing inline, and the app-registration
// form references environment variable names only.
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
// The multi-line settings-key table, lifted verbatim.
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
  ['kbAuthLabel', 'kbPlaudAuthLine', 'kbPlaudAuthButtons', 'kbGoogleMeetPasteHtml', 'kbMeetingAuthFormHTML'].map(fnSrc).join('\n') +
  '\nreturn { kbAuthLabel, kbPlaudAuthLine, kbPlaudAuthButtons, kbGoogleMeetPasteHtml, kbMeetingAuthFormHTML };')(
  { connectors }, auth || {}, escapeHtml, () => {}, () => {});

const meet = { name: 'gmeet', type: 'google-meet', scope: {}, auth: { mode: 'user_oauth', client_id_env: 'GOOGLE_MEET_CLIENT_ID', client_secret_env: 'GOOGLE_MEET_CLIENT_SECRET' } };
const sa = { name: 'gmeet-sa', type: 'google-meet', scope: { users: 'alice@example.com' }, auth: { mode: 'server_to_server', file: '/run/secrets/sa.json' } };

test('google meet sign-in line and buttons carry the Google Meet label', () => {
  const h = load([meet], { gmeet: { pending: true, auth_url: 'https://accounts.google.com/o/oauth2/v2/auth?client_id=cid&state=s' } });
  assert.strictEqual(h.kbAuthLabel('gmeet'), 'Google Meet');
  const line = h.kbPlaudAuthLine('gmeet', 'google-meet');
  assert.match(line, /Google Meet: waiting for authorization/);
  assert.match(line, /open the Google Meet authorization page/);
  assert.match(line, /href="https:\/\/accounts\.google\.com\/o\/oauth2\/v2\/auth\?client_id=cid&amp;state=s"/);
  const buttons = h.kbPlaudAuthButtons('gmeet', '', 'google-meet');
  assert.match(buttons, /Connect Google Meet/);
  assert.match(buttons, /data-action="kbPlaudConnect" data-arg0="gmeet" disabled/);
  assert.match(buttons, /Re-check sign-in with Google Meet/);
});

test('user-level card offers the pasted-redirect fallback until connected', () => {
  const h = load([meet], { gmeet: { connected: false } });
  const card = h.kbGoogleMeetPasteHtml('gmeet', meet);
  assert.match(card, /data-kb-google-meet-paste/);
  assert.match(card, /http:\/\/localhost:8199\/google-meet\/callback/);
  assert.match(card, /meetings\.space\.readonly/);
  assert.match(card, /drive\.meet\.readonly/);
  assert.match(card, /id="kb-plaud-paste-gmeet"/);
  assert.match(card, /data-action="kbPlaudPasteComplete" data-arg0="gmeet"/);
  assert.doesNotMatch(card, /tokens-mcp\.json/);

  const custom = Object.assign({}, meet, { scope: { redirect_uri: 'https://hive.example/cb' } });
  assert.match(h.kbGoogleMeetPasteHtml('gmeet', custom), /https:\/\/hive\.example\/cb/);

  const connected = load([meet], { gmeet: { connected: true, account: 'andy@example.com' } });
  const done = connected.kbGoogleMeetPasteHtml('gmeet', meet);
  assert.doesNotMatch(done, /data-kb-google-meet-paste/);
  assert.match(done, /Open Inbox/);
  assert.match(done, /data-kb-google-meet-playback/);
});

test('service-account card has no browser step', () => {
  const h = load([sa], { 'gmeet-sa': { connected: false } });
  const card = h.kbGoogleMeetPasteHtml('gmeet-sa', sa);
  assert.match(card, /data-kb-google-meet-sa/);
  assert.match(card, /Domain-wide delegation/);
  assert.doesNotMatch(card, /<textarea/);
});

test('registration form references env var names only and carries settings keys', () => {
  const h = load([meet]);
  const form = h.kbMeetingAuthFormHTML('google-meet', meet);
  for (const key of ['mode', 'client_id_env', 'client_secret_env']) {
    assert.match(form, new RegExp('data-kb-kc-auth-field="' + key + '"'), key + ' field');
    assert.match(form, new RegExp('data-setting-key="knowledge\\.connectors\\[\\]\\.auth\\.' + key + '"'), key + ' settings key');
  }
  assert.match(form, /value="GOOGLE_MEET_CLIENT_ID"/);
  assert.match(form, /<option value="user_oauth" selected>/);
  assert.doesNotMatch(form, /tenant_id_env|account_id_env/);
  assert.match(form, /data-kb-google-meet-note/);
  assert.match(form, /\.google-meet\/tokens\.json/);
  const saForm = h.kbMeetingAuthFormHTML('google-meet', sa);
  assert.match(saForm, /<option value="server_to_server" selected>/);
  assert.match(saForm, /value=""/);
});
