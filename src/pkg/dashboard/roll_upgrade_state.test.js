// #15788: a pod roll (hub kubectl, operator `rollout restart`, bare SIGTERM)
// must drive the same upgrade banner the self-upgrade uses: SSE "upgrading"
// event → record → survives the reconnect → "Upgraded ✓" when the build hash
// changed, "Restarted ✓" when only the process did.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');

function slice(from, to) {
  const start = html.indexOf(from);
  assert.ok(start >= 0, 'missing ' + from);
  const end = html.indexOf(to, start);
  assert.ok(end > start, 'missing ' + to);
  return html.slice(start, end);
}

const stateSrc = slice('function versionNowMs()', 'function versionDeliveryLabel(');
const helpersSrc = slice('function versionShortSHA(sha)', 'function versionDashHTML(');
const renderSrc = slice('function versionUpgradeProgressStatus(v, st)', 'function versionSetHTMLPreservingUpgradeBee(');

function build() {
  const store = new Map();
  const toasts = [];
  const prelude = `
    const VERSION_UPGRADE_STORAGE_KEY = 'hive.version.upgradeProgress';
    const VERSION_UPGRADE_HUB_SEEN_KEY = 'hive.version.upgradeHubAttempt';
    const VERSION_UPGRADE_DONE_MS = 30000;
    const VERSION_UPGRADE_LONG_MS = 15 * 60 * 1000;
    const VERSION_UPGRADE_POLL_MS = 5000;
    const VERSION_ROLL_POLL_MS = 2000;
    const VERSION_ROLL_TOAST_MS = 15000;
    var _upgradeInProgress = false, _upgradeTargetHash = null, _versionUpgradePollTimer = null;
    function escapeHtml(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'); }
    function versionPolicy(v) { return v && v.upgradePolicy || null; }
    function fetchGitVersion() {}
    function setTimeout() { return 1; }
    function clearTimeout() {}
    function showToast(msg, type, persistent, ms) { toasts.push({ msg, type, ms }); }
  `;
  const h = new Function('localStorage', 'toasts', prelude + helpersSrc + stateSrc + renderSrc + `
    return {
      read: versionReadUpgradeProgress, clear: versionClearUpgradeProgress,
      fromEvent: versionRollRecordFromEvent, reconcile: versionReconcileUpgradeProgress,
      adoptServer: versionAdoptServerRollProgress, outcome: versionRollOutcome,
      title: versionRollTitle, status: versionRollStatusText, navbar: versionRollNavbarStatus,
      navbarHTML: versionNavbarUpgradeHTML, beeHTML: versionBeeProgressHTML,
      reloadDeferred: versionRollReloadDeferred, demoRequested: versionDemoRequested,
      pollDelayIsFast: function(st) { versionScheduleUpgradePoll(st); return true; },
      inProgress: function() { return _upgradeInProgress; }
    };`)({
    getItem: (k) => (store.has(k) ? store.get(k) : null),
    setItem: (k, v) => store.set(k, String(v)),
    removeItem: (k) => store.delete(k),
  }, toasts);
  h.toasts = toasts;
  h.store = store;
  return h;
}

const OLD = { hash: 'aaaaaaa1111111111111111111111111111111111', short: 'aaaaaaa', startedAt: '2026-10-20T10:00:00Z' };
const NEW_BUILD = { hash: 'bbbbbbb2222222222222222222222222222222222', short: 'bbbbbbb', startedAt: '2026-10-20T10:05:00Z' };
const SAME_BUILD_NEW_PROCESS = { hash: OLD.hash, short: OLD.short, startedAt: '2026-10-20T10:05:00Z' };
const T0 = Date.parse('2026-10-20T10:04:00Z');

