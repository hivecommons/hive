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
    fnSrc('inboxViewerCanPlay') + '\n' + fnSrc('inboxApiPath') + '\n' + fnSrc('inboxTxUnavailableText') + '\n' + fnSrc('inboxTxEtaText') + '\n' + fnSrc('inboxTxElapsedText') + '\n' + fnSrc('inboxTxProgressHtml') + '\n' + fnSrc('inboxIntentThen') + '\n' + fnSrc('inboxIntentLabel') + '\n' + fnSrc('inboxTranscriptionHtml') + '\n' + fnSrc('inboxTranscribe') + '\n' +
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
  fnSrc('inboxTxProviderInfo') + '\n' + fnSrc('inboxTxConsentText') + '\n' + fnSrc('inboxTxProviderHtml') + '\n' +
  fnSrc('inboxTranscriptionFormHtml') + '\n' + fnSrc('inboxTranscriptionUsageHtml') + '\n' + fnSrc('inboxReadTranscription') + '\nreturn { inboxTranscriptionFormHtml, inboxTranscriptionUsageHtml, inboxReadTranscription, inboxTxConsentText, inboxTxProviderHtml };',
)(escapeHtml, p => '<code>' + p + '</code>', BYTES_PER_MB);

test('settings section shows the consent notice, key status and every yaml key', () => {
  const { inboxTranscriptionFormHtml } = settingsHarness();
  let out = inboxTranscriptionFormHtml({ enabled: false, provider: 'deepgram', api_key_env: 'MY_DG', key_present: false, max_bytes: 524288000, sources: ['plaud'] });
  assert.match(out, /other people's voices/);
  assert.match(out, /Deepgram under the hive owner's Deepgram account and key/);
  assert.match(out, /data-inbox-tx-field="consent_acknowledged">/);
  assert.match(out, /\$MY_DG<\/code>: <span class="kc-pill" data-status="error">key missing/);
  assert.match(out, /value="500"/, 'max_bytes shown in MB');
  assert.match(out, /value="plaud"/);
  for (const key of ['enabled', 'mode', 'sources[]', 'api_key_env', 'provider', 'model', 'whisper_model', 'whisper_threads', 'gemini_model', 'diarize', 'language', 'max_duration_minutes', 'max_bytes', 'monthly_budget_minutes', 'daily_max_items', 'consent_acknowledged']) {
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
    provider: { value: 'deepgram' }, api_key_env: { value: ' MY_DG ' }, sources: { value: 'plaud, zoom,' }, diarize: { checked: true }, language: { value: 'auto' },
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
  assert.strictEqual(off.transcription.provider, 'auto', 'no selector and no current value means auto');
  const kept = inboxReadTranscription(formRoot({ enabled: { checked: false } }), { provider: 'deepgram' });
  assert.strictEqual(kept.transcription.provider, 'deepgram', 'an existing explicit provider is kept');
  const picked = inboxReadTranscription(formRoot({ enabled: { checked: false }, provider: { value: 'whisper' } }), { provider: 'deepgram' });
  assert.strictEqual(picked.transcription.provider, 'whisper', 'the selector wins');
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

const TX_REASONS = [
  ['disabled', /Transcription is off: enable it in Inbox ⚙ → Transcription/],
  ['no_key', /The transcription provider is not ready on this hive: see Inbox ⚙ → Transcription/],
  ['no_consent', /Consent not acknowledged/],
  ['budget', /Monthly budget used/],
];

test('unavailable transcription shows a disabled button, the reason and a setup link for owner and read-write', () => {
  for (const [code, re] of TX_REASONS) {
    for (const role of ['owner', 'read-write']) {
      const { api } = itemHarness({ role });
      const out = api.inboxTranscriptionHtml({ source: 'plaud', id: 'r1', pending: true, transcribe_unavailable: code }, 'plaud/r1');
      assert.match(out, /<button[^>]*inbox-transcribe[^>]*disabled/, code + ' ' + role);
      assert.doesNotMatch(out, /data-action="inboxTranscribe"/, 'disabled control must not fire the action');
      assert.match(out, re, code);
      assert.match(out, /data-action="inboxOpenSettings"/, 'setup link for ' + role);
    }
  }
});

test('read-only viewers see only the reason text, no control or link', () => {
  for (const [code, re] of TX_REASONS) {
    for (const role of ['read-only', '']) {
      const { api } = itemHarness({ role });
      const out = api.inboxTranscriptionHtml({ source: 'plaud', id: 'r1', pending: true, transcribe_unavailable: code }, 'plaud/r1');
      assert.match(out, re, code);
      assert.doesNotMatch(out, /<button/, code + ' no control for ' + role);
    }
  }
});

test('no reason means no hint, and an unknown code renders nothing', () => {
  const { api } = itemHarness({ role: 'owner' });
  assert.strictEqual(api.inboxTranscriptionHtml({ source: 'plaud', id: 'r1', pending: true }, 'plaud/r1'), '');
  assert.strictEqual(api.inboxTranscriptionHtml({ source: 'plaud', id: 'r1', pending: true, transcribe_unavailable: 'bogus' }, 'plaud/r1'), '');
});

test('pending text is provider-neutral for every source and keeps the upstream hint', () => {
  const ctx = { _inboxContent: {}, _inboxExpanded: new Set() };
  const f = new Function('escapeHtml', 'ctx', 'const _inboxContent = ctx._inboxContent; const _inboxExpanded = ctx._inboxExpanded; const INBOX_SUMMARY_COLLAPSE_CHARS = 280;\n' + fnSrc('inboxSummaryHtml') + '\nreturn inboxSummaryHtml;')(escapeHtml, ctx);
  const upstream = { plaud: /Plaud app/, zoom: /Zoom has no transcript/, teams: /Teams has no transcript/, discord: /Discord has no built-in transcription/, 'google-meet': /Google Meet has no transcript/, webex: /Webex has no transcript/, fireflies: /Fireflies has no transcript/, fathom: /Fathom has no transcript/, otter: /Otter has no transcript/ };
  for (const [src, re] of Object.entries(upstream)) {
    let out = f({ source: src, id: 'x', pending: true, transcribable: true }, src + '/x');
    assert.match(out, re, src + ' upstream hint');
    assert.match(out, /Hive can transcribe this: click 🎧 Transcribe/, src);
    assert.doesNotMatch(out, /Deepgram|Gemini|whisper/i, src + ' names no provider');
    out = f({ source: src, id: 'x', pending: true, transcribe_unavailable: 'disabled' }, src + '/x');
    assert.match(out, /Hive can transcribe this once transcription is set up/, src);
    out = f({ source: src, id: 'x', pending: true }, src + '/x');
    assert.doesNotMatch(out, /Hive can transcribe/, src + ' no audio, no transcription claim');
  }
});

// progressHarness extracts the progress helpers and the live poll merge.
const progressHarness = () => new Function('escapeHtml',
  'function inboxTxBusy(st) { return !!st && (st.status === "queued" || st.status === "running"); }\n' +
  fnSrc('inboxTxEtaText') + '\n' + fnSrc('inboxTxElapsedText') + '\n' + fnSrc('inboxTxProgressHtml') + '\n' + fnSrc('inboxIntentThen') + '\n' + fnSrc('inboxIntentLabel') + '\n' + fnSrc('inboxTxApplyActive') + '\n' +
  'return { inboxTxEtaText, inboxTxElapsedText, inboxTxProgressHtml, inboxTxApplyActive };')(escapeHtml);

test('ETA text rounds the way people say it', () => {
  const { inboxTxEtaText } = progressHarness();
  const cases = [[undefined, ''], [0, ''], [-5, ''], [12, 'less than a minute left'], [59, 'less than a minute left'], [60, 'about 1 min left'],
    [89, 'about 1 min left'], [150, 'about 3 min left'], [540, 'about 9 min left'], [610, 'about 10 min left'], [1400, 'about 25 min left'],
    [3600, 'about 1 h left'], [4300, 'about 1 h 10 min left'], [7300, 'about 2 h left']];
  for (const [eta, want] of cases) assert.strictEqual(inboxTxEtaText(eta), want, 'eta ' + eta);
});

test('elapsed text counts from started_at', () => {
  const { inboxTxElapsedText } = progressHarness();
  const t0 = Date.parse('2026-11-15T10:00:00Z');
  assert.strictEqual(inboxTxElapsedText('2026-11-15T10:00:00Z', t0 + 42000), '42s');
  assert.strictEqual(inboxTxElapsedText('2026-11-15T10:00:00Z', t0 + 125000), '2m 5s');
  assert.strictEqual(inboxTxElapsedText('2026-11-15T10:00:00Z', t0 + 3725000), '1h 2m');
  assert.strictEqual(inboxTxElapsedText('', t0), '');
  assert.strictEqual(inboxTxElapsedText('not a date', t0), '');
});

test('progress bar: running shows phase, percent, time left and elapsed, accessibly', () => {
  const { inboxTxProgressHtml } = progressHarness();
  const t0 = Date.parse('2026-11-15T10:00:00Z');
  const out = inboxTxProgressHtml({ status: 'running', phase: 'transcribing', progress: 0.423, eta_seconds: 150, started_at: '2026-11-15T10:00:00Z' }, t0 + 80000);
  assert.match(out, /role="progressbar"/);
  assert.match(out, /aria-valuenow="42"/);
  assert.match(out, /aria-valuemin="0" aria-valuemax="100"/);
  assert.match(out, /<progress class="kc-progress-bar" max="100" value="42" aria-hidden="true">/);
  assert.match(out, /Transcribing · 42%/);
  assert.match(out, /<span data-inbox-tx-eta aria-live="polite">about 3 min left<\/span>/);
  assert.match(out, /kc-progress-elapsed" title="elapsed">1m 20s</);
  assert.match(inboxTxProgressHtml({ status: 'running', phase: 'downloading', progress: 0, started_at: '2026-11-15T10:00:00Z' }, t0), /Downloading audio · 0%.*estimating time left/);
  assert.match(inboxTxProgressHtml({ status: 'running', phase: 'decoding', progress: 0.01, eta_seconds: 30, started_at: '2026-11-15T10:00:00Z' }, t0), /Preparing audio · 1%.*less than a minute left/);
  assert.match(inboxTxProgressHtml({ status: 'running', phase: 'finalizing', progress: 0.99, started_at: '2026-11-15T10:00:00Z' }, t0), /Finishing · 99%.*almost done/);
  assert.match(inboxTxProgressHtml({ status: 'running', phase: 'transcribing', progress: 0.95, estimated: true, started_at: '2026-11-15T10:00:00Z' }, t0), /finishing up/, 'an estimate past its expected time');
  assert.match(inboxTxProgressHtml({ status: 'running', phase: 'transcribing', progress: 1.7, started_at: 'x' }, t0), /aria-valuenow="100"/, 'clamped');
  assert.doesNotMatch(inboxTxProgressHtml({ status: 'running', phase: 'transcribing', progress: 0.5, started_at: '' }, t0), /kc-progress-elapsed/);
  assert.match(inboxTxProgressHtml({ status: 'running', phase: '<b>', progress: 0.5, started_at: '2026-11-15T10:00:00Z' }, t0), /data-phase="&lt;b&gt;"/, 'escaped');
});

test('progress bar: queued shows the place in line; done/failed show none', () => {
  const { inboxTxProgressHtml } = progressHarness();
  let out = inboxTxProgressHtml({ status: 'queued', queue_position: 2 });
  assert.match(out, /Waiting: 2 transcriptions ahead \(one runs at a time\)/);
  assert.match(out, /role="progressbar"/);
  assert.match(out, /<progress class="kc-progress-bar" aria-hidden="true"><\/progress>/, 'indeterminate while queued');
  assert.match(inboxTxProgressHtml({ status: 'queued', queue_position: 1 }), /Waiting: 1 transcription ahead/);
  assert.match(inboxTxProgressHtml({ status: 'queued' }), /Starting…/);
  for (const status of ['done', 'failed', 'skipped']) assert.strictEqual(inboxTxProgressHtml({ status, progress: 1 }), '', status);
  assert.strictEqual(inboxTxProgressHtml(null), '');
});

test('item shows the progress bar while busy and Retry with the reason on failure', () => {
  for (const role of ['owner', 'read-write', 'read-only']) {
    const { api } = itemHarness({ role });
    const running = api.inboxTranscriptionHtml({ source: 'plaud', id: 'r1', transcription: { status: 'running', phase: 'transcribing', progress: 0.5, eta_seconds: 70, started_at: '2026-11-15T10:00:00Z' } }, 'plaud/r1');
    assert.match(running, /data-inbox-tx-progress/, role + ' sees progress');
    assert.match(running, /data-inbox-tx-slot="plaud\/r1"/);
    assert.doesNotMatch(running, /data-action="inboxTranscribe"/, 'no button while running');
    const failed = api.inboxTranscriptionHtml({ source: 'plaud', id: 'r1', transcribable: true, transcription: { status: 'failed', reason: 'whisper: timed out' } }, 'plaud/r1');
    assert.match(failed, /role="alert">whisper: timed out/);
    assert.doesNotMatch(failed, /data-inbox-tx-progress/);
    assert.strictEqual(/Retry transcription/.test(failed), role !== 'read-only', role + ' retry');
  }
});

test('live poll merge updates busy items and reports finished ones', () => {
  const { inboxTxApplyActive } = progressHarness();
  const items = [
    { source: 'plaud', id: 'a', transcription: { status: 'running', attempts: 1, progress: 0.1 } },
    { source: 'plaud', id: 'b', transcription: { status: 'queued', attempts: 1 } },
    { source: 'plaud', id: 'c', transcription: { status: 'done' } },
    { source: 'plaud', id: 'd' },
  ];
  let finished = inboxTxApplyActive(items, [{ source: 'plaud', id: 'a', state: { status: 'running', progress: 0.6, eta_seconds: 40 } }, { source: 'plaud', id: 'b', state: { status: 'queued', queue_position: 1 } }]);
  assert.deepStrictEqual(finished, []);
  assert.strictEqual(items[0].transcription.progress, 0.6);
  assert.strictEqual(items[0].transcription.attempts, 1, 'fields the poll omits are kept');
  assert.strictEqual(items[1].transcription.queue_position, 1);
  finished = inboxTxApplyActive(items, [{ source: 'plaud', id: 'b', state: { status: 'running', progress: 0.05 } }]);
  assert.deepStrictEqual(finished, ['plaud/a'], 'a left the active list: re-read the Inbox');
  assert.deepStrictEqual(inboxTxApplyActive(items, null), ['plaud/a', 'plaud/b']);
});

test('settings: provider selector with availability and provider-specific consent', () => {
  const { inboxTranscriptionFormHtml, inboxTxConsentText } = settingsHarness();
  const providers = [
    { name: 'whisper', available: true, status: 'ready', detail: 'model will download on first use' },
    { name: 'gemini', available: false, status: 'missing_credentials', detail: 'no GEMINI_API_KEY' },
    { name: 'deepgram', available: true, status: 'ready', detail: 'key present in $DEEPGRAM_API_KEY' },
  ];
  const out = inboxTranscriptionFormHtml({ provider: 'auto', resolved_provider: 'whisper', providers });
  assert.match(out, /data-inbox-tx-field="provider"/);
  for (const p of ['auto', 'whisper', 'gemini', 'deepgram']) assert.match(out, new RegExp('<option value="' + p + '"'), p);
  assert.match(out, /<option value="auto" selected>auto \(now: whisper\)<\/option>/);
  assert.match(out, /<option value="gemini">gemini \(not ready\)<\/option>/);
  assert.match(out, /data-inbox-tx-provider="whisper"><strong>whisper<\/strong>: Built in, no account[^<]*never leaves this hive\. <span class="kc-pill" data-status="ok">ready<\/span> model will download on first use/);
  assert.match(out, /data-inbox-tx-provider="gemini">[^]*data-status="error">not ready<\/span> no GEMINI_API_KEY/);
  assert.match(out, /Auto currently picks whisper\. With whisper the audio stays on this hive/);
  assert.match(inboxTranscriptionFormHtml({ provider: 'bogus' }), /<option value="auto" selected>/, 'unknown provider shows auto');
  assert.match(inboxTxConsentText('whisper'), /stays on this hive/);
  assert.match(inboxTxConsentText('gemini'), /to Google \(the Gemini API\) under the hive's own Gemini credentials/);
  assert.match(inboxTxConsentText('deepgram'), /to Deepgram under the hive owner's Deepgram account/);
  assert.match(inboxTxConsentText('auto', ''), /stays on this hive \(whisper\) or is sent to Google \(Gemini\) or Deepgram/);
});

test('settings: whisper model and threads round-trip only when the form has them', () => {
  const { inboxTranscriptionFormHtml, inboxReadTranscription } = settingsHarness();
  const out = inboxTranscriptionFormHtml({ provider: 'whisper', whisper_model: 'small.en', whisper_threads: 3 });
  assert.match(out, /data-inbox-tx-field="whisper_model"/);
  assert.match(out, /<option value="small.en" selected>small.en \(488 MB, about 3x slower\)<\/option>/);
  assert.match(out, /<option value="">default: base.en/);
  assert.match(out, /data-inbox-tx-field="whisper_threads"[^>]*value="3"/);
  const read = inboxReadTranscription(formRoot({ enabled: { checked: false }, provider: { value: 'whisper' }, whisper_model: { value: 'base' }, whisper_threads: { value: '2' } }), {});
  assert.strictEqual(read.transcription.whisper_model, 'base');
  assert.strictEqual(read.transcription.whisper_threads, 2);
  const legacy = inboxReadTranscription(formRoot({ enabled: { checked: false } }), {});
  assert.ok(!('whisper_model' in legacy.transcription) && !('whisper_threads' in legacy.transcription), 'no whisper keys without the fields');
});

test('settings: gemini model round-trips', () => {
  const { inboxTranscriptionFormHtml, inboxReadTranscription } = settingsHarness();
  assert.match(inboxTranscriptionFormHtml({ gemini_model: 'gemini-3.6-flash' }), /data-inbox-tx-field="gemini_model"[^>]*value="gemini-3.6-flash"/);
  const read = inboxReadTranscription(formRoot({ enabled: { checked: false }, gemini_model: { value: ' gemini-3.6-flash ' } }), {});
  assert.strictEqual(read.transcription.gemini_model, 'gemini-3.6-flash');
});
