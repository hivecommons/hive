// Inbox machine transcription UI: the 🎧 Transcribe button and state pill on
// items, the POST handler, and the ⚙ settings section with its consent gate.
// Functions are extracted from the generated page and run in a sandbox.
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

const BYTES_PER_MB = 1048576;
const escapeHtml = s => String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
const LABELS = { queued: 'Transcription queued', running: 'Transcribing', done: 'Machine transcript', failed: 'Transcription failed', skipped: 'Not transcribed' };

// itemHarness builds inboxTranscriptionHtml + inboxTranscribe with the
// helpers they call injected.
const itemHarness = ({ role = 'owner', status = 202, body = { state: { status: 'queued' } } } = {}) => {
  const calls = { fetches: [], toasts: [], renders: 0, refreshes: 0 };
  const fetch = async (url, opts) => {
    calls.fetches.push({ url, opts });
    return { ok: status >= 200 && status < 300, status, json: async () => body };
  };
  const ctx = {
    window: { _hiveRole: role },
    _inboxTranscribing: new Set(),
    _inboxContent: { 'plaud/r1': { transcript: 'old' } },
  };
  const h = new Function('ctx', 'fetch', 'escapeHtml', 'formatDashboardDateTime', 'showToast', 'renderInbox', 'fetchInbox', 'INBOX_TX_STATUS_LABELS',
    'const window = ctx.window; const _inboxTranscribing = ctx._inboxTranscribing; const _inboxContent = ctx._inboxContent;\n' +
    fnSrc('inboxViewerCanPlay') + '\n' + fnSrc('inboxApiPath') + '\n' + fnSrc('inboxTranscriptionHtml') + '\n' + fnSrc('inboxTranscribe') + '\n' +
    'return { inboxTranscriptionHtml, inboxTranscribe };');
  const api = h(ctx, fetch, escapeHtml, s => 'at ' + s, (msg, kind) => calls.toasts.push([msg, kind]),
    () => { calls.renders++; }, async () => { calls.refreshes++; }, LABELS);
  return { api, ctx, calls };
};

test('Transcribe button shows on transcribable items for owner and read-write only', () => {
  const it = { source: 'plaud', id: 'r1', pending: true, transcribable: true };
  for (const [role, want] of [['owner', true], ['read-write', true], ['read-only', false], ['', false]]) {
    const { api } = itemHarness({ role });
    const out = api.inboxTranscriptionHtml(it, 'plaud/r1');
    assert.strictEqual(/data-action="inboxTranscribe" data-arg0="plaud" data-arg1="r1"/.test(out), want, 'role ' + role);
  }
  const { api } = itemHarness();
  assert.strictEqual(api.inboxTranscriptionHtml({ source: 'plaud', id: 'r2', pending: true }, 'plaud/r2'), '', 'no button without transcribable');
  assert.match(api.inboxTranscriptionHtml(it, 'plaud/r1'), /🎧 Transcribe</);
});

test('state pill: spinner while running, reason and retry on failure, provider when done', () => {
  const { api, ctx } = itemHarness();
  let out = api.inboxTranscriptionHtml({ source: 'plaud', id: 'r1', transcription: { status: 'running', updated_at: '2026-11-15T10:00:00Z' } }, 'plaud/r1');
  assert.match(out, /inbox-tx-spinner/);
  assert.match(out, /data-status="syncing"/);
  assert.match(out, /Transcribing/);
  assert.match(out, /title="Updated at 2026-11-15T10:00:00Z"/);
  assert.doesNotMatch(out, /inboxTranscribe/, 'no button while running');
  out = api.inboxTranscriptionHtml({ source: 'plaud', id: 'r1', transcribable: true, transcription: { status: 'failed', reason: 'check the Deepgram key in $DEEPGRAM_API_KEY <x>' } }, 'plaud/r1');
  assert.match(out, /data-status="error"/);
  assert.match(out, /check the Deepgram key in \$DEEPGRAM_API_KEY &lt;x&gt;/);
  assert.match(out, /Retry transcription/);
  out = api.inboxTranscriptionHtml({ source: 'plaud', id: 'r1', transcription: { status: 'done', provider: 'deepgram', model: 'nova-3' } }, 'plaud/r1');
  assert.match(out, /Machine transcript \(deepgram\/nova-3\)/);
  assert.match(out, /data-status="ok"/);
  ctx._inboxTranscribing.add('plaud/r1');
  out = api.inboxTranscriptionHtml({ source: 'plaud', id: 'r1', transcribable: true }, 'plaud/r1');
  assert.match(out, /disabled aria-busy="true"/);
  assert.match(out, /Sending…/);
});

