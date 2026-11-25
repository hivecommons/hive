// Sessions section (tabbed shared CLI sessions): tab grouping/selection state,
// role gating, input-bar limit checks and the layout feature gate that swaps
// Sessions in for Inception. Helpers are cut out of the generated dashboard so
// the tests run the shipped code. Run: node --test src/pkg/dashboard/*.test.js
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
const start = html.indexOf('const SESSIONS_API');
const end = html.indexOf('// ── Storage conveniences');
assert.ok(start >= 0 && end > start, 'sessions helper block not found in index.html');
const names = ['sessionsRoleCanWrite', 'sessionsGroupSessions', 'sessionsExistingGroups', 'sessionsPickSelected', 'sessionsTabOrder',
  'sessionsNextTab', 'sessionsViewMode', 'sessionsCheckInput', 'sessionsCheckUpload', 'sessionsCanCreate', 'sessionsTerminalPath',
  'sessionsFormatBytes', 'sessionsIsEnabled', 'sessionsWorkspaceLabel', 'sessionsSessionWorkspace', 'sessionsIsLive', 'sessionsHasPreparing',
  'sessionsStatusText', 'sessionsCanSend', 'sessionsKeyboardAction', 'sessionsPresenceText', 'sessionsQueueLabel', 'sessionsQueuedToast',
  'sessionsStartupBadge', 'sessionsHasStartupPreparing'];
const h = new Function('window', html.slice(start, end) + '\nreturn {' + names.join(',') + '};')({});

const S = (id, group, status) => ({ id, name: id, group, status: status || 'running' });

test('sessions are grouped alphabetically with the ungrouped bucket last', () => {
  const groups = h.sessionsGroupSessions([S('a', ''), S('b', 'zeta'), S('c', 'alpha'), S('d', 'alpha'), null, {}]);
  assert.deepStrictEqual(groups.map(g => g.label), ['alpha', 'zeta', 'Ungrouped']);
  assert.deepStrictEqual(groups[0].sessions.map(s => s.id), ['c', 'd']);
  assert.deepStrictEqual(h.sessionsExistingGroups([S('a', ''), S('b', 'x'), S('c', 'x')]), ['x']);
  assert.deepStrictEqual(h.sessionsGroupSessions(undefined), []);
});

test('selected tab falls back to the first session, then to none', () => {
  const list = [S('a', ''), S('b', '')];
  assert.strictEqual(h.sessionsPickSelected(list, 'b'), 'b');
  assert.strictEqual(h.sessionsPickSelected(list, 'gone'), 'a');
  // The retired Campaigns tab id, still remembered in some browsers, falls through.
  assert.strictEqual(h.sessionsPickSelected(list, 'campaigns'), 'a');
  assert.strictEqual(h.sessionsPickSelected([], 'gone'), '');
  assert.strictEqual(h.sessionsPickSelected(null, ''), '');
});

test('arrow keys walk the visible tabs and wrap; collapsed groups keep only the selected tab', () => {
  const list = [S('a', 'g1'), S('b', 'g1'), S('c', 'g2')];
  const order = h.sessionsTabOrder(list, { g1: true }, 'b');
  assert.deepStrictEqual(order, ['b', 'c']);
  assert.strictEqual(h.sessionsNextTab(order, 'c', 'ArrowRight'), 'b');
  assert.strictEqual(h.sessionsNextTab(order, 'b', 'ArrowLeft'), 'c');
  assert.strictEqual(h.sessionsNextTab(order, 'c', 'Home'), 'b');
  assert.strictEqual(h.sessionsNextTab(order, 'b', 'End'), 'c');
  assert.strictEqual(h.sessionsNextTab(order, 'b', 'x'), '');
  assert.strictEqual(h.sessionsNextTab([], 'b', 'ArrowRight'), '');
});

