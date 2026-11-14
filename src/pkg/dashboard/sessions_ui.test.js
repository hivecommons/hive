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
  'sessionsFormatBytes', 'sessionsIsEnabled'];
const h = new Function('window', html.slice(start, end) + '\nreturn {' + names.join(',') + '};')({});

const S = (id, group, status) => ({ id, name: id, group, status: status || 'running' });

test('sessions are grouped alphabetically with the ungrouped bucket last', () => {
  const groups = h.sessionsGroupSessions([S('a', ''), S('b', 'zeta'), S('c', 'alpha'), S('d', 'alpha'), null, {}]);
  assert.deepStrictEqual(groups.map(g => g.label), ['alpha', 'zeta', 'Ungrouped']);
  assert.deepStrictEqual(groups[0].sessions.map(s => s.id), ['c', 'd']);
  assert.deepStrictEqual(h.sessionsExistingGroups([S('a', ''), S('b', 'x'), S('c', 'x')]), ['x']);
  assert.deepStrictEqual(h.sessionsGroupSessions(undefined), []);
});

test('selected tab falls back to the first session, then to Campaigns', () => {
  const list = [S('a', ''), S('b', '')];
  assert.strictEqual(h.sessionsPickSelected(list, 'b'), 'b');
  assert.strictEqual(h.sessionsPickSelected(list, 'gone'), 'a');
  assert.strictEqual(h.sessionsPickSelected(list, 'campaigns'), 'campaigns');
  assert.strictEqual(h.sessionsPickSelected([], 'gone'), 'campaigns');
  assert.strictEqual(h.sessionsPickSelected(null, ''), 'campaigns');
});

test('arrow keys walk the visible tabs and wrap; collapsed groups keep only the selected tab', () => {
  const list = [S('a', 'g1'), S('b', 'g1'), S('c', 'g2')];
  const order = h.sessionsTabOrder(list, { g1: true }, 'b');
  assert.deepStrictEqual(order, ['b', 'c', 'campaigns']);
  assert.strictEqual(h.sessionsNextTab(order, 'campaigns', 'ArrowRight'), 'b');
  assert.strictEqual(h.sessionsNextTab(order, 'b', 'ArrowLeft'), 'campaigns');
  assert.strictEqual(h.sessionsNextTab(order, 'c', 'Home'), 'b');
  assert.strictEqual(h.sessionsNextTab(order, 'b', 'End'), 'campaigns');
  assert.strictEqual(h.sessionsNextTab(order, 'b', 'x'), '');
  assert.strictEqual(h.sessionsNextTab([], 'b', 'ArrowRight'), '');
});

test('only owner and read-write get a terminal; read sees scrollback; ended sessions never attach', () => {
  const run = S('a', '');
  const ended = S('a', '', 'ended');
  for (const role of ['owner', 'read-write']) assert.strictEqual(h.sessionsViewMode(role, run), 'terminal');
  for (const role of ['read', 'merger', '', undefined]) assert.strictEqual(h.sessionsViewMode(role, run), 'scrollback');
  assert.strictEqual(h.sessionsViewMode('owner', ended), 'ended');
  assert.strictEqual(h.sessionsViewMode('owner', null), 'none');
  assert.strictEqual(h.sessionsRoleCanWrite('read'), false);
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

test('layout gate shows Sessions and hides Inception only when sessions are enabled', () => {
  const m = /function sessionsFeatureEnabled\(\)\{[^\n]*\n\s*function dashboardFeatureSectionHidden\(id\)\{[^\n]*\}/.exec(html);
  assert.ok(m, 'layout gate not found');
  const gate = enabled => new Function('window', 'dashboardAliasSectionId', 'experimentLabEnabled',
    m[0] + '\nreturn dashboardFeatureSectionHidden;')({ _sessionsEnabled: enabled }, id => id, () => true);
  const off = gate(false);
  assert.strictEqual(off('sessions-section'), true);
  assert.strictEqual(off('inception-section'), false);
  const on = gate(true);
  assert.strictEqual(on('sessions-section'), false);
  assert.strictEqual(on('inception-section'), true);
  assert.strictEqual(on('nous-section'), false);
});