test('inboxTranscribe posts to the item endpoint, toasts and refreshes', async () => {
  const { api, ctx, calls } = itemHarness();
  await api.inboxTranscribe('plaud', 'a/b');
  assert.strictEqual(calls.fetches.length, 1);
  assert.strictEqual(calls.fetches[0].url, '/api/inbox/plaud/a%2Fb/transcribe');
  assert.strictEqual(calls.fetches[0].opts.method, 'POST');
  assert.strictEqual(calls.toasts[0][1], 'success');
  assert.strictEqual(calls.refreshes, 1);
  assert.strictEqual(ctx._inboxTranscribing.size, 0, 'spinner cleared');

  const skipped = itemHarness({ body: { state: { status: 'skipped', reason: 'budget: 600 of 600 minutes used' } } });
  await skipped.api.inboxTranscribe('plaud', 'r1');
  assert.deepStrictEqual(skipped.calls.toasts[0], ['Not transcribed: budget: 600 of 600 minutes used', 'error']);

  const denied = itemHarness({ status: 412, body: { error: 'transcription is not enabled on this hive' } });
  await denied.api.inboxTranscribe('plaud', 'r1');
  assert.deepStrictEqual(denied.calls.toasts[0], ['Transcription failed: transcription is not enabled on this hive', 'error']);
  assert.strictEqual(denied.calls.refreshes, 1);
  assert.ok(denied.ctx._inboxContent['plaud/r1'], 'cached content kept on failure');

  const busy = itemHarness();
  busy.ctx._inboxTranscribing.add('plaud/r1');
  await busy.api.inboxTranscribe('plaud', 'r1');
  assert.strictEqual(busy.calls.fetches.length, 0, 'a second click while in flight is ignored');
});

// formRoot fakes the settings form: querySelector by data attribute.
const formRoot = fields => ({
  querySelector: sel => {
    if (sel === '[data-inbox-tx-section]') return {};
    const m = /data-inbox-tx-field="([^"]+)"/.exec(sel);
    return m && fields[m[1]] !== undefined ? fields[m[1]] : null;
  },
});

const settingsHarness = () => new Function('escapeHtml', 'yamlHint', 'INBOX_TX_BYTES_PER_MB',
  fnSrc('inboxTranscriptionFormHtml') + '\n' + fnSrc('inboxTranscriptionUsageHtml') + '\n' + fnSrc('inboxReadTranscription') + '\nreturn { inboxTranscriptionFormHtml, inboxTranscriptionUsageHtml, inboxReadTranscription };',
)(escapeHtml, p => '<code>' + p + '</code>', BYTES_PER_MB);

