'use strict';

// Keyboard-focus regressions for the Commons web page script
// (hives_web_tree.js). The real script runs in a vm against a small fake DOM,
// the same approach as src/pkg/dashboard/*.test.js, so no browser is needed.
//
// The one browser rule the fake must honour is the one that caused the bug:
// a control inside a display:none ancestor cannot take focus. Secondary row
// actions live in `.more`, which the page CSS shows only inside
// `.tree-row.active`. renderTree rebuilds every row, so after Pause / Deselect
// all / Exclude it must activate the restored control's row *before* focusing
// it. Otherwise focus() silently fails, focus falls to <body> and the first
// row becomes active.

const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

// COMMONS_TREE_JS lets a developer run the suite against another revision of
// the script to confirm a test fails without the fix.
const SCRIPT = process.env.COMMONS_TREE_JS || path.join(__dirname, 'hives_web_tree.js');

// ---- selector engine: only what the script uses -------------------------

function parseCompound(w) {
  const c = { tag: null, classes: [], attrs: [], scope: false };
  let rest = w;
  const t = rest.match(/^[a-zA-Z][a-zA-Z0-9-]*/);
  if (t) { c.tag = t[0].toLowerCase(); rest = rest.slice(t[0].length); }
  while (rest) {
    let m;
    if ((m = rest.match(/^:scope/))) c.scope = true;
    else if ((m = rest.match(/^\.([a-zA-Z0-9_-]+)/))) c.classes.push(m[1]);
    else if ((m = rest.match(/^\[([a-zA-Z0-9_-]+)(?:=([^\]]+))?\]/))) c.attrs.push([m[1], m[2]]);
    else throw new Error('fake DOM: unsupported selector part ' + rest);
    rest = rest.slice(m[0].length);
  }
  return c;
}

function parseSelector(sel) {
  return sel.split(',').map(part => {
    const toks = [];
    let comb = ' ';
    for (const w of part.trim().match(/>|[^\s>]+/g)) {
      if (w === '>') { comb = '>'; continue; }
      toks.push({ c: parseCompound(w), comb });
      comb = ' ';
    }
    return toks;
  });
}

function matchCompound(el, c, scope) {
  if (c.scope) return el === scope;
  if (c.tag && el.tag !== c.tag) return false;
  if (!c.classes.every(k => el.hasClass(k))) return false;
  return c.attrs.every(([name, val]) => {
    if (name === 'hidden') return !!el.hidden;
    return val === undefined ? el.attrs.has(name) : el.attrs.get(name) === val;
  });
}

function matchFrom(el, toks, i, scope) {
  if (!matchCompound(el, toks[i].c, scope)) return false;
  if (i === 0) return true;
  if (toks[i].comb === '>') return el.parent ? matchFrom(el.parent, toks, i - 1, scope) : false;
  for (let p = el.parent; p; p = p.parent) if (matchFrom(p, toks, i - 1, scope)) return true;
  return false;
}

// ---- fake DOM ------------------------------------------------------------

