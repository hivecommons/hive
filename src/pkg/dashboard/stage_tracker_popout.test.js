// Pop out & full screen for the Hive activity monitor (hivecommons/hive#15829):
// the ⧉ header button opens the SPA in a named popup on
// `?popout=1#section-activity-monitor-section`, boot.js flags the body before
// first paint so the chrome never shows, and ⛶ toggles the Fullscreen API on
// the document with a body class that shares the pop-out layout. The
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

function fakeEl(id) {
  const attrs = {};
  const classes = new Set();
  return {
    id, textContent: '', title: '', attrs,
    classList: { add: c => classes.add(c), remove: c => classes.delete(c), contains: c => classes.has(c), toggle(c, on) { on ? classes.add(c) : classes.delete(c); } },
    setAttribute(k, v) { attrs[k] = v; }, getAttribute(k) { return attrs[k]; },
    contains(n) { return n === this || (n && n.parent === this); },
  };
}

function load(env) {
  const src = [
    'var toasts = [];',
    'function showToast(msg, type) { toasts.push({ msg, type }); }',
    pick(/const STAGE_TRACKER_POPOUT_PARAM = [^\n]*\n/, 'POPOUT_PARAM'),
    pick(/const STAGE_TRACKER_POPOUT_WINDOW_NAME = [^\n]*\n/, 'WINDOW_NAME'),
    pick(/const STAGE_TRACKER_POPOUT_FEATURES = [^\n]*\n/, 'FEATURES'),
    pick(/const STAGE_TRACKER_POPOUT_HASH = [^\n]*\n/, 'HASH'),
    pick(/const STAGE_TRACKER_POPOUT_TITLE = [^\n]*\n/, 'TITLE'),
    pick(/const STAGE_TRACKER_FULLSCREEN_KEY = [^\n]*\n/, 'FULLSCREEN_KEY'),
    pick(/const STAGE_TRACKER_FULLSCREEN_LABELS = \{[\s\S]*?\n    \};\n/, 'FULLSCREEN_LABELS'),
    'var _stageTrackerPopoutHiveId = "";',
    pick(/function stageTrackerPopoutUrl\(href\) \{[\s\S]*?\n    \}\n/, 'stageTrackerPopoutUrl'),
    pick(/function stageTrackerDashboardUrl\(href\) \{[\s\S]*?\n    \}\n/, 'stageTrackerDashboardUrl'),
    pick(/function stageTrackerPopoutTitle\(hiveId\) \{[\s\S]*?\n    \}\n/, 'stageTrackerPopoutTitle'),
    pick(/function stageTrackerPopoutMode\(\) \{[\s\S]*?\n    \}\n/, 'stageTrackerPopoutMode'),
    pick(/function stageTrackerPopoutSyncTitle\(hiveId\) \{[\s\S]*?\n    \}\n/, 'stageTrackerPopoutSyncTitle'),
    pick(/function openStageTrackerPopout\(\) \{[\s\S]*?\n    \}\n/, 'openStageTrackerPopout'),
    pick(/function stageTrackerReturnToDashboard\(\) \{[\s\S]*?\n    \}\n/, 'stageTrackerReturnToDashboard'),
    pick(/function stageTrackerFullscreenTarget\(\) \{[\s\S]*?\n    \}\n/, 'stageTrackerFullscreenTarget'),
    pick(/function stageTrackerFullscreenButton\(\) \{[\s\S]*?\n    \}\n/, 'stageTrackerFullscreenButton'),
    pick(/function stageTrackerIsFullscreen\(\) \{[\s\S]*?\n    \}\n/, 'stageTrackerIsFullscreen'),
    pick(/function stageTrackerFullscreenLabel\(isFull\) \{[\s\S]*?\n    \}\n/, 'stageTrackerFullscreenLabel'),
    pick(/function stageTrackerRenderFullscreenButton\(\) \{[\s\S]*?\n    \}\n/, 'stageTrackerRenderFullscreenButton'),
    pick(/function toggleStageTrackerFullscreen\(\) \{[\s\S]*?\n    \}\n/, 'toggleStageTrackerFullscreen'),
    pick(/function stageTrackerFullscreenHotkey\(ev, inTracker, typing, popout\) \{[\s\S]*?\n    \}\n/, 'stageTrackerFullscreenHotkey'),
    'return { stageTrackerPopoutUrl, stageTrackerDashboardUrl, stageTrackerPopoutTitle, stageTrackerPopoutSyncTitle, openStageTrackerPopout, stageTrackerReturnToDashboard, stageTrackerRenderFullscreenButton, toggleStageTrackerFullscreen, stageTrackerFullscreenHotkey, stageTrackerIsFullscreen, toasts };',
  ].join('\n');
  const body = fakeEl('body');
  const root = { requestFullscreen: null, calls: [] };
  root.requestFullscreen = function() { root.calls.push('request'); document.fullscreenElement = root; return Promise.resolve(); };
  const btn = fakeEl('activity-monitor-fullscreen');
  const document = {
    body, documentElement: root, fullscreenElement: null, title: '',
    exitFullscreen() { root.calls.push('exit'); document.fullscreenElement = null; return Promise.resolve(); },
    getElementById(id) { return id === 'activity-monitor-fullscreen' ? btn : null; },
  };
  const window = Object.assign({ location: { href: 'http://127.0.0.1:8099/?theme=light#section-agents-section' }, closed: false, opened: [], hivePopoutMode: () => false }, env || {});
  window.open = window.open || function(url, name, features) { window.opened.push({ url, name, features }); return { focus() { window.focusedChild = true; } }; };
  const api = new Function('window', 'document', 'URL', src)(window, document, URL);
  return { api, window, document, body, root, btn };
}

