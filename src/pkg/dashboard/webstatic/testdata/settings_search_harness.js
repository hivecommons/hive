// Node harness for the Settings dialog search (webstatic/settings_search_test.go).
//
// It loads the dashboard's inline script from index.html and runs the real
// Settings renderers against a small DOM shim. Top-level functions and
// constants are pulled out of the script lazily — a `with (scope)` Proxy
// resolves any free identifier the first time it is used by extracting its
// declaration from the source — so the test exercises the real
// renderGovernorTab / search / reveal code without listing every helper by
// hand. Anything that cannot be found (browser APIs, bootstrap state) becomes
// an inert stub and is recorded in `missing` for diagnostics.
//
// Usage: node settings_search_harness.js <index.html> <scenario>
'use strict';
const fs = require('node:fs');

const [,, indexPath, scenario] = process.argv;
const html = fs.readFileSync(indexPath, 'utf8');

// ---------------------------------------------------------------- DOM shim --
const VOID = new Set(['br', 'hr', 'img', 'input', 'meta', 'link', 'path', 'circle', 'rect', 'line', 'polyline', 'use', 'source', 'wbr', 'col', 'area', 'base']);
const scrollCalls = [];
const focusCalls = [];

function decode(s) {
  return String(s).replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&amp;/g, '&');
}

class ClassList {
  constructor(el) { this.el = el; }
  _set() { return new Set((this.el.attrs.get('class') || '').split(/\s+/).filter(Boolean)); }
  _write(set) { this.el.attrs.set('class', [...set].join(' ')); }
  contains(c) { return this._set().has(c); }
  add(...cs) { const s = this._set(); cs.forEach(c => s.add(c)); this._write(s); }
  remove(...cs) { const s = this._set(); cs.forEach(c => s.delete(c)); this._write(s); }
  toggle(c, force) { const s = this._set(); const on = force === undefined ? !s.has(c) : !!force; if (on) s.add(c); else s.delete(c); this._write(s); return on; }
}

class Node {
  constructor() { this.parentNode = null; this.childNodes = []; }
  get parentElement() { return this.parentNode instanceof Element ? this.parentNode : null; }
}

class Text extends Node {
  constructor(text) { super(); this.nodeType = 3; this.data = text; }
  get textContent() { return this.data; }
}