class El {
  constructor(doc, tag) {
    this.doc = doc; this.tag = tag.toLowerCase();
    this.attrs = new Map(); this.children = []; this.parent = null; this.own = '';
    this.listeners = {}; this.hidden = false; this.checked = false; this.indeterminate = false; this.disabled = false;
    const key = k => 'data-' + k.replace(/[A-Z]/g, m => '-' + m.toLowerCase());
    this.dataset = new Proxy({}, { get: (_, k) => this.attrs.get(key(k)), set: (_, k, v) => { this.attrs.set(key(k), String(v)); return true; } });
  }
  get tagName() { return this.tag.toUpperCase(); }
  get type() { return this.attrs.get('type'); }
  set type(v) { this.attrs.set('type', v); }
  get tabIndex() { return Number(this.attrs.get('tabindex') ?? -1); }
  set tabIndex(v) { this.attrs.set('tabindex', String(v)); }
  get className() { return this.attrs.get('class') || ''; }
  set className(v) { this.attrs.set('class', v); }
  hasClass(k) { return this.className.split(/\s+/).includes(k); }
  get classList() {
    const set = fn => { const s = new Set(this.className.split(/\s+/).filter(Boolean)); fn(s); this.className = [...s].join(' '); };
    return { add: k => set(s => s.add(k)), remove: k => set(s => s.delete(k)), contains: k => this.hasClass(k) };
  }
  get textContent() { return this.own + this.children.map(c => c.textContent).join(''); }
  set textContent(v) {
    for (const c of this.children) c.parent = null;
    this.children = []; this.own = String(v);
  }
  get parentElement() { return this.parent; }
  get childNodes() { return this.children; }
  setAttribute(n, v) { this.attrs.set(n, String(v)); }
  getAttribute(n) { return this.attrs.has(n) ? this.attrs.get(n) : null; }
  hasAttribute(n) { return this.attrs.has(n); }
  appendChild(c) { c.remove(); c.parent = this; this.children.push(c); return c; }
  append(...xs) { for (const x of xs) typeof x === 'string' ? (this.own += x) : this.appendChild(x); }
  remove() { if (this.parent) { this.parent.children = this.parent.children.filter(c => c !== this); this.parent = null; } }
  contains(el) { for (let e = el; e; e = e.parent) if (e === this) return true; return false; }
  matches(sel, scope = null) { return parseSelector(sel).some(toks => matchFrom(this, toks, toks.length - 1, scope)); }
  closest(sel) { for (let e = this; e; e = e.parent) if (e.matches(sel)) return e; return null; }
  querySelectorAll(sel) {
    const out = [];
    const walk = n => { for (const c of n.children) { if (c.matches(sel, this)) out.push(c); walk(c); } };
    walk(this);
    return out;
  }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  // Mirrors the page CSS: `.more` is display:none unless its row is `.active`.
  focusable() {
    if (this.disabled) return false;
    let top = this;
    for (let e = this; e; e = e.parent) {
      if (e.hidden) return false;
      if (e.hasClass('more') && !(e.parent && e.parent.hasClass('active'))) return false;
      top = e;
    }
    return top === this.doc.root;
  }
  focus() {
    if (!this.focusable()) return;
    this.doc.activeElement = this;
    this.dispatch('focusin');
  }
  blur() { this.doc.activeElement = this.doc.body; }
  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
  dispatch(type, init = {}) {
    const ev = { type, target: this, ctrlKey: false, metaKey: false, altKey: false, defaultPrevented: false, ...init,
      preventDefault() { this.defaultPrevented = true; }, stopPropagation() { this.stopped = true; } };
    for (let n = this; n && !ev.stopped; n = n.parent) {
      if (typeof n['on' + type] === 'function') n['on' + type].call(n, ev);
      for (const fn of n.listeners[type] || []) fn.call(n, ev);
    }
    return ev;
  }
  click() {
    if (this.disabled) return;
    if (this.type === 'checkbox') { this.checked = !this.checked; this.indeterminate = false; this.dispatch('change'); return; }
    this.dispatch('click');
  }
}

class Doc {
  constructor() {
    this.root = new El(this, 'html'); this.body = new El(this, 'body'); this.root.appendChild(this.body);
    this.activeElement = this.body; this.ids = new Map(); this.listeners = {};
  }
  createElement(tag) { return new El(this, tag); }
  getElementById(id) { return this.ids.get(id) || null; }
  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
  add(tag, id) { const e = this.createElement(tag); e.attrs.set('id', id); this.ids.set(id, e); this.body.appendChild(e); return e; }
}

// ---- fake server: just enough of /api/tree for secondary actions ----------