test('announce event opens a roll record rendered as an in-flight upgrade', () => {
  const h = build();
  const st = h.fromEvent({ phase: 'announced', source: 'hub', target: NEW_BUILD.hash, reason: 'hub-managed upgrade', startedAt: '2026-10-20T10:03:50Z' }, OLD, T0);
  assert.strictEqual(st.source, 'roll');
  assert.strictEqual(st.phase, 'announced');
  assert.strictEqual(st.targetShort, 'bbbbbbb');
  assert.strictEqual(st.startedFrom, OLD.hash);
  assert.strictEqual(st.fromStartedAt, OLD.startedAt);
  assert.strictEqual(st.startedAt, Date.parse('2026-10-20T10:03:50Z'));
  assert.ok(h.inProgress());
  assert.strictEqual(h.title(st), 'Upgrading hive…');
  assert.match(h.status(st, OLD, T0), /^roll announced by the hub → bbbbbbb — hub-managed upgrade · 10s elapsed$/);
  assert.match(h.navbarHTML(st, OLD), /oc-version-hive/);
  assert.match(h.navbarHTML(st, OLD), /Upgrading hive…/);
  assert.match(h.beeHTML(st, OLD), /oc-version-upgrade-progress/);
});

test('SIGTERM with no prior announce still shows restarting, then Restarted on the same build', () => {
  const h = build();
  const st = h.fromEvent({ phase: 'restarting', source: 'signal', target: '', processStartedAt: OLD.startedAt }, OLD, T0);
  assert.strictEqual(st.target, '');
  assert.strictEqual(h.title(st), 'Restarting hive…');
  assert.strictEqual(h.status(st, {}, T0 + 3000), 'restarting · reconnecting · 3s elapsed');
  assert.strictEqual(h.navbar(st, {}), 'reconnecting');
  // The record survives a reload: read it back from storage.
  assert.strictEqual(h.read(T0 + 5000).phase, 'restarting');
  // While the pod is down /api/version fails; nothing to reconcile, record stays.
  assert.strictEqual(h.outcome(h.read(T0 + 5000), null), 'pending');
  // Old pod still answering (same process) → still pending.
  assert.strictEqual(h.outcome(h.read(T0 + 5000), OLD), 'pending');
  // New pod, same build → Restarted.
  const done = h.reconcile(SAME_BUILD_NEW_PROCESS, T0 + 20000);
  assert.strictEqual(done.outcome, 'restarted');
  assert.ok(done.completedAt);
  assert.strictEqual(h.title(done), 'Restarted ✓');
  assert.match(h.status(done, SAME_BUILD_NEW_PROCESS, T0 + 20000), /^↻ Restarted · still on aaaaaaa/);
  assert.strictEqual(h.toasts.length, 1);
  assert.match(h.toasts[0].msg, /Hive restarted/);
  assert.strictEqual(h.toasts[0].ms, 15000);
  assert.ok(!h.inProgress());
  // Reconciling again does not toast twice.
  h.reconcile(SAME_BUILD_NEW_PROCESS, T0 + 21000);
  assert.strictEqual(h.toasts.length, 1);
});

test('announce → restarting → reconnect onto a new build reads Upgraded to <ver>', () => {
  const h = build();
  h.fromEvent({ phase: 'announced', source: 'hub', target: NEW_BUILD.hash, startedAt: '2026-10-20T10:04:00Z' }, OLD, T0);
  const restarting = h.fromEvent({ phase: 'restarting', source: 'hub', target: NEW_BUILD.hash, processStartedAt: OLD.startedAt, startedAt: '2026-10-20T10:04:00Z' }, OLD, T0 + 60000);
  assert.strictEqual(restarting.startedAt, T0, 'the restart keeps the announce clock');
  assert.strictEqual(restarting.phase, 'restarting');
  // Fresh pod still booting: /api/version carries a startup phase.
  const booting = Object.assign({}, NEW_BUILD, { startupPhase: 'bootStores', startupPhaseLabel: 'opening stores' });
  assert.strictEqual(h.navbar(restarting, booting), '→ bbbbbbb · starting: opening stores');
  const done = h.reconcile(NEW_BUILD, T0 + 90000);
  assert.strictEqual(done.outcome, 'upgraded');
  assert.strictEqual(done.targetShort, 'bbbbbbb');
  assert.strictEqual(h.title(done), 'Upgraded ✓');
  assert.match(h.status(done, NEW_BUILD, T0 + 90000), /^✅ Upgraded to bbbbbbb/);
  assert.deepStrictEqual(h.toasts.map((t) => t.msg), ['Upgraded to bbbbbbb ✓']);
  // The silent hash-change reload waits for the toast countdown.
  assert.strictEqual(h.reloadDeferred(done, T0 + 90000 + 1000), true);
  assert.strictEqual(h.reloadDeferred(done, T0 + 90000 + 15000), false);
  // The done record expires like a self-upgrade's.
  assert.strictEqual(h.read(T0 + 90000 + 31000), null);
});

