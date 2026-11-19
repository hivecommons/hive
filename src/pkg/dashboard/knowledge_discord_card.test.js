// Knowledge Integrations card for the Discord meetings connector: the shared
// sign-in line and buttons (Connect probes the bot token), the setup help and
// the bot-invite status when the bot cannot read the server or a channel, and
// the connector row wiring. Functions are extracted from the generated page
// into a `new Function` sandbox, so every helper they call is injected here.
// Run: node --test src/pkg/dashboard/*.test.js
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
const ASYNC_PREFIX = 'async ';
const fnSrc = name => {
  let start = html.indexOf('function ' + name + '(');
  assert.ok(start >= 0, name + ' not found');
  if (html.slice(start - ASYNC_PREFIX.length, start) === ASYNC_PREFIX) start -= ASYNC_PREFIX.length;
  let depth = 0;
  for (let i = html.indexOf('{', start); i < html.length; i++) {
    if (html[i] === '{') depth++;
    else if (html[i] === '}' && --depth === 0) return html.slice(start, i + 1);
  }
  throw new Error('unbalanced ' + name);
};
// Any one-line `const NAME = <expr>;` declaration, verbatim.
const declSrc = name => {
  const m = html.match(new RegExp('const ' + name + ' = [^\\n]+;'));
  assert.ok(m, name + ' not found');
  return m[0];
};
const constSrc = name => {
  const m = html.match(new RegExp('const ' + name + " = '([^']*)';"));
  assert.ok(m, name + ' not found');
  return m[1];
};

const escapeHtml = s => String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
const DISCORD = { name: 'community', type: 'discord', enabled: true, scope: { guild: '111111111111111111', channels: '222222222222222222' }, auth: { env: 'DISCORD_BOT_TOKEN' } };
const MISSING_ACCESS = 'channel 222222222222222222: discord: the bot cannot read the configured server or channel: invite it with View Channel and Read Message History, and enable the Message Content intent (HTTP 404)';
const INVITE = 'https://discord.com/oauth2/authorize?client_id=444444444444444444&guild_id=111111111111111111&permissions=66560&scope=bot';

// The card helpers with their constants, bound to one auth cache.
const helpers = (auth, connectors = [DISCORD]) => new Function('_kcState', '_kbPlaudAuth', 'escapeHtml', 'setTimeout', 'kbPlaudAuthLoad',
  declSrc('KB_AUTH_TYPES') + '\n' + ['kbAuthLabel', 'kbPlaudAuthLine', 'kbPlaudAuthButtons', 'kbDiscordHelpHtml'].map(fnSrc).join('\n') +
  '\nreturn { kbAuthLabel, kbPlaudAuthLine, kbPlaudAuthButtons, kbDiscordHelpHtml, KB_AUTH_TYPES };')(
  { connectors }, auth, escapeHtml, () => {}, () => {});

test('Discord is an interactive-auth type with its own label', () => {
  const h = helpers({});
  assert.strictEqual(h.KB_AUTH_TYPES.discord, 'Discord');
  assert.strictEqual(h.kbAuthLabel('community'), 'Discord');
  assert.strictEqual(h.kbAuthLabel('community', 'discord'), 'Discord');
});

test('sign-in line, invite link and buttons follow the bot probe', () => {
  const h = helpers({ community: { connected: false, message: MISSING_ACCESS, auth_url: INVITE } });
  const line = h.kbPlaudAuthLine('community', 'discord');
  assert.match(line, /Discord: not connected/);
  assert.match(line, /invite it with View Channel and Read Message History/);
  assert.match(line, /open the Discord bot invite page/);
  assert.ok(line.includes('href="' + escapeHtml(INVITE) + '"'), 'invite URL linked');

  const buttons = h.kbPlaudAuthButtons('community', '', 'discord');
  assert.match(buttons, /Connect Discord/);
  assert.match(buttons, /Re-check sign-in with Discord/);
  assert.doesNotMatch(buttons, /Disconnect/);

  const ok = helpers({ community: { connected: true, account: 'bot hive-meetings in server 111111111111111111' } });
  assert.match(ok.kbPlaudAuthLine('community', 'discord'), /Discord: connected as <strong>bot hive-meetings in server 111111111111111111<\/strong>/);
  assert.match(ok.kbPlaudAuthButtons('community', '', 'discord'), /data-action="kbPlaudDisconnect"/);
});

