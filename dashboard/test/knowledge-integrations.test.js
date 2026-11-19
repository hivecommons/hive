'use strict';

// Integrations cards for the meeting-recording connectors (Zoom, Teams):
// the sign-in line and buttons shared with Plaud, the Zoom paste fallback,
// the Teams admin-consent status and the app-registration form, extracted
// from the generated dashboard page so the test exercises the shipped code.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const indexHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'src', 'pkg', 'dashboard', 'static', 'index.html'), 'utf8');

function extractFunction(name) {
  const start = indexHtml.indexOf('function ' + name + '(');
  assert.notEqual(start, -1, `${name} function exists`);
  const brace = indexHtml.indexOf('{', start);
  let depth = 0;
  for (let i = brace; i < indexHtml.length; i++) {
    const ch = indexHtml[i];
    if (ch === '{') depth++;
    if (ch === '}') depth--;
    if (depth === 0) return indexHtml.slice(start, i + 1);
  }
  throw new Error(`unterminated ${name}`);
}

function makeContext(connectors, auth) {
  const ctx = {
    KB_AUTH_TYPES: { plaud: 'Plaud', zoom: 'Zoom', teams: 'Teams' },
    KB_MEETING_AUTH_SETTING_KEYS: { client_id_env: 'knowledge.connectors[].auth.client_id_env', client_secret_env: 'knowledge.connectors[].auth.client_secret_env', account_id_env: 'knowledge.connectors[].auth.account_id_env', tenant_id_env: 'knowledge.connectors[].auth.tenant_id_env' },
    _kcState: { connectors },
    _kbPlaudAuth: auth || {},
    escapeHtml: (s) => String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'),
    setTimeout: () => {},
    kbPlaudAuthLoad: () => {},
    String,
    RegExp,
  };
  vm.createContext(ctx);
  for (const fn of ['kbAuthLabel', 'kbPlaudAuthLine', 'kbPlaudAuthButtons', 'kbZoomPasteHtml', 'kbTeamsConsentHtml', 'kbMeetingAuthFormHTML']) {
    vm.runInContext(extractFunction(fn), ctx);
  }
  return ctx;
}

const zoomConn = { name: 'calls', type: 'zoom', scope: { redirect_uri: 'http://localhost:8199/zoom/callback' }, auth: { mode: 'user_oauth', client_id_env: 'ZOOM_CLIENT_ID', client_secret_env: 'ZOOM_CLIENT_SECRET' } };
const teamsConn = { name: 'mtg', type: 'teams', scope: { users: '8b081ef6-4792-4def-b2c9-c363a1bf41d5' }, auth: { tenant_id_env: 'TEAMS_TENANT_ID', client_id_env: 'TEAMS_CLIENT_ID', client_secret_env: 'TEAMS_CLIENT_SECRET' } };

test('sign-in line and buttons follow the connector type', () => {
  const ctx = makeContext([zoomConn, teamsConn], {
    calls: { pending: true, auth_url: 'https://zoom.us/oauth/authorize?client_id=cid&state=s' },
    mtg: { connected: false, message: 'teams: requires Entra admin consent for OnlineMeetingRecording.Read.All and OnlineMeetingTranscript.Read.All', auth_url: 'https://login.microsoftonline.com/t/adminconsent?client_id=app' },
  });
  assert.equal(vm.runInContext('kbAuthLabel("calls")', ctx), 'Zoom');
  assert.equal(vm.runInContext('kbAuthLabel("mtg")', ctx), 'Teams');
  assert.equal(vm.runInContext('kbAuthLabel("unknown")', ctx), 'Plaud');

  const zoomLine = vm.runInContext('kbPlaudAuthLine("calls", "zoom")', ctx);
  assert.match(zoomLine, /Zoom: waiting for authorization/);
  assert.match(zoomLine, /open the Zoom authorization page/);
  assert.match(zoomLine, /href="https:\/\/zoom\.us\/oauth\/authorize\?client_id=cid&amp;state=s"/);

  const teamsLine = vm.runInContext('kbPlaudAuthLine("mtg", "teams")', ctx);
  assert.match(teamsLine, /Teams: not connected/);
  assert.match(teamsLine, /requires Entra admin consent/);
  assert.match(teamsLine, /open the Teams admin consent page/);

  const buttons = vm.runInContext('kbPlaudAuthButtons("calls", "", "zoom")', ctx);
  assert.match(buttons, /Connect Zoom/);
  assert.match(buttons, /data-action="kbPlaudConnect" data-arg0="calls" disabled/);
  assert.match(buttons, /Re-check sign-in with Zoom/);
});