// Scenario (c): popout URL building.
test('pop-out URL keeps origin/query, adds popout=1, drops kiosk/filter, lands on the tracker', () => {
  const { api } = load();
  const u = new URL(api.stageTrackerPopoutUrl('http://127.0.0.1:8099/?theme=light&kiosk=1&filter=waiting-on-you#section-agents-section'));
  assert.strictEqual(u.origin + u.pathname, 'http://127.0.0.1:8099/');
  assert.strictEqual(u.searchParams.get('popout'), '1');
  assert.strictEqual(u.searchParams.get('theme'), 'light');
  assert.strictEqual(u.searchParams.get('kiosk'), null);
  assert.strictEqual(u.searchParams.get('filter'), null);
  assert.strictEqual(u.hash, '#section-activity-monitor-section');
  assert.strictEqual(api.stageTrackerPopoutUrl('not a url'), '');
  const back = new URL(api.stageTrackerDashboardUrl(u.toString()));
  assert.strictEqual(back.searchParams.get('popout'), null, 'return URL drops popout');
  assert.strictEqual(back.searchParams.get('theme'), 'light');
});

test('⧉ opens one named popup window with the popup features and focuses it; a blocked popup toasts', () => {
  const { api, window } = load();
  const win = api.openStageTrackerPopout();
  assert.ok(win);
  assert.strictEqual(window.opened.length, 1);
  assert.strictEqual(window.opened[0].name, 'hive-activity-monitor');
  assert.strictEqual(window.opened[0].features, 'popup,width=1600,height=900');
  assert.ok(window.opened[0].url.includes('popout=1'));
  assert.strictEqual(window.focusedChild, true);
  const blocked = load({ open: () => null });
  assert.strictEqual(blocked.api.openStageTrackerPopout(), null);
  assert.strictEqual(blocked.api.toasts.length, 1);
  assert.match(blocked.api.toasts[0].msg, /blocked/);
});

// Scenario (a): popout mode hides the chrome and renders only the tracker.
test('popout mode: boot flags the body before paint and the CSS removes every piece of chrome', () => {
  assert.ok(html.includes("popout=u.searchParams.get('popout')==='1'"), 'boot.js reads ?popout=1');
  assert.ok(html.includes("window.hivePopoutMode=function(){ return !!window.HIVE_POPOUT; };"));
  assert.ok(html.includes("document.body.classList.add('popout-mode');"));
  assert.ok(html.includes("if(kiosk||window.HIVE_POPOUT){\n    if(!location.hash) history.replaceState(null,'',location.pathname+location.search+'#section-activity-monitor-section');"), 'popout lands on the tracker section');
  for (const rule of [
    'body.popout-mode .oc-sidebar, body.popout-mode .oc-sidebar-resize, body.popout-mode .oc-topbar,',
    'html body.popout-mode #oc-topbar[id], html body.ham-fullscreen #oc-topbar[id] { display: none !important; }',
    'body.popout-mode #hive-dashboard-root > [data-dashboard-section]:not(#activity-monitor-section),',
    'body.ham-fullscreen #hive-dashboard-root > [data-dashboard-section]:not(#activity-monitor-section) { display: none !important; }',
    'body.popout-mode .ham-popout-return { display: inline-flex; align-items: center; }',
    'body.popout-mode .ham-popout-btn, body.ham-fullscreen .ham-popout-btn,',
  ]) assert.ok(html.includes(rule), 'missing CSS rule: ' + rule);
  // Header tools render next to the gear, dispatch through data-action and
  // never collapse the section (data-stop).
  assert.match(html, /id="activity-monitor-popout" data-action="openStageTrackerPopout" data-stop="1"/);
  assert.match(html, /id="activity-monitor-fullscreen" data-action="toggleStageTrackerFullscreen" data-stop="1" aria-pressed="false"/);
  assert.match(html, /id="activity-monitor-return" data-action="stageTrackerReturnToDashboard" data-stop="1"/);
  // The welcome tour and NPS card stay in the main window; the title is the monitor's.
  assert.ok(html.includes('if (window.hivePopoutMode && window.hivePopoutMode()) return;'));
  assert.ok(html.includes('if (!(window.hivePopoutMode && window.hivePopoutMode())) document.title = "Hive Dashboard for " + repoDisplay;'));
  assert.ok(html.includes("if (typeof stageTrackerPopoutSyncTitle === 'function') stageTrackerPopoutSyncTitle(hiveId);"));
  // Nothing is persisted for the layout: the flag lives in the URL only.
  assert.ok(!html.includes("localStorage.setItem('hive-popout"));
});

