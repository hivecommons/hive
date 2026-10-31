// Activity Monitor display mode (hivecommons/hive#15925): 📺 navigates to the
// current URL with `display=1`, Esc (outside browser full screen) or `d`
// leaves, and a display hides every row action. Neither path requests full
// screen or a wake lock. The functions are lifted out of index.html so this
// runs against the shipped source.
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
    pick(/const STAGE_TRACKER_POPOUT_HASH = [^\n]*\n/, 'POPOUT_HASH'),
    pick(/const STAGE_TRACKER_DISPLAY_PARAM = [^\n]*\n/, 'DISPLAY_PARAM'),
    pick(/const STAGE_TRACKER_DISPLAY_KEY = [^\n]*\n/, 'DISPLAY_KEY'),
    pick(/const STAGE_TRACKER_DISPLAY_LABELS = \{[\s\S]*?\n    \};\n/, 'DISPLAY_LABELS'),
    pick(/function stageTrackerDisplayMode\(\) \{[\s\S]*?\n    \}\n/, 'stageTrackerDisplayMode'),
    pick(/function stageTrackerDisplayUrl\(href, on\) \{[\s\S]*?\n    \}\n/, 'stageTrackerDisplayUrl'),
    pick(/function stageTrackerDisplayHotkey\(ev, inTracker, typing, display, fullscreen\) \{[\s\S]*?\n    \}\n/, 'stageTrackerDisplayHotkey'),
    pick(/function toggleStageTrackerDisplay\(\) \{[\s\S]*?\n    \}\n/, 'toggleStageTrackerDisplay'),
    pick(/function stageTrackerRenderDisplayButton\(\) \{[\s\S]*?\n    \}\n/, 'stageTrackerRenderDisplayButton'),
    pick(/function stageTrackerDisplayBlocksRowAction\(ev, display\) \{[\s\S]*?\n    \}\n/, 'stageTrackerDisplayBlocksRowAction'),
    pick(/const STAGE_TRACKER_DISPLAY_OVERLAY_SELECTOR = [^\n]*\n/, 'DISPLAY_OVERLAY_SELECTOR'),
    pick(/function stageTrackerDisplayOverlayOpen\(doc\) \{[\s\S]*?\n    \}\n/, 'stageTrackerDisplayOverlayOpen'),
    'return { stageTrackerDisplayUrl, stageTrackerDisplayHotkey, toggleStageTrackerDisplay, stageTrackerRenderDisplayButton, stageTrackerDisplayBlocksRowAction, stageTrackerDisplayOverlayOpen, STAGE_TRACKER_DISPLAY_OVERLAY_SELECTOR };',
  ].join('\n');
  const attrs = {};
  const btn = { textContent: '', title: '', attrs, setAttribute(k, v) { attrs[k] = v; } };
  const calls = [];
  const document = {
    getElementById: id => (id === 'activity-monitor-display' ? btn : null),
    documentElement: { requestFullscreen() { calls.push('requestFullscreen'); } },
  };
  const window = Object.assign({
    location: { href: 'https://hive.example/?theme=light&am.kind=pr#section-agents-section', assign(u) { calls.push(u); } },
    hiveDisplayMode: () => false,
  }, env || {});
  const navigator = { wakeLock: { request() { calls.push('wakeLock'); } } };
  const api = new Function('window', 'document', 'navigator', 'URL', src)(window, document, navigator, URL);
  return { api, btn, calls };
}

test('display URL adds display=1 keeping am.* and theme, drops kiosk and the one-shot filter, and lands on the tracker', () => {
  const { api } = load();
  const u = new URL(api.stageTrackerDisplayUrl('https://hive.example/?theme=light&am.kind=pr&am.group=repo&kiosk=1&filter=waiting-on-you#section-agents-section', true));
  assert.strictEqual(u.searchParams.get('display'), '1');
  assert.strictEqual(u.searchParams.get('theme'), 'light');
  assert.strictEqual(u.searchParams.get('am.kind'), 'pr');
  assert.strictEqual(u.searchParams.get('am.group'), 'repo');
  assert.strictEqual(u.searchParams.get('kiosk'), null);
  assert.strictEqual(u.searchParams.get('filter'), null);
  assert.strictEqual(u.hash, '#section-activity-monitor-section');
  const out = new URL(api.stageTrackerDisplayUrl(u.toString(), false));
  assert.strictEqual(out.searchParams.get('display'), null);
  assert.strictEqual(out.searchParams.get('am.kind'), 'pr');
  assert.strictEqual(out.searchParams.get('theme'), 'light');
  assert.strictEqual(out.hash, '#section-activity-monitor-section');
  assert.strictEqual(api.stageTrackerDisplayUrl('not a url', true), '');
});