class Element extends Node {
  constructor(tag) {
    super();
    this.nodeType = 1;
    this.tagName = tag.toUpperCase();
    this.localName = tag.toLowerCase();
    this.attrs = new Map();
    this.listeners = {};
    this.classList = new ClassList(this);
    this.style = {};
    this.value = '';
    this.open = false;
    this.hidden = false;
    this.scrollTop = 0;
    this.clientHeight = 400;
    const el = this;
    const attrName = k => 'data-' + k.replace(/[A-Z]/g, m => '-' + m.toLowerCase());
    this.dataset = new Proxy({}, {
      get(_, k) { return typeof k === 'string' ? el.attrs.get(attrName(k)) : undefined; },
      set(_, k, v) { el.attrs.set(attrName(k), String(v)); return true; },
      has(_, k) { return el.attrs.has(attrName(k)); },
    });
  }
  get children() { return this.childNodes.filter(n => n instanceof Element); }
  get firstElementChild() { return this.children[0] || null; }
  get id() { return this.attrs.get('id') || ''; }
  set id(v) { this.attrs.set('id', String(v)); }
  get className() { return this.attrs.get('class') || ''; }
  set className(v) { this.attrs.set('class', String(v)); }
  getAttribute(k) { return this.attrs.has(k) ? this.attrs.get(k) : null; }
  setAttribute(k, v) { this.attrs.set(k, String(v)); if (k === 'open') this.open = true; }
  hasAttribute(k) { return this.attrs.has(k); }
  removeAttribute(k) { this.attrs.delete(k); if (k === 'open') this.open = false; }
  appendChild(c) { if (c.parentNode) c.remove(); c.parentNode = this; this.childNodes.push(c); return c; }
  remove() { if (this.parentNode) { const p = this.parentNode; p.childNodes = p.childNodes.filter(x => x !== this); this.parentNode = null; } }
  contains(n) { while (n) { if (n === this) return true; n = n.parentNode; } return false; }
  get textContent() { return this.childNodes.map(n => n.textContent).join(''); }
  set textContent(v) { this.childNodes = []; if (v !== '') this.appendChild(new Text(String(v))); }
  get innerText() { return this.textContent; }
  set innerHTML(v) { this.childNodes = []; parseInto(String(v), this); }
  get innerHTML() { return serialize(this); }
  insertAdjacentHTML(where, v) { const tmp = new Element('div'); tmp.innerHTML = v; tmp.childNodes.slice().forEach(c => this.appendChild(c)); }
  addEventListener(t, fn) { (this.listeners[t] || (this.listeners[t] = [])).push(fn); }
  removeEventListener(t, fn) { this.listeners[t] = (this.listeners[t] || []).filter(f => f !== fn); }
  dispatchEvent(ev) { ev.target = ev.target || this; let n = this; while (n) { (n.listeners && n.listeners[ev.type] || []).forEach(fn => fn.call(n, ev)); n = n.parentNode || (n === document.body ? document : null); } return true; }
  click() { this.dispatchEvent({ type: 'click', target: this, preventDefault() {}, stopPropagation() {} }); }
  focus() { focusCalls.push(this); document.activeElement = this; }
  blur() {}
  select() {}
  scrollIntoView(opts) { scrollCalls.push({ el: this, opts }); }
  getBoundingClientRect() { return { top: 0, left: 0, bottom: 0, right: 0, width: 0, height: 0 }; }
  get offsetParent() { return this.parentElement; }
  get offsetTop() { return 0; }
  get offsetHeight() { return 0; }
  matches(sel) { return sel.split(',').some(s => matchCompound(this, s.trim())); }
  closest(sel) { let n = this; while (n instanceof Element) { if (n.matches(sel)) return n; n = n.parentElement; } return null; }
  querySelector(sel) { return queryAll(this, sel, true)[0] || null; }
  querySelectorAll(sel) { return queryAll(this, sel, false); }
}

function serialize(el) {
  return el.childNodes.map(n => {
    if (n instanceof Text) return n.data;
    const attrs = [...n.attrs].map(([k, v]) => ' ' + k + '="' + v.replace(/"/g, '&quot;') + '"').join('');
    return '<' + n.localName + attrs + '>' + (VOID.has(n.localName) ? '' : serialize(n) + '</' + n.localName + '>');
  }).join('');
}

function parseInto(src, root) {
  const tagRe = /<!--[\s\S]*?-->|<(\/?)([a-zA-Z][\w-]*)((?:\s+[^\s=>\/]+(?:\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>]+))?)*)\s*(\/?)>/g;
  let last = 0, m, cur = root;
  while ((m = tagRe.exec(src))) {
    if (m.index > last) cur.appendChild(new Text(decode(src.slice(last, m.index))));
    last = tagRe.lastIndex;
    if (m[0].startsWith('<!--')) continue;
    const [, close, tag, attrSrc, selfClose] = m;
    const name = tag.toLowerCase();
    if (close) {
      let n = cur;
      while (n && n !== root && n.localName !== name) n = n.parentNode;
      if (n && n !== root) cur = n.parentNode;
      continue;
    }
    const el = new Element(name);
    const attrRe = /([^\s=>\/]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+)))?/g;
    let a;
    while ((a = attrRe.exec(attrSrc))) el.attrs.set(a[1].toLowerCase(), decode(a[2] !== undefined ? a[2] : a[3] !== undefined ? a[3] : a[4] !== undefined ? a[4] : ''));
    if (el.attrs.has('open')) el.open = true;
    if (el.attrs.has('hidden')) el.hidden = true;
    if (el.attrs.has('value')) el.value = el.attrs.get('value');
    cur.appendChild(el);
    if (!selfClose && !VOID.has(name)) cur = el;
  }
  if (last < src.length) cur.appendChild(new Text(decode(src.slice(last))));
}

