// Sessions: deleting a session (#18767). The hive answers a DELETE at once
// (202) and removes the files in the background, so the UI must show the
// session as "Deleting…", remove the tab when a list no longer has it, and
// bring it back with the reason when the delete failed. These tests drive the
// shipped code (cut out of the generated dashboard) with fakes for the API,
// the confirm dialog and the toast.
// Run: node --test src/pkg/dashboard/*.test.js
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
const helpersStart = html.indexOf('const SESSIONS_API');
const helpersEnd = html.indexOf('// ── Storage conveniences');
assert.ok(helpersStart >= 0 && helpersEnd > helpersStart, 'sessions helper block not found in index.html');
const deleteStart = html.indexOf('// ── Close and delete ──');
const deleteEnd = html.indexOf('// ── Resume ──');
assert.ok(deleteStart > helpersEnd && deleteEnd > deleteStart, 'close-and-delete block not found in index.html');

// load builds one sandbox from the helper block and the close-and-delete
// block. api is what the fake sessionsApi does for a DELETE.
function load(sessions, api) {
  const toasts = [];
  const calls = [];
  const state = { renders: 0, loads: 0, confirms: [], confirmAnswer: true };
  const body = html.slice(helpersStart, helpersEnd) + '\n' + html.slice(deleteStart, deleteEnd)
    + '\nreturn { sessionsClose, sessionsDelete, sessionsDeleteRetry, sessionsSettleDeletes, sessionsViewMode, sessionsHasDeleting, sessionsDeleteErrorText,'
    + ' setData: (d) => { _sessionsData = d; }, data: () => _sessionsData, select: (id) => { _sessionsSelected = id; }, selected: () => _sessionsSelected,'
    + ' frames: _sessionsFrames, deleting: _sessionsDeleting };';
  const fn = new Function('window', 'showToast', 'hiveConfirm', 'sessionsApi', 'renderSessions', 'loadSessions', body);
  const h = fn({},
    (msg, type) => toasts.push({ msg, type }),
    (opts) => { state.confirms.push(opts); return Promise.resolve(state.confirmAnswer); },
    (p, method) => { calls.push([p, method]); return api(p, method); },
    () => { state.renders++; },
    () => { state.loads++; return Promise.resolve(); });
  h.setData({ enabled: true, sessions: sessions, vendors: [], limits: {}, workspace: {} });
  return { h, toasts, calls, state };
}

const S = (id, status, extra) => Object.assign({ id, name: 'name-' + id, group: '', vendor: 'claude', status }, extra || {});
const ids = (h) => h.data().sessions.map((s) => s.id);

test('deleting is its own view mode and speeds up the list poll', () => {
  const { h } = load([], () => Promise.resolve({}));
  assert.strictEqual(h.sessionsViewMode('owner', S('a', 'deleting')), 'deleting');
  assert.strictEqual(h.sessionsHasDeleting([S('a', 'ended'), S('b', 'deleting')]), true);
  assert.strictEqual(h.sessionsHasDeleting([S('a', 'ended'), null]), false);
  assert.strictEqual(h.sessionsHasDeleting(undefined), false);
  const poll = html.slice(html.indexOf('function sessionsSyncPreparingPoll('), html.indexOf('function sessionsSummary('));
  assert.match(poll, /sessionsHasDeleting\(sessions\)/, 'the list is polled faster while a session is deleting');
  assert.doesNotMatch(poll.replace(/dashboardSetInterval/g, ''), /setInterval/, 'no raw setInterval');
});

test('a 202 shows the tab as deleting (no toast yet); the tab goes and a toast shows when a list no longer has it', async () => {
  const sessions = [S('a', 'ended'), S('b', 'running')];
  const env = load(sessions, () => Promise.resolve({ ok: true, deleting: true, session: S('a', 'deleting') }));
  env.h.select('a');
  env.h.frames.a = { remove() {} };
  await env.h.sessionsClose('a');

  assert.strictEqual(env.state.confirms.length, 1);
  assert.match(env.state.confirms[0].title, /Delete this session\?/);
  assert.deepStrictEqual(env.calls, [['/a', 'DELETE']]);
  assert.strictEqual(env.h.data().sessions[0].status, 'deleting', 'the tab stays, marked deleting');
  assert.deepStrictEqual(ids(env.h), ['a', 'b']);
  assert.ok(env.state.renders >= 1 && env.state.loads >= 1, 'the section is re-rendered and the list refreshed');
  assert.deepStrictEqual(env.toasts, [], 'nothing is announced before the hive is done');

  // Still deleting on the next poll: nothing to say.
  env.h.sessionsSettleDeletes([S('a', 'deleting'), S('b', 'running')]);
  assert.deepStrictEqual(env.toasts, []);

  // The hive finished: the list no longer has the session.
  env.h.sessionsSettleDeletes([S('b', 'running')]);
  assert.deepStrictEqual(env.toasts, [{ msg: 'Session name-a deleted.', type: 'success' }]);
  assert.strictEqual(env.h.frames.a, undefined, 'its terminal frame is dropped');
  assert.deepStrictEqual(Object.keys(env.h.deleting), []);
  // Announced once.
  env.h.sessionsSettleDeletes([S('b', 'running')]);
  assert.strictEqual(env.toasts.length, 1);
});

