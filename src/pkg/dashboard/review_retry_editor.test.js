// Settings -> Features -> Review: stranded review-feedback retry card
// (hivecommons/hive#19320). renderReviewRetryPanel shows each
// review_feedback.retry.* value with where it comes from; Save PUTs to
// /api/config/review-feedback/retry and shows a server refusal as-is.
// Run: node --test src/pkg/dashboard/*.test.js
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
const ASYNC_PREFIX = 'async ';
const fnSrc = name => {
  let start = html.indexOf('function ' + name + '(');
  assert.ok(start >= 0, name + ' not found');
  if (html.slice(start - ASYNC_PREFIX.length, start) === ASYNC_PREFIX) start -= ASYNC_PREFIX.length;
  let depth = 0;
  for (let i = html.indexOf('{', start); i < html.length; i++) {
    if (html[i] === '{') depth++;
    else if (html[i] === '}' && --depth === 0) return html.slice(start, i + 1);
  }
  throw new Error('unbalanced ' + name);
};

// Minimal fake DOM: elements with children, className, textContent (which
// clears children, as in the browser), attributes and listeners.
const makeDoc = () => {
  const doc = {};
  const makeEl = tag => {
    const el = { tag, children: [], listeners: {}, className: '', title: '', _text: '' };
    Object.defineProperty(el, 'textContent', {
      get() { return el._text + el.children.map(c => c.textContent).join(''); },
      set(v) { el._text = String(v); el.children = []; },
    });
    el.appendChild = c => { el.children.push(c); return c; };
    el.addEventListener = (type, fn) => { (el.listeners[type] = el.listeners[type] || []).push(fn); };
    el.fire = (type, ev) => (el.listeners[type] || []).map(fn => fn(ev || {}));
    el.find = pred => {
      const out = [];
      const walk = n => { if (pred(n)) out.push(n); (n.children || []).forEach(walk); };
      walk(el);
      return out;
    };
    return el;
  };
  doc.createElement = makeEl;
  doc.createTextNode = text => ({ tag: '#text', children: [], className: '', textContent: String(text) });
  doc.getElementById = () => null;
  return doc;
};



const DATA = {
  max_retries: { saved: null, effective: 2, source: 'default', default: 2, env_var: 'HIVE_REVIEW_FEEDBACK_RETRY_MAX' },
  backoff: { saved: null, effective: '30m0s', source: 'env', default: '1h0m0s', env_var: 'HIVE_REVIEW_FEEDBACK_RETRY_BACKOFF' },
  max_max_retries: 10,
  min_backoff: '10m0s',
};

const setup = put => {
  const doc = makeDoc();
  const toasts = [];
  const calls = [];
  const fetchStub = async (url, opts) => {
    calls.push({ url, opts, body: JSON.parse(opts.body) });
    return put(calls[calls.length - 1]);
  };
  const render = new Function('document', 'fetch', 'showToast', '_inceptionAuthHeaders', 'saveErrorMessage',
    fnSrc('reviewRetryNode') + '\n' + fnSrc('reviewRetrySourceText') + '\n' + fnSrc('renderReviewRetryPanel') + '\nreturn renderReviewRetryPanel;')(
    doc, fetchStub, (m, k) => toasts.push([m, k]), () => ({}), async res => res.errorMessage);
  const host = doc.createElement('div');
  render(host, DATA);
  return { host, toasts, calls };
};

const saveButton = host => host.find(n => n.tag === 'button' && n.textContent === 'Save retry settings')[0];
const inputs = host => host.find(n => n.tag === 'input');

test('shows both keys and says when an env var supplies a value', () => {
  const { host } = setup(() => { throw new Error('no save'); });
  for (const key of ['review_feedback.retry.max_retries', 'review_feedback.retry.backoff']) {
    assert.ok(host.textContent.includes(key), key);
  }
  assert.ok(host.textContent.includes('now: 2 (default)'));
  assert.ok(host.textContent.includes('now: 30m0s (from HIVE_REVIEW_FEEDBACK_RETRY_BACKOFF)'));
  assert.strictEqual(saveButton(host).disabled, true);
});

test('Save PUTs the changed values to the owner-only endpoint', async () => {
  const { host, calls, toasts } = setup(() => ({ ok: true, json: async () => DATA }));
  const [mr, bk] = inputs(host);
  mr.value = '3';
  mr.fire('input');
  bk.value = ' 2h ';
  bk.fire('input');
  assert.strictEqual(saveButton(host).disabled, false);
  await saveButton(host).fire('click')[0];
  assert.strictEqual(calls.length, 1);
  assert.strictEqual(calls[0].url, '/api/config/review-feedback/retry');
  assert.strictEqual(calls[0].opts.method, 'PUT');
  assert.deepStrictEqual(calls[0].body, { max_retries: 3, backoff: '2h' });
  assert.deepStrictEqual(toasts, [['Retry settings saved', 'success']]);
});

test('blank inputs clear the saved values', async () => {
  const { host, calls } = setup(() => ({ ok: true, json: async () => DATA }));
  const [mr] = inputs(host);
  mr.value = '';
  mr.fire('input');
  await saveButton(host).fire('click')[0];
  assert.deepStrictEqual(calls[0].body, {});
});

test('a refusal is shown as-is and Save stays enabled', async () => {
  const { host, toasts } = setup(() => ({ ok: false, status: 400, errorMessage: 'backoff must be at least 10m0s' }));
  const [, bk] = inputs(host);
  bk.value = '1m';
  bk.fire('input');
  await saveButton(host).fire('click')[0];
  assert.ok(host.textContent.includes('backoff must be at least 10m0s'));
  assert.strictEqual(saveButton(host).disabled, false);
  assert.deepStrictEqual(toasts, []);
});