function newState() {
  const repos = org => ['agent', 'api', 'web'].map((n, i) => ({ id: `github/github.com/${org}-org/${n}`, selected: i === 1, unreviewed: i !== 1, offered: 'yes', excluded_by: [] }));
  const sub = id => ({ id, hive_id: 'http://' + id + '.example', disabled: false, shared_across_hubs: 1, follow_new_repositories: false, repositories: repos(id) });
  const state = {
    session: 's1', revision: 'r1', version: 1,
    tree: { hubs: [{ id: 'direct', name: 'Direct', direct: true, follow: 'off', subscriptions: ['alpha', 'beta'] }], subscriptions: [sub('alpha'), sub('beta')] },
    diff: { stale: false, selected_count: 0, deselected_count: 0, excluded_count: 0, cleared_exclusion_count: 0, membership: [], enabled: [], hub_follow: [], follow: [], legacy_conversions: [], exclusions: [], shared_branches: [], selected: [], deselected: [] },
  };
  recount(state);
  return state;
}

function recount(state) {
  const sel = (repos, extra = {}) => {
    const n = repos.filter(r => r.selected).length;
    return { state: n === 0 ? 'off' : n === repos.length ? 'all-current' : 'partial', repositories: repos.length, selected: n,
      excluded: repos.filter(r => r.excluded_by.length).length, unreviewed: repos.filter(r => r.unreviewed && !r.selected).length,
      membership_incomplete: false, legacy_unrestricted: false, ...extra };
  };
  for (const s of state.tree.subscriptions) s.selection = sel(s.repositories);
  const all = state.tree.subscriptions.flatMap(s => s.repositories);
  state.tree.hubs[0].selection = sel(all);
}

function makeFetch(state) {
  const json = data => ({ ok: true, status: 200, headers: { get: () => 'application/json' }, json: async () => data, text: async () => JSON.stringify(data) });
  return async (url, opts = {}) => {
    if (!opts.method) return json(JSON.parse(JSON.stringify(state)));
    if (url !== '/api/tree/diff') throw new Error('fake server: unexpected ' + url);
    const body = JSON.parse(opts.body);
    const sub = state.tree.subscriptions.find(s => s.id === body.target);
    if (body.operation === 'enabled') {
      sub.disabled = !body.value;
      state.diff.enabled = [{ target: sub.id, before: !sub.disabled, after: !!body.value }];
    } else if (body.operation === 'current') {
      sub.repositories.forEach(r => { r.selected = !!body.value; });
      state.diff.membership = [{ subscription: sub.id }];
    } else if (body.operation === 'exclude') {
      sub.repositories.find(r => r.id === body.repository).excluded_by = body.value ? [sub.id] : [];
      state.diff.membership = [{ subscription: sub.id }];
    } else {
      throw new Error('fake server: unsupported operation ' + body.operation);
    }
    recount(state);
    return json(JSON.parse(JSON.stringify(state)));
  };
}

// ---- harness --------------------------------------------------------------

const settle = async () => { for (let i = 0; i < 4; i++) await new Promise(r => setImmediate(r)); };