test('an announce the server no longer reports (aborted roll) is dropped; restarting is not', () => {
  const h = build();
  h.fromEvent({ phase: 'announced', source: 'operator', target: NEW_BUILD.hash }, OLD, T0);
  assert.strictEqual(h.reconcile(Object.assign({}, OLD, { upgradeProgress: { phase: 'announced', target: 'bbbbbbb' } }), T0 + 5000).phase, 'announced');
  assert.strictEqual(h.reconcile(OLD, T0 + 10000), null, 'server dropped the announce → record cleared');
  assert.strictEqual(h.read(T0 + 10000), null);

  h.fromEvent({ phase: 'restarting', source: 'signal', processStartedAt: OLD.startedAt }, OLD, T0);
  assert.strictEqual(h.reconcile(OLD, T0 + 10000).phase, 'restarting', 'old pod still up: keep waiting for the kill');
});

test('a tab opened after the announce adopts the server-side roll progress', () => {
  const h = build();
  const v = Object.assign({}, OLD, { upgradeProgress: { phase: 'announced', state: 'pulling', source: 'hub', target: 'bbbbbbb', from: 'aaaaaaa', startedAt: '2026-10-20T10:03:00Z' } });
  const st = h.adoptServer(v, T0);
  assert.strictEqual(st.source, 'roll');
  assert.strictEqual(st.rollSource, 'hub');
  assert.strictEqual(st.targetShort, 'bbbbbbb');
  assert.strictEqual(st.fromStartedAt, OLD.startedAt);
  assert.strictEqual(h.adoptServer(OLD, T0), null);
  // The server's phase advancing to restarting updates the record.
  const adv = h.reconcile(Object.assign({}, OLD, { upgradeProgress: { phase: 'restarting', target: 'bbbbbbb' } }), T0 + 1000);
  assert.strictEqual(adv.phase, 'restarting');
});

test('a roll record never reports a bare "target" placeholder and the demo query is recognised', () => {
  const h = build();
  assert.strictEqual(h.demoRequested('?demo=upgrade'), true);
  assert.strictEqual(h.demoRequested('?x=1&demo=upgrade&y=2'), true);
  assert.strictEqual(h.demoRequested('?demo=upgraded'), false);
  assert.strictEqual(h.demoRequested(''), false);
  const st = h.fromEvent({ phase: 'restarting', source: 'signal' }, {}, T0);
  assert.doesNotMatch(h.status(st, {}, T0), /target/);
  assert.strictEqual(h.fromEvent({ phase: 'done' }, OLD, T0), null);
});

test('the SIGTERM a self-upgrade causes does not replace the self-upgrade record', () => {
  const h = build();
  h.store.set('hive.version.upgradeProgress', JSON.stringify({ target: NEW_BUILD.hash, targetShort: 'bbbbbbb', startedFrom: OLD.hash, startedAt: T0 - 5000, completedAt: 0 }));
  const kept = h.fromEvent({ phase: 'restarting', source: 'signal', target: '' }, OLD, T0);
  assert.strictEqual(kept.source, undefined);
  assert.strictEqual(kept.targetShort, 'bbbbbbb');
  assert.strictEqual(h.read(T0).source, undefined, 'self-upgrade record untouched');
  // Whereas a roll record is superseded by the next roll event.
  h.clear();
  h.fromEvent({ phase: 'announced', source: 'hub', target: NEW_BUILD.hash }, OLD, T0);
  const killed = h.fromEvent({ phase: 'restarting', source: 'signal', target: '' }, OLD, T0 + 1);
  assert.strictEqual(killed.phase, 'restarting');
  assert.strictEqual(killed.targetShort, 'bbbbbbb', 'a bare SIGTERM after an announce keeps the announced target');
  assert.strictEqual(killed.startedAt, T0);
});