// Selector subset: compound selectors (tag, #id, .class, [attr], [attr="v"],
// [attr^="v"], :not(compound), :first-child) joined by descendant (space) or
// child (>) combinators, with comma-separated lists.
function matchCompound(el, sel) {
  if (!(el instanceof Element)) return false;
  const parts = splitCombinators(sel);
  let i = parts.length - 1;
  if (!matchSimple(el, parts[i].sel)) return false;
  let node = el;
  for (i--; i >= 0; i--) {
    const comb = parts[i + 1].comb;
    if (comb === '>') { node = node.parentElement; if (!node || !matchSimple(node, parts[i].sel)) return false; }
    else { node = node.parentElement; while (node && !matchSimple(node, parts[i].sel)) node = node.parentElement; if (!node) return false; }
  }
  return true;
}
function splitCombinators(sel) {
  const out = []; let buf = '', depth = 0, comb = '';
  for (let i = 0; i < sel.length; i++) {
    const c = sel[i];
    if (c === '(' || c === '[') depth++;
    if (c === ')' || c === ']') depth--;
    if (depth === 0 && (c === ' ' || c === '>')) {
      if (buf) { out.push({ sel: buf, comb }); buf = ''; comb = c === '>' ? '>' : ' '; }
      else if (c === '>') comb = '>';
      continue;
    }
    buf += c;
  }
  if (buf) out.push({ sel: buf, comb });
  return out;
}
function matchSimple(el, sel) {
  const re = /([a-zA-Z][\w-]*|\*)|#([\w-]+)|\.([\w-]+)|\[([\w-]+)(?:([\^$*]?)=("([^"]*)"|'([^']*)'|([^\]]*)))?\]|:not\(((?:[^()]|\([^()]*\))*)\)|:first-child/g;
  let m, any = false;
  while ((m = re.exec(sel))) {
    any = true;
    if (m[1]) { if (m[1] !== '*' && el.localName !== m[1].toLowerCase()) return false; }
    else if (m[2]) { if (el.id !== m[2]) return false; }
    else if (m[3]) { if (!el.classList.contains(m[3])) return false; }
    else if (m[4]) {
      if (!el.attrs.has(m[4])) return false;
      if (m[6] !== undefined) {
        const want = m[7] !== undefined ? m[7] : m[8] !== undefined ? m[8] : m[9];
        const have = el.attrs.get(m[4]);
        if (m[5] === '^' ? !have.startsWith(want) : m[5] === '$' ? !have.endsWith(want) : m[5] === '*' ? !have.includes(want) : have !== want) return false;
      }
    } else if (m[10] !== undefined) { if (m[10].split(',').some(s => matchCompound(el, s.trim()))) return false; }
    else if (m[0] === ':first-child') { if (!el.parentElement || el.parentElement.children[0] !== el) return false; }
  }
  return any;
}
function queryAll(root, sel, one) {
  const out = [];
  const sels = sel.split(',').map(s => s.trim()).filter(Boolean);
  (function walk(n) {
    for (const c of n.childNodes) {
      if (c instanceof Element) {
        if (sels.some(s => matchCompound(c, s))) { out.push(c); if (one) return true; }
        if (walk(c) && one) return true;
      }
    }
    return false;
  })(root);
  return out;
}

const document = {
  body: new Element('body'),
  documentElement: new Element('html'),
  activeElement: null,
  listeners: {},
  createElement: t => new Element(t),
  createTextNode: t => new Text(t),
  createDocumentFragment: () => new Element('fragment'),
  getElementById(id) { return this.body.querySelector('#' + id); },
  querySelector(sel) { return this.body.querySelector(sel); },
  querySelectorAll(sel) { return this.body.querySelectorAll(sel); },
  addEventListener(t, fn) { (this.listeners[t] || (this.listeners[t] = [])).push(fn); },
  removeEventListener() {},
  dispatchEvent() { return true; },
};
document.documentElement.appendChild(document.body);