async function boot() {
  const doc = new Doc();
  const ul = doc.add('ul', 'contribution-tree');
  ul.setAttribute('role', 'tree');
  for (const id of ['diff-summary', 'tree-diff', 'tree-diff-raw', 'tree-info', 'tree-receipt', 'tree-apply', 'tree-discard', 'tree-refresh', 'tree-leave',
    'tree-collapse-all', 'tree-expand-all', 'tree-select-matching', 'skip-apply', 'skip-profiles', 'manage', 'profiles-title']) doc.add('div', id);
  doc.add('input', 'tree-search').value = '';
  const state = newState();
  const said = [];
  const ctx = vm.createContext({
    document: doc, window: { addEventListener() {} }, history: { pushState() {}, go() {} }, location: { pathname: '/', assign() {} },
    headers: {}, fetch: makeFetch(state), say: (text, cls) => said.push([text, cls]), load: async () => {},
    styledConfirm: async () => true, console, setTimeout, clearTimeout,
  });
  vm.runInContext(fs.readFileSync(SCRIPT, 'utf8'), ctx, { filename: SCRIPT });
  await vm.runInContext('loadTree()', ctx);
  const rows = () => ul.querySelectorAll('[role=treeitem]');
  const row = label => rows().find(r => r.getAttribute('aria-label') === label);
  const control = (r, label) => r.querySelectorAll(':scope > .tree-row button').find(b => b.getAttribute('aria-label') === label);
  const label = () => (doc.activeElement.closest('[role=treeitem]') || doc.body).getAttribute('aria-label');
  const activeRows = () => ul.querySelectorAll('.tree-row.active').map(r => r.parent.getAttribute('aria-label'));
  // Key presses go through the page's own keydown handler; unhandled Enter/Space on a button is the browser's native click.
  const press = key => {
    const t = doc.activeElement;
    const ev = t.dispatch('keydown', { key });
    if (!ev.defaultPrevented && (key === 'Enter' || key === ' ') && t.tag === 'button') t.click();
  };
  const walkTo = target => {
    rows()[0].focus();
    for (let i = 0; i < 40 && label() !== target; i++) press('ArrowDown');
    assert.equal(label(), target, 'arrow keys should reach ' + target);
  };
  return { doc, ul, state, said, rows, row, control, label, activeRows, press, walkTo };
}

// ---- tests ----------------------------------------------------------------

test('fake DOM enforces the rule under test: a non-active row\'s secondary control cannot take focus', async () => {
  const t = await boot();
  t.rows()[0].focus();
  const alphaPause = t.control(t.row('alpha'), 'Pause: alpha');
  assert.ok(alphaPause, 'alpha Pause button should be rendered');
  alphaPause.focus();
  assert.notEqual(t.doc.activeElement, alphaPause, 'a control in a non-active row is display:none and must not accept focus');
});

test('Pause on a non-first row keeps focus on that control and that row active', async () => {
  const t = await boot();
  t.walkTo('beta');
  const pause = t.control(t.row('beta'), 'Pause: beta');
  const key = pause.dataset.focusKey;
  pause.focus();
  assert.equal(t.doc.activeElement, pause);

  t.press('Enter');
  await settle();

  const now = t.doc.activeElement;
  assert.equal(now.getAttribute('aria-label'), 'Resume: beta', 'focus should stay on the same control, now relabelled');
  assert.equal(now.dataset.focusKey, key);
  assert.deepEqual(t.activeRows(), ['beta'], 'only the acted-on row is active, not the first row');
  assert.equal(t.state.tree.subscriptions[1].disabled, true, 'the action itself must still happen');
});

test('Deselect all on a non-first row keeps focus on that control', async () => {
  const t = await boot();
  t.walkTo('beta');
  const btn = t.control(t.row('beta'), 'Deselect all: beta');
  btn.focus();
  assert.equal(t.doc.activeElement, btn);

  t.press('Enter');
  await settle();

  assert.equal(t.doc.activeElement.getAttribute('aria-label'), 'Deselect all: beta');
  assert.deepEqual(t.activeRows(), ['beta']);
});

test('Exclude on a repository row far from the first row keeps focus there, and arrow keys continue from it', async () => {
  const t = await boot();
  t.walkTo('beta-org/agent');
  const id = 'github/github.com/beta-org/agent';
  const exclude = t.control(t.row('beta-org/agent'), `Exclude: ${id} in beta`);
  exclude.focus();
  assert.equal(t.doc.activeElement, exclude);

  t.press('Enter');
  await settle();

  assert.equal(t.doc.activeElement.getAttribute('aria-label'), `Allow again: ${id} in beta`);
  assert.deepEqual(t.activeRows(), ['beta-org/agent']);

  t.press('ArrowDown');
  assert.equal(t.label(), 'beta-org/api', 'navigation continues from the repository, not from the top of the list');
});
