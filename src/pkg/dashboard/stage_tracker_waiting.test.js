// Waiting-on-you surfacing (hivecommons/hive#15638): the tracker payload's
// waiting_on_human block feeds a second sidebar badge, a topbar pill and a
// debounced toast; the Waiting on you filter keeps only flagged rows. The
// functions are lifted out of index.html so this runs against the shipped
// source.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');

function pick(re, what) {
  const m = html.match(re);
  assert.ok(m, what + ' not found in index.html');
  return m[0];
}

function load(env) {
  const src = [
    'var toasts = [];',
    'var navigated = [];',
    'var filtered = [];',
    'function showToast(msg, type, opts) { toasts.push({ msg, type, opts }); }',
    'function ocNavigate(s) { navigated.push(s); }',
    'function hamFilter(k, v) { filtered.push([k, v]); }',
    'function stageTrackerActedAgo(ms) { return Math.round(ms / 3600000) + "h"; }',
    'function stageTrackerLoadFilter() {}',
    'function stageTrackerSaveFilter() {}',
    'var _stageTrackerFilter = {};',
    pick(/const STAGE_TRACKER_WAITING_TOAST_GAP_MS = [^\n]*\n/, 'GAP'),
    // Toast duration comes from the Activity Monitor settings (#15813).
    'function stageTrackerToastMs() { return 15000; }',
    pick(/const STAGE_TRACKER_WAITING_TOOLTIP_MAX = [^\n]*\n/, 'TOOLTIP_MAX'),
    pick(/const STAGE_TRACKER_WAITING_TOAST_NUMBERS = [^\n]*\n/, 'TOAST_NUMBERS'),
    pick(/const STAGE_TRACKER_WAITING_TOAST_KEY = [^\n]*\n/, 'TOAST_KEY'),
    pick(/const STAGE_TRACKER_WAITING_FILTER_PARAM = [^\n]*\n/, 'FILTER_PARAM'),
    pick(/const STAGE_TRACKER_WAIT_KIND_WORDS = \{[\s\S]*?\n    \};\n/, 'WAIT_KIND_WORDS'),
    'var _stageTrackerWaitingPrevCount = null;',
    pick(/function stageTrackerWaitingOnYou\(d\) \{[\s\S]*?\n    \}\n/, 'stageTrackerWaitingOnYou'),
    pick(/function stageTrackerWaitKindWords\(kind\) \{[^\n]*\n/, 'stageTrackerWaitKindWords'),
    pick(/function stageTrackerWaitingTitle\(w, nowMs\) \{[\s\S]*?\n    \}\n/, 'stageTrackerWaitingTitle'),
    pick(/function stageTrackerRenderWaiting\(d, nowMs\) \{[\s\S]*?\n    \}\n/, 'stageTrackerRenderWaiting'),
    pick(/function stageTrackerWaitingToast\(w, nowMs\) \{[\s\S]*?\n    \}\n/, 'stageTrackerWaitingToast'),
    pick(/function stageTrackerWaitingToastText\(w\) \{[\s\S]*?\n    \}\n/, 'stageTrackerWaitingToastText'),
    pick(/function showWaitingOnYou\(\) \{[\s\S]*?\n    \}\n/, 'showWaitingOnYou'),
    pick(/function stageTrackerApplyUrlFilter\(\) \{[\s\S]*?\n    \}\n/, 'stageTrackerApplyUrlFilter'),
    'return { stageTrackerWaitingOnYou, stageTrackerWaitingTitle, stageTrackerRenderWaiting, stageTrackerWaitingToast, stageTrackerWaitingToastText, showWaitingOnYou, stageTrackerApplyUrlFilter, toasts, navigated, filtered, filter: () => _stageTrackerFilter };',
  ].join('\n');
  const store = {};
  const els = {};
  function el(id) {
    const attrs = {};
    const label = { textContent: '' };
    return els[id] = { id, textContent: '', title: '', hidden: false, attrs,
      setAttribute(k, v) { attrs[k] = v; }, removeAttribute(k) { delete attrs[k]; }, hasAttribute(k) { return k in attrs; },
      querySelector() { return label; }, label };
  }
  ['activity-monitor-waiting-badge', 'oc-waiting-on-you', 'activity-monitor-waiting'].forEach(el);
  els['activity-monitor-waiting-badge'].attrs['data-zero'] = '';
  els['oc-waiting-on-you'].hidden = true;
  const window = Object.assign({ hiveKioskMode: () => false, location: { href: 'http://h/' } }, env || {});
  const localStorage = { getItem: k => (k in store ? store[k] : null), setItem: (k, v) => { store[k] = String(v); } };
  const history = { replaceState(_, __, u) { window.location.href = u; } };
  const document = { getElementById: id => els[id] || null };
  const api = new Function('window', 'localStorage', 'history', 'document', 'URL', src)(window, localStorage, history, document, URL);
  return Object.assign(api, { els, store, window });
}

const payload = (items) => ({ waiting_on_human: { count: items.length, items } });
const T0 = Date.parse('2026-10-20T12:00:00Z');
const items3 = [
  { repo: 'o/r', number: 15312, type: 'pr', kind: 'needs-decision', since: '2026-10-20T06:00:00Z', who: 'operator' },
  { repo: 'o/r', number: 15433, type: 'pr', kind: 'ready-for-human-merge', since: '2026-10-20T09:00:00Z', who: 'operator' },
  { repo: 'o/r', number: 41, type: 'pr', kind: 'returned-to-contributor-offline', since: '2026-10-20T11:00:00Z', who: 'karibhou' },
];

test('badge, pill and chip read the block; pill hides at zero', () => {
  const api = load();
  api.stageTrackerRenderWaiting(payload(items3), T0);
  const badge = api.els['activity-monitor-waiting-badge'], pill = api.els['oc-waiting-on-you'];
  assert.strictEqual(badge.textContent, '3');
  assert.ok(!badge.hasAttribute('data-zero'));
  assert.strictEqual(pill.hidden, false);
  assert.strictEqual(pill.label.textContent, 'Waiting on you · 3');
  assert.match(pill.title, /^3 waiting on you/);
  assert.match(pill.title, /r#15312 — needs your decision · 6h/);
  assert.match(pill.title, /r#41 — contributor offline — adopt or reroute \(@karibhou\)/);
  assert.strictEqual(api.els['activity-monitor-waiting'].textContent, 'Waiting on you (3)');
  api.stageTrackerRenderWaiting(payload([]), T0);
  assert.strictEqual(badge.textContent, '0');
  assert.ok(badge.hasAttribute('data-zero'));
  assert.strictEqual(pill.hidden, true);
  assert.strictEqual(pill.title, 'Nothing is waiting on you');
});

test('tooltip lists at most five items and says how many more', () => {
  const api = load();
  const many = Array.from({ length: 8 }, (_, i) => ({ repo: 'o/r', number: i + 1, kind: 'needs-decision', who: 'operator' }));
  const title = api.stageTrackerWaitingTitle({ count: 8, items: many }, T0);
  assert.strictEqual(title.split('\n').length, 7);
  assert.match(title, /… and 3 more$/);
});

test('older payloads without the block fall back to per-item flags', () => {
  const api = load();
  const w = api.stageTrackerWaitingOnYou({ items: [
    { repo: 'o/r', number: 1, kind: 'pr', waiting_on_you: true, wait_kind: 'needs-decision' },
    { repo: 'o/r', number: 2, kind: 'pr', waiting_on_you: true, nested_under: 9 },
    { repo: 'o/r', number: 3, kind: 'pr' },
  ] });
  assert.strictEqual(w.count, 1);
  assert.strictEqual(w.items[0].number, 1);
});

test('toast fires once when the count rises, debounced ten minutes, with a Show action', () => {
  const api = load();
  api.stageTrackerRenderWaiting(payload(items3.slice(0, 1)), T0);
  assert.strictEqual(api.toasts.length, 0, 'first payload seeds the baseline, no toast');
  api.stageTrackerRenderWaiting(payload(items3), T0 + 1000);
  assert.strictEqual(api.toasts.length, 1);
  assert.strictEqual(api.toasts[0].type, 'warning');
  assert.strictEqual(api.toasts[0].msg, '3 items need your decision — #15312, #15433, #41');
  assert.strictEqual(api.toasts[0].opts.actionLabel, 'Show');
  assert.ok(api.toasts[0].opts.autoDismissMs > 0);
  api.toasts[0].opts.onAction();
  assert.deepStrictEqual(api.navigated, ['activity-monitor-section']);
  assert.deepStrictEqual(api.filtered, [['waitingOnYou', true]]);
  // Within the gap: a further rise stays quiet; a drop never toasts.
  api.stageTrackerRenderWaiting(payload(items3.concat([{ repo: 'o/r', number: 5, kind: 'needs-decision', who: 'operator' }])), T0 + 60 * 1000);
  api.stageTrackerRenderWaiting(payload(items3), T0 + 61 * 1000);
  assert.strictEqual(api.toasts.length, 1);
  // After the gap a rise toasts again, and the long list is elided.
  api.stageTrackerRenderWaiting(payload(items3.concat([{ repo: 'o/r', number: 5, kind: 'needs-decision', who: 'operator' }])), T0 + 11 * 60 * 1000);
  assert.strictEqual(api.toasts.length, 2);
  assert.strictEqual(api.toasts[1].msg, '4 items need your decision — #15312, #15433, #41 …');
});

test('toast text uses the singular for one item', () => {
  const api = load();
  assert.strictEqual(api.stageTrackerWaitingToastText({ count: 1, items: items3.slice(0, 1) }), '1 item needs your decision — #15312');
});

test('kiosk mode never toasts', () => {
  const api = load({ hiveKioskMode: () => true });
  api.stageTrackerRenderWaiting(payload([]), T0);
  api.stageTrackerRenderWaiting(payload(items3), T0 + 1000);
  assert.strictEqual(api.toasts.length, 0);
});

test('?filter=waiting-on-you applies the filter once and drops the parameter', () => {
  const api = load({ location: { href: 'http://h/?filter=waiting-on-you' } });
  assert.strictEqual(api.stageTrackerApplyUrlFilter(), true);
  assert.strictEqual(api.filter().waitingOnYou, true);
  assert.strictEqual(api.window.location.href, 'http://h/#section-activity-monitor-section');
  const plain = load({ location: { href: 'http://h/#section-governor' } });
  assert.strictEqual(plain.stageTrackerApplyUrlFilter(), false);
});
