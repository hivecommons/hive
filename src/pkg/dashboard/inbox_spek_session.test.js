// Inbox → Inception (spek) session: the Share → Hive → spek… entry (single
// item and the selection bar), the dialog (new vs running session), the POST
// it sends, the hand-over to the session tab, and the "not transcribed yet"
// line hidden once an item has text. Functions are extracted from the
// generated page and run in a sandbox with their helpers injected.
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
// constSrc cuts one top-level `const NAME = ...;` declaration (a one-liner,
// or an array literal closed by `];`).
const constSrc = name => {
  const start = html.indexOf('const ' + name + ' =');
  assert.ok(start >= 0, name + ' not found');
  const open = html.slice(start, html.indexOf('\n', start));
  const end = open.trim().endsWith('[') ? html.indexOf('\n    ];', start) + '\n    ];'.length : html.indexOf(';\n', start) + 1;
  return html.slice(start, end);
};

const escapeHtml = s => String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');

const SESSION_FNS = ['inboxSessionCanSend', 'inboxKeyRef', 'inboxSessionName', 'inboxSessionLastVendor', 'inboxSessionPickVendor', 'inboxSessionTargets',
  'inboxSessionDialogHtml', 'inboxOpenSessionDialog', 'inboxOpenSessionSelected', 'inboxSessionRequest', 'inboxSessionErrorText', 'inboxConfirmSession', 'inboxOpenSession'];

// harness builds the dialog functions with a fake DOM, fetch and storage.
const harness = ({ role = 'owner', sessions = [], vendors = [{ id: 'claude', label: 'Claude Code', available: true }, { id: 'codex', label: 'Codex', available: true }], lastVendor = '', items = [], post = { status: 202, body: { ok: true, session: { id: 'abc123', name: 'nate v2', status: 'preparing', vendor: 'claude' } } }, form = {} } = {}) => {
  const calls = { fetches: [], toasts: [], modals: [], closed: 0, focused: [], refreshes: 0, stored: {} };
  const elements = Object.assign({ 'inbox-session-error': { textContent: '' } }, form.fields || {});
  const document = {
    getElementById: id => elements[id] || null,
    querySelector: sel => sel === '[data-inbox-session-mode]:checked' ? { value: form.mode || 'new' } : null,
  };
  const localStorage = { getItem: k => (k === 'hive.sessions.lastVendor' ? lastVendor : null), setItem: (k, v) => { calls.stored[k] = v; } };
  const fetch = async (url, opts) => {
    calls.fetches.push({ url, opts });
    if (!opts || !opts.method) return { ok: true, status: 200, json: async () => ({ enabled: true, sessions, vendors }) };
    return { ok: post.status >= 200 && post.status < 300, status: post.status, json: async () => post.body };
  };
  const ctx = { window: { _hiveRole: role }, selected: new Set(items.map(i => i.source + '/' + i.id)) };
  const h = new Function('ctx', 'document', 'localStorage', 'fetch', 'escapeHtml', 'showToast', 'kbShowModal', 'kbCloseModal', 'sessionsFocus', 'fetchInbox', 'inboxFind', '_inceptionAuthHeaders',
    'const window = ctx.window; const _inboxSelected = ctx.selected; let _inboxSessionKeys = [];\n' +
    ['INBOX_SESSION_GROUP', 'INBOX_SESSION_VENDOR_KEY', 'INBOX_SESSION_NAME_MAX'].map(constSrc).join('\n') + '\n' +
    SESSION_FNS.map(fnSrc).join('\n') + '\nreturn {' + SESSION_FNS.join(',') + ', keys: () => _inboxSessionKeys };');
  const api = h(ctx, document, localStorage, fetch, escapeHtml,
    (msg, kind) => calls.toasts.push([msg, kind]),
    (title, body) => calls.modals.push({ title, body }),
    () => { calls.closed++; },
    s => calls.focused.push(s),
    async () => { calls.refreshes++; },
    (src, id) => items.find(i => i.source === src && i.id === id),
    () => ({ 'Content-Type': 'application/json' }));
  return { api, calls, ctx, elements };
};

const ITEM = { source: 'plaud', id: 'rec-1', title: 'nate v2 - workflow - cost considerations' };
const ITEM2 = { source: 'plaud', id: 'rec-2', title: 'follow-up' };
const RUNNING = { id: 'run1', name: 'jam', group: 'rfc', vendor: 'codex', status: 'running' };
const ENDED = { id: 'old1', name: 'old', vendor: 'claude', status: 'ended' };

