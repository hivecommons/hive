package dashboard

import (
	"strings"
	"testing"
)

// TestStageTrackerHumanGateMenuReachableWiring pins hivecommons/hive#19655:
// at the stacked-head breakpoint the gate keeps one verb inline and folds
// the rest ("Request changes…", "Let hive decide") behind "…". That menu
// used to be unhidden in place, a position: fixed box with no offsets
// inside the pill's overflow: hidden, so it fell to its static spot shifted
// by the page scroll and opened off-screen: a phone could not reach "Let
// hive decide". On the phone tier it now opens as the row menu's sheet;
// elsewhere it is anchored to its button and clamped to the viewport.
func TestStageTrackerHumanGateMenuReachableWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`const STAGE_TRACKER_GATE_SHEET_TITLE = 'More ways to settle';`,
		`function stageTrackerHumanGateSheetOn()`,
		`function stageTrackerHumanGateSheetItem(b)`,
		`function stageTrackerOpenHumanGateSheet(b, menu)`,
		`function stageTrackerPlaceHumanGateMenu(b, menu)`,
		`if (stageTrackerHumanGateSheetOn() && stageTrackerOpenHumanGateSheet(b, menu)) return;`,
		`stageTrackerPlaceHumanGateMenu(b, menu);`,
		`window.addEventListener('scroll', closeAll, true);`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing human-gate menu reachability wiring %q", want)
		}
	}
}