// Static dialog skeleton (ids match index.html).
document.body.innerHTML =
  '<div id="config-overlay" class="config-overlay hidden"><div class="config-modal">' +
  '<div class="config-header"><div class="config-title"></div></div>' +
  '<nav class="config-tabs" id="config-tabs"></nav>' +
  '<div id="settings-search-panel" class="settings-search-panel" hidden><input id="settings-search-input"><button id="settings-search-clear" hidden></button><span id="settings-search-count"></span></div>' +
  '<div class="config-body" id="config-body"></div>' +
  '<div class="config-footer"><button id="config-save"></button></div>' +
  '</div></div>';

// ----------------------------------------------------------- lazy globals --
const scriptSrc = (() => {
  let out = '';
  const re = /<script(?![^>]*\bsrc=)[^>]*>([\s\S]*?)<\/script>/g;
  let m;
  while ((m = re.exec(html))) out += m[1] + '\n';
  return out;
})();

// Top-level declarations in index.html's main script sit at a four-space
// indent; a function runs to the first "\n    }\n" and a const/let to the
// next line that starts a new top-level statement.
function extractDecl(name) {
  const fnRe = new RegExp('\\n    (async\\s+)?function\\s+' + name + '\\s*\\(');
  let m = fnRe.exec(scriptSrc);
  if (m) {
    const start = m.index + 1;
    const end = scriptSrc.indexOf('\n    }\n', start);
    if (end < 0) return null;
    return { kind: 'function', src: scriptSrc.slice(start, end + 6) };
  }
  const varRe = new RegExp('\\n    (const|let|var)\\s+' + name + '\\s*=');
  m = varRe.exec(scriptSrc);
  if (m) {
    const start = m.index + 1;
    const nextTop = /\n    (\S)/g;
    nextTop.lastIndex = start + 4;
    let n, end = scriptSrc.length;
    // A multi-line literal closes with a bracket back at the top-level indent
    // (`    ];`), which still belongs to this statement.
    while ((n = nextTop.exec(scriptSrc))) {
      if (!/[\]})]/.test(n[1])) { end = n.index; break; }
    }
    return { kind: m[1], src: scriptSrc.slice(start, end) };
  }
  const bareRe = new RegExp('\\n    (let|var)\\s+' + name + '\\s*;');
  if (bareRe.test(scriptSrc)) return { kind: 'bare', src: '' };
  return null;
}