test('d toggles from the tracker (anywhere while displaying); Esc leaves only outside browser full screen; never while typing or with a modifier', () => {
  const { api } = load();
  const k = (key, mods) => Object.assign({ key }, mods || {});
  assert.strictEqual(api.stageTrackerDisplayHotkey(k('d'), true, false, false, false), true, 'd enters from the tracker');
  assert.strictEqual(api.stageTrackerDisplayHotkey(k('d'), false, false, false, false), false, 'd outside the tracker does nothing');
  assert.strictEqual(api.stageTrackerDisplayHotkey(k('d'), false, false, true, false), true, 'd leaves from anywhere while displaying');
  assert.strictEqual(api.stageTrackerDisplayHotkey(k('Escape'), false, false, true, false), true, 'Esc leaves');
  assert.strictEqual(api.stageTrackerDisplayHotkey(k('Escape'), false, false, true, true), false, 'browser full screen consumes Esc itself');
  assert.strictEqual(api.stageTrackerDisplayHotkey(k('Escape'), true, false, false, false), false, 'Esc never enters');
  assert.strictEqual(api.stageTrackerDisplayHotkey(k('d'), true, true, true, false), false, 'typing');
  for (const m of ['ctrlKey', 'metaKey', 'altKey', 'shiftKey']) assert.strictEqual(api.stageTrackerDisplayHotkey(k('d', { [m]: true }), true, false, true, false), false, m);
  assert.strictEqual(api.stageTrackerDisplayHotkey(k('f'), true, false, true, false), false);
  assert.strictEqual(api.stageTrackerDisplayHotkey(null, true, false, true, false), false);
});

test('📺 navigates in and out without requesting full screen or a wake lock', () => {
  const enter = load();
  const url = enter.api.toggleStageTrackerDisplay();
  assert.ok(url.includes('display=1') && url.includes('am.kind=pr'));
  assert.deepStrictEqual(enter.calls, [url]);
  const leave = load({ hiveDisplayMode: () => true, location: { href: 'https://hive.example/?display=1&am.kind=pr#section-activity-monitor-section', assign(u) { leave.calls.push(u); } } });
  const back = leave.api.toggleStageTrackerDisplay();
  assert.ok(!back.includes('display=') && back.includes('am.kind=pr'));
  assert.deepStrictEqual(leave.calls, [back]);
  const fn = pick(/function toggleStageTrackerDisplay\(\) \{[\s\S]*?\n    \}\n/, 'toggleStageTrackerDisplay');
  assert.ok(!/requestFullscreen|wakeLock/.test(fn));
});

test('the 📺 button reads as an exit while displaying', () => {
  const off = load();
  off.api.stageTrackerRenderDisplayButton();
  assert.strictEqual(off.btn.textContent, '📺');
  assert.strictEqual(off.btn.attrs['aria-pressed'], 'false');
  const on = load({ hiveDisplayMode: () => true });
  on.api.stageTrackerRenderDisplayButton();
  assert.strictEqual(on.btn.textContent, '📺 Exit display');
  assert.strictEqual(on.btn.attrs['aria-pressed'], 'true');
  assert.match(on.btn.title, /Esc or d/);
});

test('a display keeps row actions away: context menu, menu keys, Space and modifier-click selection', () => {
  const { api } = load();
  const b = api.stageTrackerDisplayBlocksRowAction;
  assert.strictEqual(b({ type: 'contextmenu' }, true), true);
  assert.strictEqual(b({ type: 'keydown', key: 'ContextMenu' }, true), true);
  assert.strictEqual(b({ type: 'keydown', key: 'F10', shiftKey: true }, true), true);
  assert.strictEqual(b({ type: 'keydown', key: ' ' }, true), true);
  assert.strictEqual(b({ type: 'click', shiftKey: true }, true), true);
  assert.strictEqual(b({ type: 'click', metaKey: true }, true), true);
  assert.strictEqual(b({ type: 'click' }, true), false, 'a plain click still opens links');
  assert.strictEqual(b({ type: 'keydown', key: 'd' }, true), false);
  for (const ev of [{ type: 'contextmenu' }, { type: 'keydown', key: ' ' }, { type: 'click', shiftKey: true }]) {
    assert.strictEqual(b(ev, false), false, 'nothing changes when display mode is off');
  }
});