// stageTrackerGateMenuStubJS is a tiny DOM for the gate's … wiring: enough
// selector matching (.class, [attr], [attr="v"], one :not(…)), tree walking
// and cloning for stageTrackerWireHumanGate and the sheet to run under node.
const stageTrackerGateMenuStubJS = `
const assert = require('assert');
function hamMatch(el, sel) {
  if (!el || !el.attrs) return false;
  const not = /:not\((.*)\)$/.exec(sel);
  if (not) { sel = sel.slice(0, not.index); if (hamMatch(el, not[1])) return false; }
  const parts = sel.match(/\.[\w-]+|\[[^\]]+\]/g) || [];
  return parts.every(p => {
    if (p[0] === '.') return el.classList.contains(p.slice(1));
    const m = /^\[([\w-]+)(?:="([^"]*)")?\]$/.exec(p);
    if (m[1] === 'hidden') return !!el.hidden;
    if (!(m[1] in el.attrs)) return false;
    return m[2] === undefined || el.attrs[m[1]] === m[2];
  });
}
class HamEl {
  constructor(tag, cls, attrs) {
    this.tagName = String(tag).toUpperCase(); this.attrs = {}; this.children = []; this.parentNode = null; this.hidden = false; this._text = '';
    this.style = { props: {}, setProperty(k, v) { this.props[k] = v; } };
    this.listeners = {}; this.offsetWidth = 0; this.offsetHeight = 0; this.rect = null; this.focused = false;
    const self = this;
    this.classList = {
      add(...c) { c.forEach(x => { if (!self.classes.includes(x)) self.classes.push(x); }); },
      remove(...c) { self.classes = self.classes.filter(x => !c.includes(x)); },
      contains(c) { return self.classes.includes(c); },
    };
    this.classes = String(cls || '').split(/\s+/).filter(Boolean);
    Object.entries(attrs || {}).forEach(([k, v]) => this.setAttribute(k, v));
  }
  get className() { return this.classes.join(' '); }
  set className(v) { this.classes = String(v).split(/\s+/).filter(Boolean); }
  get dataset() { const d = {}; Object.keys(this.attrs).filter(k => k.startsWith('data-')).forEach(k => { d[k.slice(5).replace(/-(\w)/g, (m, c) => c.toUpperCase())] = this.attrs[k]; }); return d; }
  get textContent() { return this._text + this.children.map(c => c.textContent).join(''); }
  set textContent(v) { this._text = String(v); this.children = []; }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  removeAttribute(k) { delete this.attrs[k]; }
  appendChild(c) { c.parentNode = this; this.children.push(c); return c; }
  get nextElementSibling() { const s = this.parentNode ? this.parentNode.children : []; return s[s.indexOf(this) + 1] || null; }
  get previousElementSibling() { const s = this.parentNode ? this.parentNode.children : []; return s[s.indexOf(this) - 1] || null; }
  closest(sel) { for (let n = this; n; n = n.parentNode) if (sel.split(/,\s*/).some(s => hamMatch(n, s))) return n; return null; }
  contains(n) { for (; n; n = n.parentNode) if (n === this) return true; return false; }
  querySelectorAll(sel) { const out = []; const walk = n => n.children.forEach(c => { if (sel.split(/,\s*/).some(s => hamMatch(c, s))) out.push(c); walk(c); }); walk(this); return out; }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  cloneNode() { const c = new HamEl(this.tagName, this.className, this.attrs); c.hidden = this.hidden; c._text = this._text; this.children.forEach(k => c.appendChild(k.cloneNode(true))); return c; }
  addEventListener(type, fn) { (this.listeners[type] = this.listeners[type] || []).push(fn); }
  getBoundingClientRect() { return this.rect || { left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0 }; }
  focus() { this.focused = true; }
}
function hamFire(root, target, type) {
  const e = { type, target, defaultPrevented: false, preventDefault() { this.defaultPrevented = true; }, stopPropagation() {} };
  for (let n = target; n; n = n.parentNode) (n.listeners[type] || []).forEach(fn => fn(e));
  return e;
}
global.document = { createElement: tag => new HamEl(tag), contains: () => true, activeElement: null };
global.window = { innerWidth: 390, innerHeight: 844, addEventListener() {} };
global.esc = s => String(s);
// One PR row parked on a review verdict, as rendered at ≤ 1100px:
// Approve inline, Request changes… and Let hive decide folded behind ….
function hamGateRow() {
  const panel = new HamEl('div');
  const row = panel.appendChild(new HamEl('div', 'ham-row', { 'data-ham-key': 'hivecommons/hive/pr#19655' }));
  const gate = row.appendChild(new HamEl('span', 'ham-human-gate'));
  gate.appendChild(new HamEl('button', 'ham-lead-act ham-gate-act', { 'data-ham-act': 'decide:approve' }));
  const wrap = gate.appendChild(new HamEl('span', 'ham-gate-more-wrap'));
  const more = wrap.appendChild(new HamEl('button', 'ham-lead-act ham-gate-act ham-gate-more', { 'data-ham-gate-more': '', 'aria-expanded': 'false' }));
  more.rect = { left: 346, top: 410, right: 390, bottom: 424, width: 44, height: 14 };
  const menu = wrap.appendChild(new HamEl('div', 'ham-menu ham-gate-menu', { role: 'menu' }));
  menu.hidden = true; menu.offsetWidth = 172; menu.offsetHeight = 42;
  const rc = menu.appendChild(new HamEl('button', 'ham-menu-item ham-lead-act ham-gate-act', { role: 'menuitem', 'data-ham-act': 'gate:request-changes', 'aria-disabled': 'true', title: 'Sign in with write access to settle this gate.', tabindex: '-1' }));
  rc._text = 'Request changes…';
  const lhd = menu.appendChild(new HamEl('button', 'ham-menu-item ham-lead-act ham-gate-act', { role: 'menuitem', 'data-ham-act': 'decide:delegate', title: 'The hive decides for you under its policy and rails; recorded as your decision.', tabindex: '-1' }));
  lhd._text = 'Let hive decide';
  return { panel, row, gate, more, menu };
}
`