test('every role gets the live terminal (read-only unless it holds the keyboard); ended sessions never attach', () => {
  const run = S('a', '');
  const ended = S('a', '', 'ended');
  for (const role of ['owner', 'read-write', 'read', 'merger', '', undefined]) assert.strictEqual(h.sessionsViewMode(role, run), 'terminal');
  assert.strictEqual(h.sessionsViewMode('owner', ended), 'ended');
  assert.strictEqual(h.sessionsViewMode('owner', null), 'none');
  assert.strictEqual(h.sessionsRoleCanWrite('read'), false);
  // Send and the keyboard stay with the write roles.
  for (const role of ['owner', 'read-write']) assert.strictEqual(h.sessionsCanSend(role, run), true);
  for (const role of ['read', 'merger', '']) assert.strictEqual(h.sessionsCanSend(role, run), false);
  assert.strictEqual(h.sessionsCanSend('owner', ended), false);
});

test('keyboard button: release for the holder, take for another writer, nothing for read or non-running', () => {
  const mine = Object.assign(S('a', ''), { keyboard_mine: true, keyboard_holder: 'andy' });
  const theirs = Object.assign(S('a', ''), { keyboard_mine: false, keyboard_holder: 'bob' });
  assert.strictEqual(h.sessionsKeyboardAction('read-write', mine), 'release');
  assert.strictEqual(h.sessionsKeyboardAction('owner', theirs), 'take');
  assert.strictEqual(h.sessionsKeyboardAction('read-write', Object.assign({}, theirs, { keyboard_holder: '' })), 'take', 'a free keyboard is taken, not released');
  assert.strictEqual(h.sessionsKeyboardAction('read', theirs), '');
  assert.strictEqual(h.sessionsKeyboardAction('owner', Object.assign({}, mine, { status: 'ended' })), '');
  const js = html.slice(html.indexOf('function sessionsKeyboardHtml('), html.indexOf('function sessionsHeaderHtml('));
  assert.match(js, /data-action="sessionsKeyboard"[^>]*SESSIONS_KEYBOARD_RELEASE[^>]*>Release keyboard</);
  assert.match(js, /data-action="sessionsKeyboard"[^>]*SESSIONS_KEYBOARD_TAKE[^>]*>Take keyboard</);
  assert.match(js, /class="sessions-watching"[^>]*>watching · you have the keyboard\? no</, 'a read-only viewer sees the watching pill');
});

