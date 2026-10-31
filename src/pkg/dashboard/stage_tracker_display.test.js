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
  assert.ok(html.includes('if (stageTrackerDisplayOverlayOpen(document)) return;\n      e.preventDefault();\n      toggleStageTrackerDisplay();'), 'the Esc listener checks overlays before leaving');
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