test('Esc leaves display mode only when no popover or dialog is open: settings, labels, merged and queue popovers and hive dialogs all take Esc first', () => {
  const { api } = load();
  const sel = api.STAGE_TRACKER_DISPLAY_OVERLAY_SELECTOR;
  for (const id of ['activity-monitor-settings-overlay', 'activity-monitor-labels-overlay', 'activity-monitor-merged-pop', 'activity-monitor-queue-pop']) assert.ok(sel.includes('#' + id), id);
  assert.ok(sel.includes('.hive-dialog-overlay'));
  const doc = els => ({ querySelectorAll: s => (s === sel ? els : []) });
  const el = (o) => Object.assign({ hidden: false, classList: { contains: () => false }, style: {} }, o || {});
  assert.strictEqual(api.stageTrackerDisplayOverlayOpen(doc([])), false, 'nothing open');
  assert.strictEqual(api.stageTrackerDisplayOverlayOpen(doc([el({ hidden: true })])), false, 'hidden attribute (merged/queue popovers)');
  assert.strictEqual(api.stageTrackerDisplayOverlayOpen(doc([el({ classList: { contains: c => c === 'hidden' } })])), false, '.hidden class (settings overlay)');
  assert.strictEqual(api.stageTrackerDisplayOverlayOpen(doc([el({ style: { display: 'none' } })])), false, 'display:none');
  assert.strictEqual(api.stageTrackerDisplayOverlayOpen(doc([el({ hidden: true }), el()])), true, 'one open popover is enough');
  assert.strictEqual(api.stageTrackerDisplayOverlayOpen(null), false);
  assert.ok(html.includes('if (stageTrackerDisplayOverlayOpen(document)) return;\n      e.preventDefault();\n'), 'the Esc listener checks overlays before leaving');
  assert.ok(html.includes('if (stageTrackerDisplayMode()) stageTrackerDisplayExit(); else toggleStageTrackerDisplay();'), 'the Esc listener leaves through stageTrackerDisplayExit like the overlay control');
  // The merged and queue popover closers also claim Esc so later listeners see defaultPrevented.
  assert.ok(html.includes("if (pop && !pop.hidden) { e.preventDefault(); hamMergedToggle(false);"), 'merged popover claims Esc');
  assert.ok(html.includes("if (pop && !pop.hidden) { e.preventDefault(); hamQueueToggle(false);"), 'queue popover claims Esc');
});