test('new-session dialog: name from the title, group inbox, last-used vendor, running sessions only', async () => {
  const { api, calls } = harness({ items: [ITEM], sessions: [RUNNING, ENDED], lastVendor: 'codex' });
  await api.inboxOpenSessionDialog(['plaud/rec-1']);
  assert.strictEqual(calls.modals.length, 1);
  const { title, body } = calls.modals[0];
  assert.strictEqual(title, 'Send to an Inception (spek) session');
  assert.match(body, /id="inbox-session-name"[^>]*value="nate v2 - workflow - cost considerations"/);
  assert.match(body, /id="inbox-session-group"[^>]*value="inbox"/);
  assert.match(body, /<option value="codex" selected>Codex<\/option>/, 'last-used vendor preselected');
  assert.match(body, /value="new" checked/);
  assert.match(body, /<option value="run1">jam · rfc \(codex\)<\/option>/);
  assert.doesNotMatch(body, /old1/, 'ended sessions are not offered');
  assert.doesNotMatch(body, /value="existing"[^>]*disabled/);
  assert.deepStrictEqual(api.keys(), ['plaud/rec-1']);
});

test('vendor falls back to the first available one; no running session disables "add to running"', () => {
  const { api } = harness();
  assert.strictEqual(api.inboxSessionPickVendor([{ id: 'goose', available: false }, { id: 'claude', available: true }], 'goose'), 'claude');
  assert.strictEqual(api.inboxSessionPickVendor([], 'claude'), '');
  const body = api.inboxSessionDialogHtml([ITEM], { vendors: [{ id: 'claude', available: true }], sessions: [ENDED] }, '');
  assert.match(body, /value="existing"[^>]*disabled/);
  assert.match(body, /No running session you can type into/);
  const long = api.inboxSessionName([{ title: 'x'.repeat(200) }]);
  assert.strictEqual(long.length, 64);
});

test('read role never opens the dialog', async () => {
  const { api, calls } = harness({ role: 'read', items: [ITEM] });
  await api.inboxOpenSessionDialog(['plaud/rec-1']);
  assert.strictEqual(calls.modals.length, 0);
  assert.strictEqual(calls.fetches.length, 0);
});

test('multi-select seeds one session from every selected item', async () => {
  const { api, calls } = harness({ items: [ITEM, ITEM2] });
  await api.inboxOpenSessionSelected();
  assert.deepStrictEqual(api.keys().sort(), ['plaud/rec-1', 'plaud/rec-2']);
  assert.match(calls.modals[0].body, /<strong>2 items<\/strong>/);
  assert.match(calls.modals[0].body, /<li>follow-up<\/li>/);
});

test('request shape: new session carries seed_items, a running one posts to /seed', () => {
  const { api } = harness();
  const nw = api.inboxSessionRequest('new', ['plaud/rec-1', 'plaud/a/b'], { name: 'n', group: 'inbox', vendor: 'claude' });
  assert.strictEqual(nw.path, '/api/sessions');
  assert.deepStrictEqual(nw.body, { name: 'n', group: 'inbox', vendor: 'claude', shared: false, seed_items: [{ source: 'plaud', id: 'rec-1' }, { source: 'plaud', id: 'a/b' }] });
  const ex = api.inboxSessionRequest('existing', ['plaud/rec-1'], { session: 'run1' });
  assert.strictEqual(ex.path, '/api/sessions/run1/seed');
  assert.deepStrictEqual(ex.body, { items: [{ source: 'plaud', id: 'rec-1' }] });
});

test('confirm (new): toast, session tab focused, selection cleared, vendor remembered', async () => {
  const fields = { 'inbox-session-name': { value: 'nate v2' }, 'inbox-session-group': { value: 'inbox' }, 'inbox-session-vendor': { value: 'claude' }, 'inbox-session-existing': { value: '' } };
  const { api, calls, ctx } = harness({ items: [ITEM, ITEM2], form: { mode: 'new', fields } });
  await api.inboxOpenSessionSelected();
  await api.inboxConfirmSession();
  const post = calls.fetches.find(f => f.opts && f.opts.method === 'POST');
  assert.strictEqual(post.url, '/api/sessions');
  assert.strictEqual(JSON.parse(post.opts.body).seed_items.length, 2);
  assert.deepStrictEqual(calls.toasts, [['Sent 2 items to nate v2', 'success']]);
  assert.strictEqual(calls.closed, 1);
  assert.deepStrictEqual(calls.focused.map(s => s.id), ['abc123'], 'navigates to the new session tab');
  assert.strictEqual(ctx.selected.size, 0);
  assert.strictEqual(calls.stored['hive.sessions.lastVendor'], 'claude');
});