// TestStageTrackerHumanGateMenuReachable runs the … wiring under node
// (hivecommons/hive#19655). Phone tier: the folded verbs open as a sheet
// whose items are plain menu items carrying their hover text as a visible
// hint, a tap on "Let hive decide" runs decide:delegate on the row and
// closes the sheet, and a disabled verb runs nothing. Wider tiers: the
// popover is placed under its button and clamped inside the viewport.
func TestStageTrackerHumanGateMenuReachable(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerGateMenuStubJS+"\n"+strings.Join([]string{
		jsConstLine(t, html, "STAGE_TRACKER_MENU_MARGIN"),
		jsConstLine(t, html, "STAGE_TRACKER_GATE_SHEET_TITLE"),
		jsFunc(t, html, "stageTrackerMenuPosition"),
		jsFunc(t, html, "stageTrackerHumanGateSheetOn"),
		jsFunc(t, html, "stageTrackerHumanGateSheetItem"),
		jsFunc(t, html, "stageTrackerOpenHumanGateSheet"),
		jsFunc(t, html, "stageTrackerPlaceHumanGateMenu"),
		jsFunc(t, html, "stageTrackerWireHumanGate"),
	}, "\n")+`
const item = { repo: 'hivecommons/hive', kind: 'pr', number: 19655 };
var _stageTracker = { items: [item] };
global.stageTrackerItemByKey = (d, key) => key === 'hivecommons/hive/pr#19655' ? item : null;
global.stageTrackerRefOf = it => it.repo + '#' + it.number;
const runs = [];
global.stageTrackerDecisionRun = (it, act) => { runs.push([it, act]); return null; };
global.stageTrackerHumanGateSettling = () => {};
let phone = true;
global.stageTrackerIsPhoneSheet = () => phone;
let sheet = null;
global.stageTrackerOpenSheet = (title, body, cls, onClose, onEnter) => {
  const host = { title, body, cls, closed: false };
  host.hiveDialogClose = () => { if (host.closed) return; host.closed = true; onClose(); };
  sheet = host;
  return host;
};

// Phone: … opens the sheet, never the in-row popover.
let g = hamGateRow();
stageTrackerWireHumanGate(g.panel);
hamFire(g.panel, g.more, 'click');
assert.ok(sheet, 'the … opens a sheet on a phone');
assert.strictEqual(sheet.title, 'More ways to settle hivecommons/hive#19655');
assert.ok(/ham-menu-dialog/.test(sheet.cls), 'the sheet is the row menu dialog: ' + sheet.cls);
assert.ok(sheet.body.classList.contains('ham-menu-sheet') && !sheet.body.hidden, 'the sheet body is the tap-sized menu');
assert.ok(g.menu.hidden, 'the off-screen in-row popover stays hidden');
assert.strictEqual(g.more.getAttribute('aria-expanded'), 'true');
const items = sheet.body.querySelectorAll('[data-ham-act]');
assert.deepStrictEqual(items.map(i => i.dataset.hamAct), ['gate:request-changes', 'decide:delegate'], 'every folded verb, Let hive decide included, is in the sheet');
items.forEach(i => {
  assert.ok(!i.classList.contains('ham-lead-act') && !i.classList.contains('ham-gate-act'), 'a sheet item is a menu item, not an inline pill: ' + i.className);
  assert.ok(i.classList.contains('ham-menu-item-hinted'), 'a sheet item writes its hover text out');
  assert.strictEqual(i.getAttribute('tabindex'), null, 'sheet items are in the tab order');
});
assert.ok(items[0].textContent.includes('Sign in with write access'), 'a greyed verb says why on a phone: ' + items[0].textContent);
assert.ok(items[1].textContent.includes('The hive decides for you'), 'Let hive decide says what it does: ' + items[1].textContent);

// A disabled verb runs nothing and leaves the sheet open.
hamFire(sheet.body, items[0], 'click');
assert.strictEqual(runs.length, 0, 'a disabled verb never runs');
assert.ok(!sheet.closed, 'the sheet stays open on a disabled verb');
// Let hive decide runs on the row the … sits on and closes the sheet.
hamFire(sheet.body, items[1], 'click');
assert.deepStrictEqual(runs, [[item, 'decide:delegate']], 'Let hive decide runs decide:delegate on the row');
assert.ok(sheet.closed, 'picking a verb closes the sheet');
assert.strictEqual(g.more.getAttribute('aria-expanded'), 'false', 'the … reads collapsed again');

// Wider tiers: the popover opens in place, anchored under its button and
// clamped inside the viewport (the button sits at the right edge here).
phone = false; sheet = null;
g = hamGateRow();
stageTrackerWireHumanGate(g.panel);
hamFire(g.panel, g.more, 'click');
assert.strictEqual(sheet, null, 'no sheet off the phone tier');
assert.ok(!g.menu.hidden, 'the popover opens');
const left = parseFloat(g.menu.style.props.left), top = parseFloat(g.menu.style.props.top);
assert.ok(left >= STAGE_TRACKER_MENU_MARGIN && left + g.menu.offsetWidth <= window.innerWidth - STAGE_TRACKER_MENU_MARGIN, 'the popover fits the viewport horizontally: left=' + left);
assert.strictEqual(top, 424, 'the popover sits under its button');
// Near the bottom of the viewport it flips above the button.
g.more.rect = { left: 10, top: 830, right: 54, bottom: 844, width: 44, height: 14 };
hamFire(g.panel, g.more, 'click');
assert.ok(g.menu.hidden, 'a second tap closes it');
hamFire(g.panel, g.more, 'click');
assert.ok(parseFloat(g.menu.style.props.top) + g.menu.offsetHeight <= window.innerHeight, 'the popover never opens below the fold: top=' + g.menu.style.props.top);
`)
}
