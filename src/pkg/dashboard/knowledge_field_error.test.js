// Knowledge panel inline required-field errors (hivecommons/hive#17587):
// kbFieldError marks the input invalid, focuses it and renders one
// role="alert" message right under it; kbClearFieldError / the next edit undoes it.
// Run: node --test src/pkg/dashboard/*.test.js
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
const fnSrc = name => {
  const start = html.indexOf('function ' + name + '(');
  assert.ok(start >= 0, name + ' not found');
  let depth = 0;
  for (let i = html.indexOf('{', start); i < html.length; i++) {
    if (html[i] === '{') depth++;
    else if (html[i] === '}' && --depth === 0) return html.slice(start, i + 1);
  }
  throw new Error('unbalanced ' + name);
};

const makeClassList = () => {
  const set = new Set();
  return { add: c => set.add(c), remove: c => set.delete(c), contains: c => set.has(c) };
};

// Minimal fake DOM: an input with a sibling list, attributes, listeners and focus tracking.
const makeDoc = () => {
  const doc = { focused: null, toasts: [] };
  doc.createElement = tag => {
    const el = { tag, className: '', textContent: '', attrs: {}, listeners: {}, parent: null, classList: makeClassList() };
    Object.defineProperty(el, 'className', {
      get() { return [...(el._cls || [])].join(' '); },
      set(v) { el._cls = new Set(String(v).split(/\s+/).filter(Boolean)); el.classList = {
        add: c => el._cls.add(c), remove: c => el._cls.delete(c), contains: c => el._cls.has(c) }; },
    });
    el.setAttribute = (k, v) => { el.attrs[k] = String(v); };
    el.removeAttribute = k => { delete el.attrs[k]; };
    el.addEventListener = (type, fn, opts) => { (el.listeners[type] = el.listeners[type] || []).push({ fn, once: !!(opts && opts.once) }); };
    el.fire = type => {
      const ls = el.listeners[type] || [];
      el.listeners[type] = ls.filter(l => !l.once);
      ls.forEach(l => l.fn());
    };
    el.focus = () => { doc.focused = el; };
    el.remove = () => { if (el.parent) el.parent.siblings.splice(el.parent.siblings.indexOf(el), 1); el.parent = null; };
    Object.defineProperty(el, 'nextElementSibling', {
      get() { const s = el.parent && el.parent.siblings; return s ? s[s.indexOf(el) + 1] || null : null; },
    });
    el.insertAdjacentElement = (where, node) => {
      assert.strictEqual(where, 'afterend');
      const s = el.parent.siblings;
      s.splice(s.indexOf(el) + 1, 0, node);
      node.parent = el.parent;
      return node;
    };
    return el;
  };
  return doc;
};

const setup = () => {
  const doc = makeDoc();
  const toasts = [];
  const h = new Function('document', 'showToast',
    fnSrc('kbClearFieldError') + '\n' + fnSrc('kbFieldError') + '; return { kbClearFieldError, kbFieldError };')(doc, (m, k) => toasts.push([m, k]));
  const container = { siblings: [] };
  const input = doc.createElement('input');
  input.parent = container;
  container.siblings.push(input);
  return { doc, h, input, container, toasts };
};

test('kbFieldError marks the input, focuses it and renders one alert under it', () => {
  const { doc, h, input, container } = setup();
  h.kbFieldError(input, 'URL is required');
  assert.ok(input.classList.contains('field-invalid'));
  assert.strictEqual(input.attrs['aria-invalid'], 'true');
  assert.strictEqual(doc.focused, input);
  assert.strictEqual(container.siblings.length, 2);
  const err = input.nextElementSibling;
  assert.ok(err.classList.contains('field-error'));
  assert.strictEqual(err.attrs.role, 'alert');
  assert.strictEqual(err.textContent, 'URL is required');
});

test('a second call replaces the message instead of duplicating it', () => {
  const { h, input, container } = setup();
  h.kbFieldError(input, 'first');
  h.kbFieldError(input, 'second');
  assert.strictEqual(container.siblings.length, 2);
  assert.strictEqual(input.nextElementSibling.textContent, 'second');
});

test('editing the input clears the invalid state and the message', () => {
  const { h, input, container } = setup();
  h.kbFieldError(input, 'Display name is required');
  input.fire('input');
  assert.ok(!input.classList.contains('field-invalid'));
  assert.ok(!('aria-invalid' in input.attrs));
  assert.strictEqual(container.siblings.length, 1);
});

test('kbClearFieldError clears explicitly and tolerates a clean or missing input', () => {
  const { h, input, container } = setup();
  h.kbFieldError(input, 'x');
  h.kbClearFieldError(input);
  assert.ok(!input.classList.contains('field-invalid'));
  assert.ok(!('aria-invalid' in input.attrs));
  assert.strictEqual(container.siblings.length, 1);
  h.kbClearFieldError(input);
  h.kbClearFieldError(null);
});

test('a missing input falls back to an error toast', () => {
  const { h, toasts } = setup();
  h.kbFieldError(null, 'boom');
  assert.deepStrictEqual(toasts, [['boom', 'error']]);
});

test('Clone & Index busy state targets the data-action button and a blank branch is sent blank', () => {
  const fn = fnSrc('kbAddGitSource');
  assert.ok(fn.includes('[data-action="kbAddGitSource"]'), 'busy-state selector must match the data-action button');
  assert.ok(!fn.includes('onclick='), 'stale onclick selector');
  assert.ok(!fn.includes("branch || 'main'"), 'blank branch must not be guessed as main');
  assert.ok(html.includes('Branch (blank = repo default)'));
});

test('a failed connect is kept and rendered as an error row with a Retry control', () => {
  assert.ok(html.includes('_kbGitSourceFailed = { url,'));
  assert.ok(html.includes('&middot; error: ${escapeHtml(failedGs.error)}'));
  assert.ok(html.includes('data-action="kbRetryGitSource"'));
  assert.ok(html.includes('function kbDismissGitSourceFailure('));
});