const missing = new Set();
const resolved = new Set();
const scope = Object.create(null);
const realGlobals = new Set(['undefined', 'NaN', 'Infinity', 'globalThis', 'Symbol', 'Object', 'Array', 'String', 'Number', 'Boolean', 'Math', 'JSON', 'Date', 'RegExp', 'Map', 'Set', 'WeakMap', 'WeakSet', 'Promise', 'Error', 'TypeError', 'RangeError', 'parseInt', 'parseFloat', 'isNaN', 'isFinite', 'encodeURIComponent', 'decodeURIComponent', 'encodeURI', 'decodeURI', 'escape', 'unescape', 'setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'queueMicrotask', 'console', 'Intl', 'Reflect', 'Proxy', 'Function', 'eval', 'arguments', 'Uint8Array', 'ArrayBuffer', 'TextEncoder', 'TextDecoder', 'URL', 'URLSearchParams', 'structuredClone', 'AbortController']);

const inert = new Proxy(function () { return inert; }, {
  get(_, k) { if (k === Symbol.toPrimitive) return () => ''; if (k === 'then') return undefined; if (k === 'length') return 0; return inert; },
  apply() { return inert; },
  construct() { return inert; },
  has() { return true; },
});

const browser = {
  document,
  window: null,
  navigator: { userAgent: 'node', language: 'en-US', clipboard: {} },
  location: { hash: '', href: 'http://localhost/', pathname: '/', search: '', origin: 'http://localhost' },
  history: { replaceState() {}, pushState() {} },
  localStorage: { _m: new Map(), getItem(k) { return this._m.has(k) ? this._m.get(k) : null; }, setItem(k, v) { this._m.set(k, String(v)); }, removeItem(k) { this._m.delete(k); } },
  sessionStorage: { getItem() { return null; }, setItem() {}, removeItem() {} },
  requestAnimationFrame: fn => setTimeout(() => fn(0), 0),
  cancelAnimationFrame: id => clearTimeout(id),
  matchMedia: () => ({ matches: false, addEventListener() {}, addListener() {} }),
  getComputedStyle: () => ({ getPropertyValue: () => '' }),
  fetch: () => new Promise(() => {}),
  CSS: { escape: s => String(s).replace(/([^\w-])/g, '\\$1'), supports: () => true },
  HIVE_GEN_HANDLERS: {},
  Element,
  Node,
  HTMLElement: Element,
  Event: class { constructor(type, init) { this.type = type; Object.assign(this, init || {}); } preventDefault() {} stopPropagation() {} },
  KeyboardEvent: class { constructor(type, init) { this.type = type; Object.assign(this, init || {}); } preventDefault() {} stopPropagation() {} },
  CustomEvent: class { constructor(type, init) { this.type = type; Object.assign(this, init || {}); } },
  performance: { now: () => Date.now() },
  innerWidth: 1400, innerHeight: 1000, devicePixelRatio: 1,
};
browser.window = new Proxy(browser, { get(t, k) { return k in t ? t[k] : resolveName(k); }, has() { return true; }, set(t, k, v) { scope[k] = v; return true; } });
browser.self = browser.window;

function resolveName(name) {
  if (typeof name !== 'string') return undefined;
  if (name in scope) return scope[name];
  if (name in browser) return browser[name];
  if (realGlobals.has(name) || name in globalThis) return globalThis[name];
  const decl = extractDecl(name);
  if (!decl) {
    missing.add(name);
    // Leading-underscore names are window-scoped state (window._lastStatus)
    // that the bootstrap never ran here; everything else is a helper call.
    scope[name] = name.startsWith('_') ? undefined : inert;
    return scope[name];
  }
  resolved.add(name);
  try {
    if (decl.kind === 'bare') {
      scope[name] = undefined;
    } else if (decl.kind === 'function') {
      scope[name] = run('(' + decl.src.replace(/^\s+/, '') + ')');
    } else {
      const eq = decl.src.indexOf('=');
      const expr = decl.src.slice(eq + 1).trim().replace(/;\s*$/, '');
      scope[name] = run('(' + expr + ')');
    }
  } catch (e) {
    missing.add(name + ' (' + String(e.message).split('\n')[0] + ')');
    scope[name] = inert;
  }
  return scope[name];
}

const scopeProxy = new Proxy(scope, {
  has(_, k) { return typeof k === 'string' && k !== '__scope' && k !== '__code' && !realGlobals.has(k) && !(k in globalThis && !(k in browser)); },
  get(_, k) { if (k === Symbol.unscopables) return undefined; return resolveName(k); },
  set(_, k, v) { scope[k] = v; return true; },
});

function run(code) {
  // Sloppy-mode `with` so free identifiers route through the Proxy.
  const fn = new Function('__scope', '__code', 'with (__scope) { return eval(__code); }');
  return fn(scopeProxy, code);
}

// ---------------------------------------------------------------- scenario --
function define(name, value) { scope[name] = value; }
function call(name, ...args) { return resolveName(name)(...args); }
function flush() { return new Promise(r => setTimeout(r, 10)); }

const report = { scenario, missing: () => [...missing].sort(), resolved: () => [...resolved].sort() };
module.exports = { document, run, define, call, flush, report, scrollCalls, focusCalls, Element, extractDecl, resolveName, scope };

if (require.main === module) {
  const scenarios = require('./settings_search_scenarios.js');
  if (!scenarios[scenario]) { console.error('unknown scenario ' + scenario); process.exit(2); }
  Promise.resolve(scenarios[scenario](module.exports)).then(out => {
    console.log(JSON.stringify(out));
  }, err => {
    console.error(err && err.stack || String(err));
    console.error('missing identifiers: ' + report.missing().join(', '));
    process.exit(1);
  });
}