test('help explains the bot setup and flags missing access with the invite link', () => {
  const failing = helpers({ community: { connected: false, message: MISSING_ACCESS, auth_url: INVITE } });
  const out = failing.kbDiscordHelpHtml('community', DISCORD);
  assert.match(out, /data-kb-discord-access/);
  assert.match(out, /server <code>111111111111111111<\/code> and channels <code>222222222222222222<\/code>/);
  assert.match(out, /invite the bot ↗/);
  assert.match(out, /data-kb-discord-help/);
  assert.match(out, /Message Content<\/strong> intent/);
  assert.match(out, /View Channel and Read Message History/);
  assert.match(out, /Craig/);

  const unchecked = helpers({ community: { connected: false, message: 'discord: auth.env names DISCORD_BOT_TOKEN, which is empty or unset' } });
  const plain = unchecked.kbDiscordHelpHtml('community', { name: 'community', type: 'discord', scope: {} });
  assert.doesNotMatch(plain, /data-kb-discord-access/);
  assert.match(plain, /the configured server and channels/);

  const connected = helpers({ community: { connected: true } });
  const done = connected.kbDiscordHelpHtml('community', DISCORD);
  assert.match(done, /Open Inbox/);
  assert.doesNotMatch(done, /data-kb-discord-help/);
});

test('help escapes scope values', () => {
  const h = helpers({});
  const out = h.kbDiscordHelpHtml('x', { scope: { guild: '<img src=x>' } });
  assert.ok(!out.includes('<img'), 'scope is escaped');
});

// The connector row renders the Discord line + help and the auth buttons.
const ROW_HELPERS = ['kbConnectorStatusOf', 'kbConnectorMetaHtml', 'kcProgressActive', 'kcProgressStartMs', 'kcElapsedText', 'kcProgressCount',
  'kcProgressLabel', 'kcProgressHtml', 'kcProgressFailedHtml'].map(fnSrc).join('\n') + '\n' + ['KC_PROGRESS_ACTIVE_PHASES', 'KC_PROGRESS_TITLE_MAX', 'KB_AUTH_TYPES'].map(declSrc).join('\n');

const renderRow = ({ runtime, c, auth = {} }) => {
  const calls = { help: 0 };
  const row = new Function('_kcState', '_kbPlaudAuth', 'escapeHtml', 'kcAgo', 'kbPlaudAuthLine', 'kbPlaudPasteHtml', 'kbZoomPasteHtml', 'kbTeamsConsentHtml', 'kbDiscordHelpHtml', 'kbWisprFlowSourceLine',
    fnSrc('kbConnectorNotLoaded') + '\n' + fnSrc('kbConnectorNotLoadedHtml') + '\n' + fnSrc('kbAuthLabel') + '\n' + fnSrc('kbPlaudAuthButtons') + '\n' + fnSrc('kbConnectorRow') + '\n' + ROW_HELPERS +
    "\nconst KC_STATUS_NOT_LOADED = '" + constSrc('KC_STATUS_NOT_LOADED') + "'; const KC_NOT_LOADED_HINT = '" + constSrc('KC_NOT_LOADED_HINT') + "';" +
    '\nreturn kbConnectorRow;')(
    { runtime, rows: [], publish: null, connectors: [c] }, auth, escapeHtml, () => '—',
    (name, type) => '<div data-kb-plaud-auth="' + name + '" data-type="' + type + '"></div>', () => '', () => '', () => '',
    () => { calls.help++; return '<p data-kb-discord-help></p>'; }, () => '');
  return { html: row('discord', c, true), calls };
};

test('the Discord row shows the sign-in line, help and Connect/Check/Sync', () => {
  const { html: out, calls } = renderRow({ runtime: true, c: DISCORD });
  assert.match(out, /data-kb-plaud-auth="community" data-type="discord"/);
  assert.strictEqual(calls.help, 1);
  for (const action of ['kbPlaudConnect', 'kbPlaudRefreshAuth', 'kbIntegrationSync', 'kbIntegrationEdit']) {
    assert.match(out, new RegExp('data-action="' + action + '"'), action + ' rendered');
  }
  assert.match(out, /Connect Discord/);
});

test('a Discord row whose connector is not loaded disables the buttons and skips the probe', () => {
  const { html: out, calls } = renderRow({ runtime: false, c: DISCORD });
  assert.strictEqual(calls.help, 0);
  assert.match(out, /data-kb-not-loaded/);
  assert.match(out, /data-action="kbPlaudConnect" data-arg0="community" disabled/);
});