test('zoom card shows the registration steps and the paste fallback until connected', () => {
  const ctx = makeContext([zoomConn], { calls: { connected: false } });
  const html = vm.runInContext('kbZoomPasteHtml("calls", ' + JSON.stringify(zoomConn) + ')', ctx);
  assert.match(html, /data-kb-zoom-paste/);
  assert.match(html, /cloud_recording:read:list_user_recordings/);
  assert.match(html, /http:\/\/localhost:8199\/zoom\/callback/);
  assert.match(html, /id="kb-plaud-paste-calls"/);
  assert.match(html, /data-action="kbPlaudPasteComplete" data-arg0="calls"/);
  assert.doesNotMatch(html, /tokens-mcp\.json/, 'Zoom never offers the Plaud token-file paste');

  const connected = makeContext([zoomConn], { calls: { connected: true, account: 'andy@example.com' } });
  const done = vm.runInContext('kbZoomPasteHtml("calls", ' + JSON.stringify(zoomConn) + ')', connected);
  assert.doesNotMatch(done, /data-kb-zoom-paste/);
  assert.match(done, /Open Inbox/);

  const s2s = Object.assign({}, zoomConn, { auth: { mode: 'server_to_server' } });
  const s2sHtml = vm.runInContext('kbZoomPasteHtml("calls", ' + JSON.stringify(s2s) + ')', ctx);
  assert.match(s2sHtml, /data-kb-zoom-s2s/);
  assert.match(s2sHtml, /cloud_recording:read:list_user_recordings:admin/);
  assert.doesNotMatch(s2sHtml, /<textarea/);
});

test('teams card states the admin consent requirement when the probe fails', () => {
  const failing = makeContext([teamsConn], { mtg: { connected: false, message: 'teams: requires Entra admin consent for OnlineMeetingRecording.Read.All and OnlineMeetingTranscript.Read.All (Authorization_RequestDenied)', auth_url: 'https://login.microsoftonline.com/t/adminconsent?client_id=app' } });
  const html = vm.runInContext('kbTeamsConsentHtml("mtg", ' + JSON.stringify(teamsConn) + ')', failing);
  assert.match(html, /data-kb-teams-consent/);
  assert.match(html, /Requires Entra admin consent for OnlineMeetingRecording\.Read\.All and OnlineMeetingTranscript\.Read\.All/);
  assert.match(html, /grant consent ↗/);
  assert.match(html, /New-CsApplicationAccessPolicy|application access policy/);

  const unchecked = makeContext([teamsConn], { mtg: { connected: false, message: 'token request rejected' } });
  const plain = vm.runInContext('kbTeamsConsentHtml("mtg", ' + JSON.stringify(teamsConn) + ')', unchecked);
  assert.doesNotMatch(plain, /data-kb-teams-consent/);
  assert.match(plain, /data-kb-teams-help/);

  const connected = makeContext([teamsConn], { mtg: { connected: true, account: 'organizer 8b08…' } });
  assert.match(vm.runInContext('kbTeamsConsentHtml("mtg", ' + JSON.stringify(teamsConn) + ')', connected), /Open Inbox/);
});

test('app registration form references env var names only and carries settings keys', () => {
  const ctx = makeContext([zoomConn, teamsConn]);
  const zoom = vm.runInContext('kbMeetingAuthFormHTML("zoom", ' + JSON.stringify(zoomConn) + ')', ctx);
  for (const key of ['mode', 'client_id_env', 'client_secret_env', 'account_id_env']) {
    assert.match(zoom, new RegExp('data-kb-kc-auth-field="' + key + '"'), key + ' field');
    assert.match(zoom, new RegExp('data-setting-key="knowledge\\.connectors\\[\\]\\.auth\\.' + key + '"'), key + ' settings key');
  }
  assert.match(zoom, /value="ZOOM_CLIENT_ID"/);
  assert.match(zoom, /<option value="user_oauth" selected>/);
  assert.match(zoom, /connector-state\/&lt;name&gt;\/home\/\.zoom\/tokens\.json/);

  const teams = vm.runInContext('kbMeetingAuthFormHTML("teams", ' + JSON.stringify(teamsConn) + ')', ctx);
  for (const key of ['tenant_id_env', 'client_id_env', 'client_secret_env']) {
    assert.match(teams, new RegExp('data-kb-kc-auth-field="' + key + '"'), key + ' field');
  }
  assert.doesNotMatch(teams, /account_id_env/);
  assert.match(teams, /OnlineMeetingRecording\.Read\.All/);
  assert.match(teams, /getAllRecordings/);
});