test('presence line names the keyboard holder and the watchers', () => {
  const s = { id: 'a', status: 'running', keyboard_holder: 'andy', viewers: [{ user: 'bob', keyboard: false }, { user: 'andy', keyboard: true }, { user: 'carol', keyboard: false }, null] };
  assert.strictEqual(h.sessionsPresenceText(s), 'keyboard: andy · watching: bob, carol');
  assert.strictEqual(h.sessionsPresenceText({ keyboard_holder: 'andy', viewers: [{ user: 'bob' }] }), 'keyboard: andy (away) · watching: bob', 'a holder who is not attached is marked away');
  assert.strictEqual(h.sessionsPresenceText({ keyboard_holder: '', viewers: [] }), 'keyboard: free');
  assert.strictEqual(h.sessionsPresenceText(null), 'keyboard: free');
  const head = html.slice(html.indexOf('function sessionsHeaderHtml('), html.indexOf('function sessionsQueueHtml('));
  assert.match(head, /class="sessions-presence' \+ \(s\.keyboard_mine \? ' mine' : ''\)/, 'the holder\'s own presence line is accented');
  assert.match(head, /sessionsKeyboardHtml\(s\)/);
  const tab = html.slice(html.indexOf('function sessionsTabHtml('), html.indexOf('function sessionsTabsHtml('));
  assert.match(tab, /sessionsPresenceText\(s\)/, 'the tab tooltip carries presence');
});

test('Send shows the queue depth, the toast says queued or sent, and the queue list offers Cancel on own items', () => {
  assert.strictEqual(h.sessionsQueueLabel('Send', { length: 0 }), 'Send');
  assert.strictEqual(h.sessionsQueueLabel('Send and run', { length: 2 }), 'Send and run · queued behind 2');
  assert.strictEqual(h.sessionsQueueLabel('Send', undefined), 'Send');
  assert.match(h.sessionsQueuedToast('fix', { position: 1 }), /^Sent to fix/);
  assert.match(h.sessionsQueuedToast('fix', { position: 3 }), /position 3/);
  const js = html.slice(html.indexOf('function sessionsQueueHtml('), html.indexOf('function sessionsInputBarHtml('));
  assert.match(js, /data-action="sessionsCancelQueued"/);
  assert.match(js, /it\.mine && !it\.delivering \?/, 'only own, not-yet-delivering items can be cancelled');
  assert.doesNotMatch(js, /it\.text/, 'the queue never renders text (the server does not send it)');
  const bar = html.slice(html.indexOf('function sessionsInputBarHtml('), html.indexOf('function sessionsRefreshQueueUI('));
  assert.match(bar, /sessionsQueueLabel\('Send', s\.queue\)/);
  assert.match(bar, /sessionsQueueHtml\(s\)/);
  const css = html.slice(html.indexOf('.sessions-presence {'), html.indexOf('.sessions-queue-item .hv-btn'));
  assert.doesNotMatch(css, /#[0-9a-fA-F]{3,8}\b/, 'presence and queue CSS must not add hex literals');
});

test('a successful send clears its draft only if it has not been edited', async () => {
  const js = html.slice(html.indexOf('// ── Input bar ──'), html.indexOf('async function sessionsCancelQueued('));
  const counter = { textContent: '', classList: { toggle() {} } };
  const bar = { querySelector: () => counter };
  const box = { value: 'first prompt', closest: () => bar };
  const requests = [];
  let finishSend;
  const { sessionsSend, sessionsInputChanged } = new Function(
    'sessionsSelectedSession', 'document', 'sessionsCheckInput', '_sessionsData', 'sessionsApi',
    'sessionsByteLength', 'sessionsFormatBytes', 'showToast', 'sessionsQueuedToast',
    js + '\nreturn { sessionsSend, sessionsInputChanged };')(
    () => S('a', ''), { getElementById: () => box }, h.sessionsCheckInput,
    { limits: { max_input_bytes: 100 }, sessions: [] },
    (route, method, body) => {
      requests.push({ route, method, ...body });
      return new Promise(resolve => { finishSend = resolve; });
    }, value => Buffer.byteLength(value), h.sessionsFormatBytes, () => {}, h.sessionsQueuedToast);
  const edit = value => {
    box.value = value;
    sessionsInputChanged(box);
  };

  const first = sessionsSend(true);
  edit('follow-up draft');
  finishSend({ queued: { position: 1 } });
  await first;
  assert.strictEqual(box.value, 'follow-up draft');

  const second = sessionsSend(false);
  edit('temporary edit');
  edit('follow-up draft');
  finishSend({ queued: { position: 2 } });
  await second;
  assert.strictEqual(box.value, 'follow-up draft');
  assert.strictEqual(counter.textContent, '15 bytes / 100 bytes');

  const third = sessionsSend(false);
  finishSend({ queued: { position: 3 } });
  await third;
  assert.strictEqual(box.value, '');
  assert.strictEqual(counter.textContent, '0 bytes / 100 bytes');
  assert.deepStrictEqual(requests, [
    { route: '/a/input', method: 'POST', text: 'first prompt', submit: true },
    { route: '/a/input', method: 'POST', text: 'follow-up draft', submit: false },
    { route: '/a/input', method: 'POST', text: 'follow-up draft', submit: false },
  ]);
});

test('a keyboard hand-over re-attaches the iframe with a fresh handoff', () => {
  const js = html.slice(html.indexOf('function sessionsSyncKeyboardFrame('), html.indexOf('async function sessionsHandoff('));
  assert.match(js, /_sessionsFrameKeyboard\[s\.id\] === mine\) return/);
  assert.match(js, /sessionsAttachTerminal\(s\)/);
});

test('input bar rejects empty and oversize text using the server limits', () => {
  const limits = { max_input_bytes: 10, max_upload_bytes: 100 };
  assert.strictEqual(h.sessionsCheckInput('   ', limits).ok, false);
  assert.strictEqual(h.sessionsCheckInput('0123456789', limits).ok, true);
  const over = h.sessionsCheckInput('01234567890', limits);
  assert.strictEqual(over.ok, false);
  assert.match(over.error, /limited to 10 bytes/);
  assert.strictEqual(h.sessionsCheckInput('é'.repeat(6), limits).ok, false, 'limit counts bytes, not characters');
  assert.strictEqual(h.sessionsCheckInput('x'.repeat(1000), {}).ok, true, 'no limit advertised means no client cap');
});

test('uploads are checked against max_upload_bytes before any request', () => {
  const limits = { max_upload_bytes: 2048 };
  assert.strictEqual(h.sessionsCheckUpload(null, limits).ok, false);
  assert.strictEqual(h.sessionsCheckUpload({ name: 'a', size: 0 }, limits).ok, false);
  assert.strictEqual(h.sessionsCheckUpload({ name: 'a', size: 2048 }, limits).ok, true);
  const big = h.sessionsCheckUpload({ name: 'big.bin', size: 3 * 1024 * 1024 }, limits);
  assert.strictEqual(big.ok, false);
  assert.match(big.error, /limited to 2 KiB/);
});

test('New session is gated by role, vendor availability and the running-session cap', () => {
  const data = { vendors: [{ id: 'claude', available: true }, { id: 'codex', available: false }], sessions: [S('a', '')], limits: { max_sessions: 2 } };
  assert.strictEqual(h.sessionsCanCreate('owner', data).ok, true);
  assert.strictEqual(h.sessionsCanCreate('read', data).ok, false);
  assert.strictEqual(h.sessionsCanCreate('owner', { vendors: [{ id: 'x', available: false }], sessions: [], limits: {} }).ok, false);
  const full = Object.assign({}, data, { sessions: [S('a', ''), S('b', ''), S('c', '', 'ended')] });
  assert.strictEqual(h.sessionsCanCreate('read-write', full).ok, false);
  assert.match(h.sessionsCanCreate('read-write', full).reason, /limit of 2/);
});

test('terminal iframe path attaches hive-sess-<id> with the handoff code', () => {
  assert.strictEqual(h.sessionsTerminalPath('abc', 'c0de'), '/terminal/?arg=hive-sess-abc&code=c0de');
  assert.strictEqual(h.sessionsTerminalPath('abc', ''), '/terminal/?arg=hive-sess-abc');
});

test('terminal pane and iframe take the dashboard terminal surface token, never a literal colour', () => {
  const css = html.slice(html.indexOf('.sessions-terminal {'), html.indexOf('.sessions-log {'));
  assert.match(css, /\.sessions-terminal \{[^}]*background: var\(--terminal-bg\)/);
  assert.match(css, /\.sessions-frame \{[^}]*background: var\(--terminal-bg\)/);
  assert.doesNotMatch(css, /#[0-9a-fA-F]{3,8}\b/, 'sessions pane CSS must not add hex literals');
});

test('Settings → Dashboard stages dashboard.sessions.* as one nested block', () => {
  const src = html.slice(html.indexOf('function markDashboardSessions'), html.indexOf('function markBackendsPolicy'));
  const dirty = {};
  const state = { data: { dashboard: { sessions: { enabled: false, max_sessions: 6 } } } };
  const mark = new Function('_configState', 'markDirty', src + '\nreturn markDashboardSessions;')(state, (section, key, value) => { dirty[section + '.' + key] = value; });
  mark('enabled', true);
  mark('max_per_user', '3');
  mark('idle_ttl_hours', '-4');
  assert.deepStrictEqual(dirty['dashboard.sessions'], { enabled: true, max_sessions: 6, max_per_user: 3, idle_ttl_hours: 0 });
  assert.match(html, /data-section="dashboard-sessions" data-key="enabled"/, 'Sessions toggle is rendered in the Dashboard tab');
  assert.match(html, /yamlHint\('dashboard\.sessions\.enabled'\)/);
  for (const key of ['max_sessions', 'max_per_user', 'idle_ttl_hours']) {
    assert.match(html, new RegExp("limit\\('" + key + "', "), 'limit field for dashboard.sessions.' + key);
  }
  assert.match(html, /HIVE_SESSIONS_ENABLED environment variable, when set, overrides this switch/);
});

test('layout gate shows Inception (spek) only when sessions are enabled; the wizard section is gone', () => {
  const m = /function sessionsFeatureEnabled\(\)\{[^\n]*\n\s*function dashboardFeatureSectionHidden\(id\)\{[^\n]*\}/.exec(html);
  assert.ok(m, 'layout gate not found');
  const gate = enabled => new Function('window', 'dashboardAliasSectionId', 'experimentLabEnabled',
    m[0] + '\nreturn dashboardFeatureSectionHidden;')({ _sessionsEnabled: enabled }, id => id, () => true);
  const off = gate(false);
  assert.strictEqual(off('sessions-section'), true);
  const on = gate(true);
  assert.strictEqual(on('sessions-section'), false);
  assert.strictEqual(on('nous-section'), false);
  assert.doesNotMatch(html, /id="inception-section"/, 'the Inception wizard section must be removed');
  assert.doesNotMatch(html, /data-section="inception-section"/, 'the Inception wizard sidebar item must be removed');
});

test('#inception-section deep links and saved layouts alias to the Inception (spek) section', () => {
  const m = /function dashboardAliasSectionId\(id\)\{[^\n]*?return id;\}/.exec(html);
  assert.ok(m, 'dashboardAliasSectionId not found');
  const alias = new Function(m[0] + '\nreturn dashboardAliasSectionId;')();
  assert.strictEqual(alias('inception-section'), 'sessions-section');
  assert.strictEqual(alias('review-pipeline-section'), 'review-queue-section');
  assert.strictEqual(alias('knowledge-section'), 'knowledge-section');
});

test('workspace label names the shared checkout or says sessions start empty', () => {
  assert.match(h.sessionsWorkspaceLabel({ repo: 'acme/widgets', path: '/data/session-sandbox/workspace/acme/widgets' }), /acme\/widgets \(\/data\/session-sandbox\/workspace\/acme\/widgets\)/);
  assert.match(h.sessionsWorkspaceLabel({ repo: 'acme/widgets' }), /Workspace: acme\/widgets, shared/);
  assert.match(h.sessionsWorkspaceLabel({}), /empty directory/);
  assert.match(h.sessionsWorkspaceLabel(undefined), /empty directory/);
  assert.deepStrictEqual(h.sessionsSessionWorkspace({ id: 'a', workspace_repo: 'acme/widgets', workspace: '/w' }), { repo: 'acme/widgets', path: '/w' });
  assert.deepStrictEqual(h.sessionsSessionWorkspace({ id: 'a' }), { repo: '', path: '' });
  assert.deepStrictEqual(h.sessionsSessionWorkspace(null), {});
});

test('a preparing session is its own view mode, counts as live and carries the server message with the clone hint', () => {
  const prep = { id: 'p', name: 'p', status: 'preparing', message: 'preparing workspace: cloning acme/widgets (first session on this hive)' };
  const failed = { id: 'f', name: 'f', status: 'failed', error: 'preparing session workspace: git clone acme/widgets: exit 128' };
  for (const role of ['owner', 'read-write', 'read']) {
    assert.strictEqual(h.sessionsViewMode(role, prep), 'preparing', 'no terminal and no scrollback while preparing');
    assert.strictEqual(h.sessionsViewMode(role, failed), 'failed');
  }
  assert.strictEqual(h.sessionsIsLive(prep), true);
  assert.strictEqual(h.sessionsIsLive(S('a', '')), true);
  assert.strictEqual(h.sessionsIsLive(failed), false);
  assert.strictEqual(h.sessionsIsLive(S('a', '', 'ended')), false);
  assert.strictEqual(h.sessionsHasPreparing([S('a', ''), prep]), true);
  assert.strictEqual(h.sessionsHasPreparing([S('a', '')]), false);
  assert.strictEqual(h.sessionsHasPreparing(undefined), false);
  const text = h.sessionsStatusText(prep);
  assert.match(text, /^Preparing workspace: cloning acme\/widgets \(first session on this hive\)… /);
  assert.match(text, /couple of minutes/);
  assert.match(h.sessionsStatusText({ status: 'preparing' }), /^Preparing the session… /, 'a missing message still reads as progress');
  assert.strictEqual(h.sessionsStatusText(failed), 'Could not start this session: preparing session workspace: git clone acme/widgets: exit 128.');
  assert.match(h.sessionsStatusText({ status: 'failed' }), /unknown error/);
  assert.match(h.sessionsStatusText({ status: 'ended', error: 'spoke is out of uids' }), /last resume failed: spoke is out of uids/);
  assert.strictEqual(h.sessionsStatusText(S('a', '')), '');
  assert.strictEqual(h.sessionsStatusText(null), '');
});

test('preparing sessions count against the running-session cap', () => {
  const data = { vendors: [{ id: 'claude', available: true }], sessions: [S('a', ''), S('b', '', 'preparing')], limits: { max_sessions: 2 } };
  assert.strictEqual(h.sessionsCanCreate('owner', data).ok, false, 'two quick clicks must not overshoot the cap');
  assert.strictEqual(h.sessionsCanCreate('owner', Object.assign({}, data, { sessions: [S('a', ''), S('b', '', 'failed')] })).ok, true);
});

test('preparing and failed tabs and panes are rendered with a spinner, the message, and Retry/Delete', () => {
  const js = html.slice(html.indexOf('function sessionsTabHtml('), html.indexOf('function sessionsEnsurePane('));
  assert.match(js, /sessions-spinner[^<]*aria-label="Preparing"/, 'the preparing tab shows a spinner in place of the status dot');
  assert.match(js, /class="sessions-status sessions-status-preparing" role="status" aria-live="polite"/);
  assert.match(js, /sessionsStatusText\(s\)/);
  assert.match(js, /role="alert"[^]*data-action="sessionsRetry"[^]*data-action="sessionsClose"/, 'a failed pane offers Retry and Delete');
  assert.match(js, /SESSIONS_STATUS_PREPARING\) closeItem = ''/, 'a preparing session cannot be closed from the menu');
  const css = html.slice(html.indexOf('.sessions-spinner {'), html.indexOf('.sessions-status-failed {'));
  assert.match(css, /animation: spin/);
  assert.doesNotMatch(css, /#[0-9a-fA-F]{3,8}\b/, 'status CSS must not add hex literals');
  // The empty-state text is hidden as soon as a session (preparing included) exists.
  const render = html.slice(html.indexOf('function renderSessions('), html.indexOf('function sessionsSyncPreparingPoll('));
  assert.match(render, /empty\.hidden = sessions\.length > 0/);
  assert.match(render, /sessionsSyncPreparingPoll\(sessions\)/, 'polling speeds up while a session is preparing');
});

test('an ended pane offers Resume only when resumable, and Start again either way', () => {
  const js = html.slice(html.indexOf('function sessionsPaneBodyHtml('), html.indexOf('function sessionsEnsurePane('));
  assert.match(js, /if \(s\.resumable\)[^]*data-action="sessionsResume"[^]*\} else \{[^]*cannot be resumed[^]*\}/, 'Resume sits only in the resumable branch');
  assert.match(js, /data-action="sessionsRestart"[^]*>Start again</);
  assert.match(js, /restartBtn\('btn-secondary'\)/, 'resumable panes get a secondary Start again');
  assert.match(js, /restartBtn\('btn-primary'\)/, 'non-resumable panes get a primary Start again');
  assert.match(html, /async function sessionsRestart\(id\)[^]*sessionsSubmitCreate\(body\)/);
});

test('startup badge: getting the CLI ready, started, and which screen needs a person', () => {
  const now = Date.parse('2026-11-17T12:00:00Z');
  const run = (startup) => ({ id: 'a', name: 'a', status: 'running', startup });
  const busy = h.sessionsStartupBadge(run({ state: 'preparing', message: 'Getting the CLI ready…' }), now);
  assert.strictEqual(busy.kind, 'busy');
  assert.strictEqual(busy.text, 'Getting the CLI ready…');
  assert.match(busy.title, /never answers a permission prompt and never signs in/);
  assert.strictEqual(h.sessionsStartupBadge(run({ state: 'preparing' }), now).text, 'Getting the CLI ready…', 'a missing message still reads as progress');

  const started = h.sessionsStartupBadge(run({ state: 'started', message: 'Started', since: '2026-11-17T11:59:30Z', answered: ['claude workspace trust'] }), now);
  assert.strictEqual(started.kind, 'ok');
  assert.strictEqual(started.text, 'Started');
  assert.match(started.title, /Hive answered: claude workspace trust/);
  assert.strictEqual(h.sessionsStartupBadge(run({ state: 'started', since: '2026-11-17T11:00:00Z' }), now), null, 'Started is shown only for a moment');
  assert.strictEqual(h.sessionsStartupBadge(run({ state: 'started' }), now), null);

  const signin = h.sessionsStartupBadge(run({ state: 'needs_signin', message: 'Needs sign-in. Sign in from the terminal; Hive never signs in for you.' }), now);
  assert.strictEqual(signin.kind, 'attention');
  assert.strictEqual(signin.text, 'Needs sign-in');
  assert.match(signin.title, /never signs in for you/);
  const blocked = h.sessionsStartupBadge(run({ state: 'blocked', screen: 'claude permission dialog', message: 'Waiting for you: claude permission dialog is on screen, and Hive does not answer it.' }), now);
  assert.strictEqual(blocked.text, 'Waiting for you: claude permission dialog', 'the badge names the screen');
  assert.match(blocked.title, /does not answer it/);
  assert.strictEqual(h.sessionsStartupBadge(run({ state: 'manual' }), now).text, 'Left to you');
  assert.strictEqual(h.sessionsStartupBadge(run({ state: 'unknown' }), now).text, 'Check the terminal');
  assert.strictEqual(h.sessionsStartupBadge(run({ state: 'something-new' }), now).kind, 'attention');

  assert.strictEqual(h.sessionsStartupBadge(run(undefined), now), null, 'no startup block, no badge');
  assert.strictEqual(h.sessionsStartupBadge({ id: 'a', status: 'ended', startup: { state: 'preparing' } }, now), null, 'only running sessions');
  assert.strictEqual(h.sessionsStartupBadge(null, now), null);

  assert.strictEqual(h.sessionsHasStartupPreparing([run({ state: 'started' }), run({ state: 'preparing' })]), true, 'polls faster while the CLI is being readied');
  assert.strictEqual(h.sessionsHasStartupPreparing([run({ state: 'needs_signin' }), run(undefined)]), false);
  assert.strictEqual(h.sessionsHasStartupPreparing(undefined), false);
  assert.match(html, /presence \+ sessionsStartupHtml\(s\)/, 'the pane header renders the badge');
  assert.match(html, /sessionsHasPreparing\(sessions\) \|\| sessionsHasStartupPreparing\(sessions\)/);
  assert.match(html, /data-section="dashboard-sessions" data-key="autostart"/, 'the startup switch is rendered in the Dashboard tab');
});