test('settings section shows the consent notice, key status and every yaml key', () => {
  const { inboxTranscriptionFormHtml } = settingsHarness();
  let out = inboxTranscriptionFormHtml({ enabled: false, api_key_env: 'MY_DG', key_present: false, max_bytes: 524288000, sources: ['plaud'] });
  assert.match(out, /other people's voices/);
  assert.match(out, /Deepgram under the hive owner's Deepgram account and key/);
  assert.match(out, /data-inbox-tx-field="consent_acknowledged">/);
  assert.match(out, /\$MY_DG<\/code>: <span class="kc-pill" data-status="error">key missing/);
  assert.match(out, /value="500"/, 'max_bytes shown in MB');
  assert.match(out, /value="plaud"/);
  for (const key of ['enabled', 'mode', 'sources[]', 'api_key_env', 'provider', 'model', 'diarize', 'language', 'max_duration_minutes', 'max_bytes', 'monthly_budget_minutes', 'daily_max_items', 'consent_acknowledged']) {
    assert.ok(out.includes('inbox.transcription.' + key), 'yaml hint for ' + key);
  }
  out = inboxTranscriptionFormHtml({ enabled: true, consent_acknowledged: true, key_present: true, mode: 'auto' });
  assert.match(out, /key present/);
  assert.match(out, /data-inbox-tx-field="enabled" checked|data-inbox-tx-field="enabled"[^>]* checked/);
  assert.match(out, /<option value="auto" selected>/);
  assert.match(inboxTranscriptionFormHtml(undefined), /DEEPGRAM_API_KEY/);
});

test('reading the section enforces consent and converts the size limit', () => {
  const { inboxReadTranscription } = settingsHarness();
  const base = {
    enabled: { checked: true }, consent_acknowledged: { checked: false }, mode: { value: 'auto' }, model: { value: 'nova-3' },
    api_key_env: { value: ' MY_DG ' }, sources: { value: 'plaud, zoom,' }, diarize: { checked: true }, language: { value: 'auto' },
    monthly_budget_minutes: { value: '300' }, daily_max_items: { value: '-2' }, max_duration_minutes: { value: '90' }, max_mb: { value: '10' },
  };
  const refused = inboxReadTranscription(formRoot(base), {});
  assert.match(refused.error, /consent box/);
  const ok = inboxReadTranscription(formRoot(Object.assign({}, base, { consent_acknowledged: { checked: true } })), { provider: 'deepgram' });
  assert.deepStrictEqual(ok.transcription, {
    enabled: true, consent_acknowledged: true, mode: 'auto', provider: 'deepgram', model: 'nova-3', api_key_env: 'MY_DG',
    sources: ['plaud', 'zoom'], diarize: true, language: 'auto', monthly_budget_minutes: 300, daily_max_items: 0,
    max_duration_minutes: 90, max_bytes: 10 * BYTES_PER_MB,
  });
  const off = inboxReadTranscription(formRoot({ enabled: { checked: false } }), null);
  assert.strictEqual(off.transcription.enabled, false, 'disabling needs no consent');
  assert.strictEqual(off.transcription.mode, 'manual');
  assert.strictEqual(inboxReadTranscription(null), null);
  assert.strictEqual(inboxReadTranscription({ querySelector: () => null }), null, 'no section, no transcription payload');
});

test('settings section shows month usage, egress and the auto-mode source hint', () => {
  const { inboxTranscriptionFormHtml, inboxTranscriptionUsageHtml } = settingsHarness();
  let out = inboxTranscriptionUsageHtml({ month_minutes_used: 123.456, monthly_budget_minutes: 600, today_items: 3, daily_max_items: 20, egress_hosts: ['api.deepgram.com'] });
  assert.match(out, /data-status="ok">123\.5 of 600 minutes used/);
  assert.match(out, /today: 3 of 20 recordings/);
  assert.match(out, /<code>api\.deepgram\.com:443<\/code>/);
  assert.doesNotMatch(out, /none are listed/);
  out = inboxTranscriptionUsageHtml({ month_minutes_used: 590, monthly_budget_minutes: 600, mode: 'auto', sources: [] });
  assert.match(out, /data-status="syncing">590 of 600/);
  assert.match(out, /none are listed/);
  assert.doesNotMatch(out, /Egress/);
  assert.match(inboxTranscriptionUsageHtml({ month_minutes_used: 700, monthly_budget_minutes: 600 }), /data-status="error"/);
  assert.match(inboxTranscriptionUsageHtml(undefined), /0 of 0 minutes used/);
  assert.match(inboxTranscriptionFormHtml({ month_minutes_used: 1, monthly_budget_minutes: 600 }), /data-inbox-tx-usage="1"/);
});