test('a delete the hive could not finish brings the tab back and shows the reason', async () => {
  const env = load([S('a', 'ended')], () => Promise.resolve({ ok: true, deleting: true, session: S('a', 'deleting') }));
  await env.h.sessionsClose('a');
  const reason = "this session holds the hive's current sign-in; adopting it failed: disk full";
  env.h.sessionsSettleDeletes([S('a', 'ended', { delete_error: reason })]);
  assert.deepStrictEqual(env.toasts, [{ msg: 'Could not delete name-a: ' + reason + '.', type: 'error' }]);
  assert.deepStrictEqual(Object.keys(env.h.deleting), []);
  assert.strictEqual(env.h.sessionsDeleteErrorText({ delete_error: reason }), 'The last delete failed: ' + reason + '. Nothing was lost; you can retry.');
  assert.strictEqual(env.h.sessionsDeleteErrorText({}), '');
});

test('a refused delete (non-2xx) leaves the tab and toasts the reason', async () => {
  const env = load([S('a', 'ended')], () => Promise.reject(new Error('only the session\'s creator or an owner may change it')));
  env.h.select('a');
  await env.h.sessionsClose('a');
  assert.deepStrictEqual(ids(env.h), ['a'], 'the tab stays');
  assert.strictEqual(env.h.data().sessions[0].status, 'ended');
  assert.strictEqual(env.h.selected(), 'a');
  assert.deepStrictEqual(env.toasts, [{ msg: 'Could not delete name-a: only the session\'s creator or an owner may change it', type: 'error' }]);
  assert.deepStrictEqual(Object.keys(env.h.deleting), []);
});

test('200 removed=true drops the tab at once, without a reload', async () => {
  const env = load([S('a', 'ended'), S('b', 'ended')], () => Promise.resolve({ ok: true, removed: true }));
  env.h.select('a');
  await env.h.sessionsClose('a');
  assert.deepStrictEqual(ids(env.h), ['b']);
  assert.strictEqual(env.h.selected(), 'b', 'the selection moves to a session that exists');
  assert.ok(env.state.renders >= 1);
  assert.deepStrictEqual(env.toasts, [{ msg: 'Session name-a deleted.', type: 'success' }]);
});

test('closing a running session keeps its tab (the list shows it ended) and says closed', async () => {
  const env = load([S('a', 'running')], () => Promise.resolve({ ok: true }));
  await env.h.sessionsClose('a');
  assert.match(env.state.confirms[0].title, /Close this session\?/);
  assert.deepStrictEqual(ids(env.h), ['a'], 'the record still exists, so the tab is not dropped locally');
  assert.ok(env.state.loads >= 1, 'the list is reloaded to show the session as ended');
  assert.deepStrictEqual(env.toasts, [{ msg: 'Session closed.', type: 'success' }]);
});

test('a declined confirm, a session already deleting and Retry delete', async () => {
  const env = load([S('a', 'ended'), S('b', 'deleting')], () => Promise.resolve({ ok: true, deleting: true, session: S('a', 'deleting') }));
  env.state.confirmAnswer = false;
  await env.h.sessionsClose('a');
  assert.deepStrictEqual(env.calls, [], 'no request when the confirm is declined');
  await env.h.sessionsClose('b');
  assert.strictEqual(env.state.confirms.length, 1, 'a deleting session offers nothing to confirm');
  assert.deepStrictEqual(env.calls, []);
  // Retry delete: the reason is already on the pane, so no second confirm.
  await env.h.sessionsDeleteRetry('a');
  assert.strictEqual(env.state.confirms.length, 1);
  assert.deepStrictEqual(env.calls, [['/a', 'DELETE']]);
  assert.strictEqual(env.h.data().sessions[0].status, 'deleting');
});

test('the deleting tab and pane show a spinner and no controls; a failed delete shows the reason and Retry delete', () => {
  const tab = html.slice(html.indexOf('function sessionsTabHtml('), html.indexOf('function sessionsHeaderHtml('));
  assert.match(tab, /sessions-spinner[^<]*aria-label="Deleting"/, 'the deleting tab shows a spinner');
  const head = html.slice(html.indexOf('function sessionsHeaderHtml('), html.indexOf('function sessionsRefreshQueueUI('));
  assert.match(head, /canManage && s\.status !== SESSIONS_STATUS_DELETING/, 'no session menu while deleting');
  const pane = html.slice(html.indexOf('function sessionsPaneBodyHtml('), html.indexOf('function sessionsEnsurePane('));
  assert.match(pane, /mode === 'deleting'[^]*sessions-spinner[^]*SESSIONS_DELETING_TEXT/);
  assert.match(pane, /s\.delete_error[^]*role="alert"[^]*sessionsDeleteErrorText\(s\)[^]*data-action="sessionsDeleteRetry"/);
  assert.match(html, /sessionsSettleDeletes\(_sessionsData\.sessions\);/, 'every fresh list settles the deletes this browser started');
});