test('popout window title carries the hive id once status delivers it; the main window is untouched', () => {
  const main = load();
  main.api.stageTrackerPopoutSyncTitle('hive-abc');
  assert.strictEqual(main.document.title, '', 'main window keeps its own title');
  const pop = load({ hivePopoutMode: () => true });
  pop.api.stageTrackerPopoutSyncTitle();
  assert.strictEqual(pop.document.title, 'Hive · Activity Monitor');
  pop.api.stageTrackerPopoutSyncTitle('hive-abc');
  assert.strictEqual(pop.document.title, 'Hive · Activity Monitor · hive-abc');
  assert.strictEqual(pop.api.stageTrackerPopoutTitle('  '), 'Hive · Activity Monitor');
});

test('⤶ Return focuses the opener and closes; a hand-opened tab navigates back instead', () => {
  let focused = false, closed = false, assigned = null;
  const popup = load({ opener: { closed: false, focus() { focused = true; } }, close() { closed = true; this.closed = true; } });
  popup.api.stageTrackerReturnToDashboard();
  assert.ok(focused && closed);
  const tab = load({ opener: null, close() {}, location: { href: 'http://h/?popout=1#section-activity-monitor-section', assign(u) { assigned = u; } } });
  tab.api.stageTrackerReturnToDashboard();
  assert.ok(assigned && !assigned.includes('popout=1') && assigned.includes('#section-activity-monitor-section'));
});

// Scenario (b): fullscreen toggle calls requestFullscreen on the wrapper and flips the label.
test('⛶ requests full screen on the document, sets body.ham-fullscreen and flips the label; a second press exits', async () => {
  const { api, root, body, btn, document } = load();
  api.stageTrackerRenderFullscreenButton();
  assert.strictEqual(btn.textContent, '⛶');
  assert.strictEqual(btn.attrs['aria-pressed'], 'false');
  assert.strictEqual(await api.toggleStageTrackerFullscreen(), true);
  assert.deepStrictEqual(root.calls, ['request']);
  assert.ok(body.classList.contains('ham-fullscreen'));
  assert.strictEqual(btn.textContent, '⛶ Exit full screen');
  assert.strictEqual(btn.attrs['aria-pressed'], 'true');
  assert.match(btn.title, /^Exit full screen/);
  assert.strictEqual(await api.toggleStageTrackerFullscreen(), true);
  assert.deepStrictEqual(root.calls, ['request', 'exit']);
  assert.ok(!body.classList.contains('ham-fullscreen'));
  assert.strictEqual(btn.textContent, '⛶');
  assert.strictEqual(btn.attrs['aria-pressed'], 'false');
  // Esc (native exit) fires fullscreenchange with no fullscreenElement: the layout drops.
  body.classList.add('ham-fullscreen');
  document.fullscreenElement = null;
  api.stageTrackerRenderFullscreenButton();
  assert.ok(!body.classList.contains('ham-fullscreen'));
});

test('a refused requestFullscreen rolls the layout back and toasts', async () => {
  const { api, body, root } = load();
  root.requestFullscreen = () => Promise.reject(new Error('denied'));
  assert.strictEqual(await api.toggleStageTrackerFullscreen(), false);
  assert.ok(!body.classList.contains('ham-fullscreen'));
  assert.match(api.toasts[0].msg, /denied/);
});

test('`f` toggles only as a bare key with focus in the tracker (or anywhere in a pop-out), never while typing', () => {
  const { api } = load();
  const f = (extra) => Object.assign({ key: 'f', ctrlKey: false, metaKey: false, altKey: false, shiftKey: false }, extra || {});
  assert.strictEqual(api.stageTrackerFullscreenHotkey(f(), true, false, false), true);
  assert.strictEqual(api.stageTrackerFullscreenHotkey(f(), false, false, false), false, 'focus outside the tracker');
  assert.strictEqual(api.stageTrackerFullscreenHotkey(f(), false, false, true), true, 'pop-out: the page is the tracker');
  assert.strictEqual(api.stageTrackerFullscreenHotkey(f(), true, true, true), false, 'typing in the search box');
  assert.strictEqual(api.stageTrackerFullscreenHotkey(f({ metaKey: true }), true, false, false), false, '⌘F is find');
  assert.strictEqual(api.stageTrackerFullscreenHotkey(f({ key: 'F', shiftKey: true }), true, false, false), false);
  assert.strictEqual(api.stageTrackerFullscreenHotkey(f({ key: 'g' }), true, false, false), false);
});