test('confirm (running session): posts to /seed and focuses that tab', async () => {
  const fields = { 'inbox-session-existing': { value: 'run1' } };
  const { api, calls } = harness({ items: [ITEM], sessions: [RUNNING], form: { mode: 'existing', fields }, post: { status: 202, body: { ok: true, session: RUNNING } } });
  await api.inboxOpenSessionDialog(['plaud/rec-1']);
  await api.inboxConfirmSession();
  const post = calls.fetches.find(f => f.opts && f.opts.method === 'POST');
  assert.strictEqual(post.url, '/api/sessions/run1/seed');
  assert.deepStrictEqual(calls.toasts, [['Sent 1 item to jam', 'success']]);
  assert.deepStrictEqual(calls.focused, [RUNNING]);
});

test('items transcribing first are named in the toast', async () => {
  const fields = { 'inbox-session-existing': { value: 'run1' } };
  const { api, calls } = harness({ items: [ITEM, ITEM2], form: { mode: 'existing', fields }, post: { status: 202, body: { ok: true, session: RUNNING, seed: { files: ['.hive/inbox/plaud-rec-1.md'], waiting: ['plaud/rec-2'] } } } });
  await api.inboxOpenSessionSelected();
  await api.inboxConfirmSession();
  assert.deepStrictEqual(calls.toasts, [['Sent 2 items to jam (1 transcribing first; it goes in when the transcript is ready)', 'success']]);
});

test('an item without text keeps the dialog open with the Transcribe hint', async () => {
  const fields = { 'inbox-session-existing': { value: 'run1' } };
  const { api, calls, elements } = harness({ items: [ITEM], form: { mode: 'existing', fields },
    post: { status: 409, body: { ok: false, error: 'plaud/rec-1: this item has no transcript or summary yet', reason: 'no_text', transcribe: true } } });
  await api.inboxOpenSessionDialog(['plaud/rec-1']);
  await api.inboxConfirmSession();
  assert.match(elements['inbox-session-error'].textContent, /no transcript or summary yet.*🎧 Transcribe/);
  assert.strictEqual(calls.closed, 0);
  assert.strictEqual(calls.focused.length, 0);
});

test('Share → Hive lists spek… second, for owners and read-write; audio-only items transcribe first', () => {
  const ctx = { window: { _hiveRole: 'owner' }, cache: { repos: ['o/r'], settings: {} }, opened: [] };
  const h = new Function('ctx', 'escapeHtml',
    'const window = ctx.window; const _inboxCache = ctx.cache; const _inboxSelected = new Set(["plaud/rec-1", "plaud/rec-2"]);\n' +
    'const inboxOpenRoute = () => {}; const inboxDismiss = () => {}; const inboxUndismiss = () => {}; const inboxDismissSelected = () => {}; const inboxOpenSettings = () => {};\n' +
    'const inboxShareCopy = () => {}; const inboxShareNative = () => {}; const inboxItemKey = it => it.source + "/" + it.id;\n' +
    'const inboxOpenSessionDialog = keys => ctx.opened.push(keys);\n' +
    constSrc('inboxShareHiveActions') + '\n' + constSrc('inboxShareLinkActions') + '\n' +
    ['inboxDibsReady', 'inboxCanShare', 'inboxTxBlocked', 'inboxTxUnavailableText', 'inboxShareGate', 'inboxIntentLabel', 'inboxShareMenuGroups', 'inboxBulkMenuGroups', 'inboxBulkMenuRun'].map(fnSrc).join('\n') + '\n' +
    'return { inboxShareHiveActions, inboxShareMenuGroups, inboxBulkMenuGroups, inboxBulkMenuRun };');
  const api = h(ctx, escapeHtml);
  const hive = g => (g.find(x => x.id === 'hive') || { items: [] }).items;
  let items = hive(api.inboxShareMenuGroups(ITEM));
  assert.deepStrictEqual(items.slice(0, 2).map(a => [a.id, a.label]), [['rfc', 'RFC…'], ['spek', 'spek…']]);
  ctx.window._hiveRole = 'read-write';
  assert.ok(hive(api.inboxShareMenuGroups(ITEM)).some(a => a.id === 'spek' && !a.disabled), 'read-write gets spek…');
  ctx.window._hiveRole = 'read';
  assert.deepStrictEqual(hive(api.inboxShareMenuGroups(ITEM)), [], 'read role: no Hive group');
  ctx.window._hiveRole = 'owner';
  const transcribable = hive(api.inboxShareMenuGroups(Object.assign({ pending: true, transcribable: true }, ITEM))).find(a => a.id === 'spek');
  assert.strictEqual(transcribable.disabled, false, 'audio-only and transcribable: enabled (transcribe first)');
  const blocked = hive(api.inboxShareMenuGroups(Object.assign({ pending: true, transcribe_unavailable: 'disabled' }, ITEM))).find(a => a.id === 'spek');
  assert.strictEqual(blocked.disabled, true);
  assert.match(blocked.hint, /transcript is needed first/);
  api.inboxShareHiveActions.find(a => a.id === 'spek').run(ITEM);
  assert.deepStrictEqual(ctx.opened.pop(), ['plaud/rec-1']);
  assert.ok(hive(api.inboxBulkMenuGroups()).some(a => a.id === 'spek' && a.label === 'spek…'), 'bulk Hive menu has spek…');
  api.inboxBulkMenuRun('spek');
  assert.deepStrictEqual(ctx.opened.pop(), ['plaud/rec-1', 'plaud/rec-2']);
});