test('display mode wiring: header button, boot flag, chrome-less CSS and hidden row actions; kiosk still hides every header tool', () => {
  assert.match(html, /id="activity-monitor-display" data-action="toggleStageTrackerDisplay" data-stop="1" aria-pressed="false"/);
  assert.ok(html.includes("display=u.searchParams.get('display')==='1'"));
  assert.ok(html.includes('window.HIVE_DISPLAY=display&&!kiosk;'), 'kiosk wins');
  assert.ok(html.includes("document.body.classList.add('display-mode');"));
  for (const rule of [
    'body.display-mode .hive-chat-panel, body.display-mode #welcome-overlay, body.display-mode .nps-card { display: none !important; }',
    'html body.display-mode #oc-topbar[id] { display: none !important; }',
    'body.display-mode #hive-dashboard-root > [data-dashboard-section]:not(#activity-monitor-section) { display: none !important; }',
    'body.display-mode #activity-monitor-section .ham-row-tools, body.display-mode #activity-monitor-selbar,',
    'body.display-mode .ham-decision-acts { display: none !important; }',
    'body.kiosk-mode .ham-header-tool, body.kiosk-mode .ham-popout-return { display: none !important; }',
  ]) assert.ok(html.includes(rule), 'missing CSS rule: ' + rule);
  assert.ok(!/body\.display-mode[^{]*\{[^}]*cursor:\s*none/.test(html), 'display mode keeps the cursor');
  assert.ok(html.includes('function setLayout(mode) { if (window.hiveDisplayMode && window.hiveDisplayMode()) return; localStorage.setItem(LAYOUT_KEY, mode); }'), 'theme is never persisted from a display');
  assert.ok(html.includes('function dashboardLayoutWrite(){if(window.hiveDisplayMode&&window.hiveDisplayMode())return;'), 'layout is never persisted from a display');
});

// ---- Display controls (hivecommons/hive#15929) ----------------------------
// The overlay is display-mode only, and Full screen / Keep screen awake are
// separate explicit controls: nothing requests either on load or on entering
// display mode, unsupported and denied states render disabled or explained,
// never throw and never retry on their own, and Copy display link carries
// only view parameters. Fakes stand in for navigator.wakeLock,
// navigator.clipboard and document.fullscreenEnabled.
const fnSrc = name => {
  const start = html.indexOf('function ' + name + '(');
  assert.ok(start >= 0, name + ' not found');
  let depth = 0;
  for (let i = html.indexOf('{', start); i < html.length; i++) {
    if (html[i] === '{') depth++;
    else if (html[i] === '}' && --depth === 0) return html.slice(start, i + 1) + '\n';
  }
  throw new Error('unbalanced ' + name);
};
const between = (begin, end) => {
  const m = html.match(new RegExp('// ' + begin + '\\n([\\s\\S]*?)// ' + end));
  assert.ok(m, begin + ' not found');
  return m[1];
};
const constLine = name => pick(new RegExp('^\\s*const ' + name + ' = .*$', 'm'), name) + '\n';

const controlsSrc = [
  constLine('STAGE_TRACKER_FILTER_KEY'), constLine('STAGE_TRACKER_PAGE_SIZES'), constLine('STAGE_TRACKER_DEFAULT_PAGE_SIZE'),
  constLine('STAGE_TRACKER_AGE_MS'), constLine('STAGE_TRACKER_SORTS'), constLine('STAGE_TRACKER_ACTED_FILTERS'),
  constLine('STAGE_TRACKER_GROUP_KEY'), constLine('STAGE_TRACKER_GROUPS'), constLine('STAGE_TRACKER_GROUP_DEFAULT'),
  fnSrc('stageTrackerDefaultFilter'), fnSrc('stageTrackerNormalizeFilter'),
  between('tracker url view helpers begin', 'tracker url view helpers end'),
  between('tracker display playlist helpers begin', 'tracker display playlist helpers end'),
  constLine('STAGE_TRACKER_POPOUT_HASH'), constLine('STAGE_TRACKER_DISPLAY_PARAM'),
  pick(/const STAGE_TRACKER_FULLSCREEN_LABELS = \{[\s\S]*?\n    \};\n/, 'FULLSCREEN_LABELS'),
  pick(/const STAGE_TRACKER_DISPLAY_CONTROLS_HTML = [\s\S]*? hidden>';\n/, 'DISPLAY_CONTROLS_HTML'),
  constLine('STAGE_TRACKER_DISPLAY_IDLE_MS'),
  pick(/const STAGE_TRACKER_DISPLAY_CONTROL_LABELS = \{[\s\S]*?\n    \};\n/, 'DISPLAY_CONTROL_LABELS'),
  between('tracker display controls helpers begin', 'tracker display controls helpers end'),
  'var _stageTrackerFilter = Object.assign(stageTrackerDefaultFilter(), { kind: "pr", repo: "hivecommons/hive", labels: ["bug"] });',
  'var _stageTrackerGroup = "repo";',
  'var _stageTrackerPlaylist = null;',
  'var _stageTrackerDisplayLastPointer = 0;', 'var _stageTrackerDisplayHideTimer = null;',
  'var _stageTrackerWakeLock = null;', 'var _stageTrackerWakeLockError = null;', 'var _stageTrackerWakeLockWanted = false;',
  ...['stageTrackerDisplayMode', 'stageTrackerDisplayUrl', 'toggleStageTrackerDisplay',
    'stageTrackerFullscreenTarget', 'stageTrackerFullscreenButton', 'stageTrackerIsFullscreen', 'stageTrackerFullscreenLabel',
    'stageTrackerRenderFullscreenButton', 'toggleStageTrackerFullscreen',
    'stageTrackerDisplayControlsEl', 'stageTrackerDisplayControlsSync', 'stageTrackerDisplayControlsPing',
    'stageTrackerWakeLockSupported', 'stageTrackerRenderDisplayControls', 'stageTrackerWakeLockAcquire',
    'stageTrackerWakeLockRelease', 'stageTrackerToggleWakeLock', 'stageTrackerWakeLockOnVisible', 'stageTrackerDisplayExit',
    'stageTrackerDisplayShowLink', 'stageTrackerCopyDisplayLink', 'stageTrackerDisplayControlsInit'].map(fnSrc),
  'return { stageTrackerDisplayOverlayVisible, stageTrackerWakeLockState, stageTrackerFullscreenSupported, stageTrackerDisplayLinkUrl, stageTrackerFilterFromUrl, stageTrackerParsePlaylist, stageTrackerDisplayControlsInit, stageTrackerDisplayControlsSync, stageTrackerToggleWakeLock, stageTrackerWakeLockOnVisible, stageTrackerCopyDisplayLink, stageTrackerDisplayExit, toggleStageTrackerFullscreen, filter: () => _stageTrackerFilter, setPlaylist: p => { _stageTrackerPlaylist = p; } };',
].join('\n');

function ctlEl(id) {
  const attrs = {};
  const classes = new Set();
  const listeners = {};
  return {
    id, attrs, listeners, hidden: false, disabled: false, title: '', textContent: '', value: '',
    setAttribute(k, v) { attrs[k] = String(v); },
    getAttribute(k) { return k in attrs ? attrs[k] : null; },
    classList: {
      toggle(c, on) { if (on === undefined) on = !classes.has(c); if (on) classes.add(c); else classes.delete(c); return on; },
      add(c) { classes.add(c); }, remove(c) { classes.delete(c); }, contains(c) { return classes.has(c); },
    },
    addEventListener(t, fn) { (listeners[t] = listeners[t] || []).push(fn); },
    focus() { this.focused = true; }, select() { this.selected = true; },
  };
}

function loadControls(opts) {
  opts = opts || {};
  const calls = [];
  const toasts = [];
  const sentinels = [];
  const byId = {};
  const docListeners = {};
  const appended = [];
  const document = {
    fullscreenEnabled: opts.fullscreenEnabled !== undefined ? opts.fullscreenEnabled : true,
    fullscreenElement: null,
    visibilityState: 'visible',
    documentElement: {},
    body: Object.assign(ctlEl('body'), { appendChild(el) { appended.push(el); if (el.id) byId[el.id] = el; } }),
    exitFullscreen() { calls.push('exitFullscreen'); document.fullscreenElement = null; return Promise.resolve(); },
    getElementById: id => byId[id] || null,
    addEventListener(t, fn) { (docListeners[t] = docListeners[t] || []).push(fn); },
    createElement() {
      const root = ctlEl('');
      const kids = {};
      Object.defineProperty(root, 'innerHTML', {
        set(v) {
          root._html = v;
          for (const k of ['exit', 'copy', 'fullscreen', 'wakelock']) if (v.includes('data-display-control="' + k + '"')) kids[k] = ctlEl('');
          if (v.includes('id="activity-monitor-display-link"')) { kids.link = ctlEl('activity-monitor-display-link'); kids.link.hidden = true; byId[kids.link.id] = kids.link; }
        },
        get() { return root._html; },
      });
      root.querySelector = sel => { const m = /data-display-control="(\w+)"/.exec(sel); return m ? kids[m[1]] || null : null; };
      root.kids = kids;
      return root;
    },
  };
  if (opts.fullscreenApi !== false) {
    document.documentElement.requestFullscreen = function() { calls.push('requestFullscreen'); document.fullscreenElement = document.documentElement; return Promise.resolve(); };
  }
  const navigator = {};
  if (opts.wakeLock !== false) {
    navigator.wakeLock = {
      request(type) {
        calls.push('wakeLock:' + type);
        if (opts.wakeLockDenied) return Promise.reject(Object.assign(new Error('Permission denied by policy'), { name: 'NotAllowedError' }));
        const s = { released: false, release() { calls.push('release'); s.released = true; return Promise.resolve(); }, addEventListener() {} };
        sentinels.push(s);
        return Promise.resolve(s);
      },
    };
  }
  if (opts.clipboard !== false) {
    navigator.clipboard = { writeText(t) { calls.push('clipboard:' + t); return opts.clipboardRefused ? Promise.reject(new Error('denied')) : Promise.resolve(); } };
  }
  const window = {
    location: { href: opts.href || 'https://hive.example/?display=1&theme=light&am.kind=pr&token=s3cret&foo=bar#section-activity-monitor-section', assign(u) { calls.push('assign:' + u); } },
    hiveDisplayMode: () => opts.display !== false,
  };
  const localStorage = { getItem: () => null, setItem(k) { calls.push('localStorage:' + k); } };
  const showToast = (m, t) => toasts.push([t, m]);
  const noTimer = () => 0;
  const api = new Function('window', 'document', 'navigator', 'URL', 'localStorage', 'showToast', 'setTimeout', 'clearTimeout', controlsSrc)(window, document, navigator, URL, localStorage, showToast, noTimer, noTimer);
  return { api, calls, toasts, sentinels, docListeners, appended, document, byId };
}
const settle = () => new Promise(r => setTimeout(r, 0));

test('entering display mode adds the overlay but requests neither full screen nor a wake lock', async () => {
  const env = loadControls();
  const root = env.api.stageTrackerDisplayControlsInit();
  assert.ok(root, 'overlay created');
  assert.strictEqual(root.id, 'activity-monitor-display-controls');
  assert.strictEqual(root.attrs.role, 'toolbar');
  assert.strictEqual(env.appended.length, 1);
  for (const t of ['mousemove', 'pointerdown', 'focusin', 'visibilitychange', 'fullscreenchange']) {
    assert.ok(env.docListeners[t] && env.docListeners[t].length, t + ' listener');
    for (const fn of env.docListeners[t]) fn({ type: t });
  }
  await settle();
  assert.deepStrictEqual(env.calls, [], 'no full-screen, wake-lock, clipboard, navigation or storage call on enter');
  assert.strictEqual(root.kids.fullscreen.disabled, false);
  assert.strictEqual(root.kids.wakelock.attrs['data-state'], 'off');
  assert.strictEqual(root.kids.wakelock.attrs['aria-pressed'], 'false');
  assert.strictEqual(env.api.stageTrackerDisplayControlsInit(), root, 'idempotent');
  assert.strictEqual(env.appended.length, 1);
});

test('no overlay and no listeners when display mode is off (and kiosk never sets it)', () => {
  const env = loadControls({ display: false });
  assert.strictEqual(env.api.stageTrackerDisplayControlsInit(), null);
  assert.deepStrictEqual(env.appended, []);
  assert.deepStrictEqual(env.docListeners, {});
  assert.ok(html.includes('window.HIVE_DISPLAY=display&&!kiosk;'), 'kiosk wins, so a headless ?kiosk=1 never gets the overlay');
  assert.ok(html.includes('body.kiosk-mode .ham-display-controls, body.kiosk-mode .ham-playlist { display: none !important; }'));
  assert.ok(html.includes('.ham-display-controls, .ham-playlist { display: none; }'), 'hidden outside display mode');
});

test('the overlay shows on activity and hides after 3 s idle', () => {
  const env = loadControls();
  const vis = env.api.stageTrackerDisplayOverlayVisible;
  assert.strictEqual(vis(0, 5000, 3000), false, 'never shown before any activity');
  assert.strictEqual(vis(1000, 1000, 3000), true);
  assert.strictEqual(vis(1000, 3999, 3000), true);
  assert.strictEqual(vis(1000, 4000, 3000), false);
  assert.strictEqual(vis(5000, 1000, 3000), false, 'clock skew');
  const root = env.api.stageTrackerDisplayControlsInit();
  assert.strictEqual(root.classList.contains('is-visible'), false);
  env.docListeners.mousemove[0]({ type: 'mousemove' });
  assert.strictEqual(root.classList.contains('is-visible'), true);
  env.api.stageTrackerDisplayControlsSync(Date.now() + 3001);
  assert.strictEqual(root.classList.contains('is-visible'), false);
});

test('Keep screen awake: explicit on/off, unsupported renders disabled, denied shows the reason and is never retried', async () => {
  const st = loadControls().api.stageTrackerWakeLockState;
  assert.deepStrictEqual([st(false, null, null).state, st(false, null, null).disabled], ['unsupported', true]);
  assert.strictEqual(st(true, null, null).state, 'off');
  assert.strictEqual(st(true, { released: false }, null).state, 'on');
  assert.strictEqual(st(true, { released: true }, null).state, 'off');
  const denied = st(true, null, new Error('Battery saver'));
  assert.strictEqual(denied.state, 'denied');
  assert.match(denied.title, /Battery saver/);
  assert.strictEqual(denied.disabled, false);

  const on = loadControls();
  const root = on.api.stageTrackerDisplayControlsInit();
  assert.strictEqual(await on.api.stageTrackerToggleWakeLock(), true);
  assert.deepStrictEqual(on.calls, ['wakeLock:screen']);
  assert.strictEqual(root.kids.wakelock.attrs['data-state'], 'on');
  assert.strictEqual(root.kids.wakelock.attrs['aria-pressed'], 'true');
  await on.api.stageTrackerToggleWakeLock();
  assert.deepStrictEqual(on.calls, ['wakeLock:screen', 'release']);
  assert.strictEqual(root.kids.wakelock.attrs['data-state'], 'off');
  await on.api.stageTrackerWakeLockOnVisible();
  assert.deepStrictEqual(on.calls, ['wakeLock:screen', 'release'], 'turned off: visibility never re-requests');

  const back = loadControls();
  back.api.stageTrackerDisplayControlsInit();
  await back.api.stageTrackerToggleWakeLock();
  // The browser drops the lock when the tab is hidden; coming back re-acquires
  // it because the user turned it on.
  back.calls.length = 0;
  await back.api.stageTrackerWakeLockOnVisible();
  assert.deepStrictEqual(back.calls, [], 'still held: nothing to do');

  const none = loadControls({ wakeLock: false });
  const r2 = none.api.stageTrackerDisplayControlsInit();
  assert.strictEqual(r2.kids.wakelock.disabled, true);
  assert.strictEqual(r2.kids.wakelock.attrs['data-state'], 'unsupported');
  assert.strictEqual(await none.api.stageTrackerToggleWakeLock(), false, 'never throws');
  assert.deepStrictEqual(none.calls, []);

  const no = loadControls({ wakeLockDenied: true });
  const r3 = no.api.stageTrackerDisplayControlsInit();
  assert.strictEqual(await no.api.stageTrackerToggleWakeLock(), false);
  assert.strictEqual(r3.kids.wakelock.attrs['data-state'], 'denied');
  assert.match(r3.kids.wakelock.title, /Permission denied by policy/);
  assert.ok(no.toasts.some(([, m]) => /refused: Permission denied by policy/.test(m)));
  for (const fn of no.docListeners.visibilitychange) await fn({ type: 'visibilitychange' });
  await settle();
  assert.deepStrictEqual(no.calls, ['wakeLock:screen'], 'no retry loop after a denial');
});

test('the wake lock is re-acquired on visibilitychange only when the user had turned it on', async () => {
  const env = loadControls();
  const root = env.api.stageTrackerDisplayControlsInit();
  const visible = () => Promise.all(env.docListeners.visibilitychange.map(fn => fn({ type: 'visibilitychange' })));
  await visible();
  assert.deepStrictEqual(env.calls, [], 'never turned on: nothing requested');
  await env.api.stageTrackerToggleWakeLock();
  // The browser releases the lock while the tab is hidden.
  env.sentinels[0].released = true;
  env.document.visibilityState = 'hidden';
  await visible();
  assert.deepStrictEqual(env.calls, ['wakeLock:screen'], 'not while hidden');
  assert.strictEqual(root.kids.wakelock.attrs['data-state'], 'off');
  env.document.visibilityState = 'visible';
  await visible();
  assert.deepStrictEqual(env.calls, ['wakeLock:screen', 'wakeLock:screen'], 're-acquired on return');
  assert.strictEqual(root.kids.wakelock.attrs['data-state'], 'on');
  await visible();
  assert.strictEqual(env.calls.length, 2, 'already held: no extra request');
});

test('Full screen delegates to the existing toggleStageTrackerFullscreen and renders disabled when unsupported', async () => {
  assert.ok(html.includes('data-display-control="fullscreen" data-action="toggleStageTrackerFullscreen"'), 'reuses the existing toggle');
  const env = loadControls();
  env.api.stageTrackerDisplayControlsInit();
  assert.deepStrictEqual(env.calls, []);
  await env.api.toggleStageTrackerFullscreen();
  assert.deepStrictEqual(env.calls, ['requestFullscreen'], 'only the click requests it');
  for (const opts of [{ fullscreenApi: false }, { fullscreenEnabled: false }]) {
    const un = loadControls(opts);
    const root = un.api.stageTrackerDisplayControlsInit();
    assert.strictEqual(root.kids.fullscreen.disabled, true);
    assert.match(root.kids.fullscreen.title, /not supported/);
    assert.strictEqual(un.api.stageTrackerFullscreenSupported(un.document), false);
  }
  assert.strictEqual(loadControls().api.stageTrackerFullscreenSupported(null), false);
});

test('Copy display link: only display=1, am.* and theme — never a token — and it reproduces the filter in a fresh profile', async () => {
  const env = loadControls({ href: 'https://user:pw@hive.example/dash?display=1&theme=light&am.kind=issue&token=s3cret&access_token=t&foo=bar&kiosk=1#x' });
  env.api.stageTrackerDisplayControlsInit();
  assert.strictEqual(await env.api.stageTrackerCopyDisplayLink(), true);
  assert.strictEqual(env.calls.length, 1);
  const link = env.calls[0].replace(/^clipboard:/, '');
  const u = new URL(link);
  assert.deepStrictEqual(Array.from(new Set(u.searchParams.keys())).sort(), ['am.group', 'am.kind', 'am.labels', 'am.repo', 'display', 'theme']);
  assert.strictEqual(u.searchParams.get('display'), '1');
  assert.strictEqual(u.searchParams.get('theme'), 'light');
  assert.strictEqual(u.searchParams.get('am.kind'), 'pr', 'the live in-memory filter, not the stale URL');
  assert.strictEqual(u.username + u.password, '');
  assert.ok(!/s3cret|token|foo|kiosk|pw/.test(link), link);
  assert.strictEqual(u.pathname, '/dash');
  assert.strictEqual(u.hash, '#section-activity-monitor-section');
  const back = env.api.stageTrackerFilterFromUrl(link);
  const f = env.api.filter();
  for (const k of ['kind', 'repo', 'labels', 'q', 'age', 'author', 'acted', 'sort', 'dir', 'needsHuman', 'onHold', 'attention']) assert.deepStrictEqual(back.filter[k], f[k], k);
  assert.strictEqual(back.group, 'repo');
  assert.ok(env.toasts.some(([t]) => t === 'success'));
});

test('Copy display link falls back to a selectable field when the clipboard is missing or refused, and a playlist link keeps its views', async () => {
  for (const opts of [{ clipboard: false }, { clipboardRefused: true }]) {
    const env = loadControls(opts);
    env.api.stageTrackerDisplayControlsInit();
    assert.strictEqual(await env.api.stageTrackerCopyDisplayLink(), false);
    const field = env.byId['activity-monitor-display-link'];
    assert.strictEqual(field.hidden, false);
    assert.ok(field.value.includes('display=1') && field.value.includes('am.kind=pr'));
    assert.ok(field.selected, 'selected for a manual copy');
    assert.ok(env.toasts.some(([t, m]) => t === 'error' && /clipboard/.test(m)));
    assert.strictEqual(env.byId['activity-monitor-display-controls'].classList.contains('is-visible'), true, 'kept visible while the link shows');
  }
  const plHref = 'https://hive.example/?display=1&am.view=am.kind%3Dpr&am.view=am.kind%3Dissue&interval=30s&am.kind=pr&token=x';
  const pl = loadControls({ href: plHref });
  const link = pl.api.stageTrackerDisplayLinkUrl(plHref, pl.api.filter(), 'none', pl.api.stageTrackerParsePlaylist(plHref));
  const u = new URL(link);
  assert.deepStrictEqual(u.searchParams.getAll('am.view'), ['am.kind=pr', 'am.kind=issue']);
  assert.strictEqual(u.searchParams.get('interval'), '30s');
  assert.strictEqual(u.searchParams.get('token'), null);
  assert.strictEqual(u.searchParams.get('am.kind'), null, 'the playlist link carries the views, not the current view');
  // Views are re-serialised from the parsed playlist: a token nested inside
  // an am.view value, an unknown key and a rejected view never ride along,
  // and a whole-minute interval comes back as Nm.
  const dirty = 'https://hive.example/?display=1&am.view=' + encodeURIComponent('am.kind=pr&token=s3cret&junk=1&am.group=repo') + '&am.view=' + encodeURIComponent('token=only') + '&am.view=' + encodeURIComponent('am.kind=issue&am.view=x') + '&am.view=' + encodeURIComponent('am.repo=hivecommons/hive') + '&interval=2m&token=x';
  const clean = new URL(pl.api.stageTrackerDisplayLinkUrl(dirty, pl.api.filter(), 'none', pl.api.stageTrackerParsePlaylist(dirty)));
  assert.deepStrictEqual(clean.searchParams.getAll('am.view'), ['am.kind=pr&am.group=repo', 'am.repo=hivecommons%2Fhive']);
  assert.ok(!/token|junk/.test(clean.toString()), clean.toString());
  assert.strictEqual(clean.searchParams.get('interval'), '2m');
  assert.deepStrictEqual(pl.api.stageTrackerParsePlaylist(clean.toString()).views.map(x => [x.filter.kind, x.filter.repo, x.group]), [['pr', '', 'repo'], ['all', 'hivecommons/hive', 'none']]);
  // An all-default view still names am.group so it survives a reload.
  const dflt = 'https://hive.example/?display=1&am.view=' + encodeURIComponent('am.kind=pr') + '&am.view=' + encodeURIComponent('am.group=none');
  const dl = new URL(pl.api.stageTrackerDisplayLinkUrl(dflt, pl.api.filter(), 'none', pl.api.stageTrackerParsePlaylist(dflt)));
  assert.strictEqual(pl.api.stageTrackerParsePlaylist(dl.toString()).views.length, 2);
  // With no parsed playlist the current view is used, whatever am.view said.
  assert.deepStrictEqual(new URL(pl.api.stageTrackerDisplayLinkUrl(plHref, pl.api.filter(), 'none', null)).searchParams.getAll('am.view'), []);
  assert.strictEqual(pl.api.stageTrackerDisplayLinkUrl('nope', {}, 'none', null), '');
});

test('Exit display drops display=1 like Esc and releases a held wake lock', async () => {
  const env = loadControls();
  env.api.stageTrackerDisplayControlsInit();
  await env.api.stageTrackerToggleWakeLock();
  env.api.stageTrackerDisplayExit();
  await settle();
  const nav = env.calls.find(c => c.startsWith('assign:'));
  assert.ok(nav && !nav.includes('display=') && nav.includes('am.kind=pr'), nav);
  assert.ok(env.calls.includes('release'));
  assert.ok(html.includes('data-display-control="exit" data-action="stageTrackerDisplayExit" aria-label="Exit display"'));
  for (const label of ['Copy display link', 'Full screen', 'Keep screen awake']) assert.ok(html.includes('aria-label="' + label + '"'), label);
});

test('display controls persist nothing', () => {
  for (const fn of ['stageTrackerDisplayControlsInit', 'stageTrackerToggleWakeLock', 'stageTrackerWakeLockAcquire', 'stageTrackerWakeLockRelease', 'stageTrackerCopyDisplayLink', 'stageTrackerDisplayExit', 'stageTrackerRenderDisplayControls']) {
    assert.ok(!/localStorage|sessionStorage|document\.cookie/.test(fnSrc(fn)), fn);
  }
});