test('the "not transcribed yet" line hides once the item has any text', () => {
  const ctx = { content: {} };
  const h = new Function('ctx', 'escapeHtml', 'INBOX_SUMMARY_COLLAPSE_CHARS',
    'const _inboxContent = ctx.content; const _inboxExpanded = new Set();\n' + fnSrc('inboxSummaryHtml') + '\nreturn { inboxSummaryHtml };');
  const api = h(ctx, escapeHtml, 280);
  const pending = { source: 'plaud', id: 'r', pending: true, transcribable: true };
  assert.match(api.inboxSummaryHtml(pending, 'plaud/r'), /inbox-pending/, 'no text: the line shows');
  assert.doesNotMatch(api.inboxSummaryHtml(Object.assign({}, pending, { transcription: { status: 'done', provider: 'whisper', model: 'base.en' } }), 'plaud/r'), /inbox-pending/, 'machine transcript done');
  assert.doesNotMatch(api.inboxSummaryHtml(Object.assign({}, pending, { has_transcript: true }), 'plaud/r'), /inbox-pending/);
  ctx.content['plaud/r'] = { pending: true, transcript: 'hello', transcript_label: 'Transcript (machine: whisper/base.en)' };
  assert.doesNotMatch(api.inboxSummaryHtml(pending, 'plaud/r'), /inbox-pending|not transcribed this recording/, 'loaded transcript');
});

test('sessionsFocus selects the tab, opens the section and keeps the known session', () => {
  const ctx = { data: { sessions: [{ id: 'run1', name: 'jam', status: 'running' }] }, selected: '', stored: {}, nav: [], renders: 0, loads: 0 };
  const h = new Function('ctx',
    'let _sessionsData = ctx.data; let _sessionsSelected = ctx.selected;\n' +
    'const SESSIONS_SELECTED_KEY = "hive.sessions.selected"; const SESSIONS_LAST_VENDOR_KEY = "hive.sessions.lastVendor";\n' +
    'const sessionsStoreSet = (k, v) => { ctx.stored[k] = v; }; const ocNavigate = s => ctx.nav.push(s);\n' +
    'const renderSessions = () => { ctx.renders++; }; const loadSessions = () => { ctx.loads++; };\n' +
    fnSrc('sessionsFocus') + '\nreturn { sessionsFocus, selected: () => _sessionsSelected };');
  const api = h(ctx);
  api.sessionsFocus({ id: 'run1' });
  assert.strictEqual(api.selected(), 'run1');
  assert.deepStrictEqual(ctx.nav, ['sessions-section']);
  assert.strictEqual(ctx.data.sessions[0].name, 'jam', 'a bare id does not wipe the known session');
  api.sessionsFocus({ id: 'new1', name: 'nate v2', status: 'preparing', vendor: 'claude' });
  assert.strictEqual(api.selected(), 'new1');
  assert.ok(ctx.data.sessions.some(s => s.id === 'new1' && s.status === 'preparing'), 'new session listed with its preparing state');
  assert.strictEqual(ctx.stored['hive.sessions.selected'], 'new1');
  assert.strictEqual(ctx.stored['hive.sessions.lastVendor'], 'claude');
  assert.ok(ctx.renders >= 2 && ctx.loads >= 2);
});
