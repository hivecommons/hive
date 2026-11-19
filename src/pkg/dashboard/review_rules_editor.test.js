// Settings -> Features -> Review: reviewer-stated rules card (hivecommons/hive#19148).
// renderReviewRulesPanel shows each review_feedback.rules.* value with where it
// comes from (saved, env var or default); Save PUTs to
// /api/config/review-feedback/rules and shows a server refusal as-is.
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
  enabled: { saved: null, effective: true, source: 'default', default: true, env_var: 'HIVE_REVIEW_FEEDBACK_RULES' },
  trusted_associations: { saved: null, effective: ['OWNER'], source: 'env', default: ['OWNER', 'MEMBER', 'COLLABORATOR'], env_var: 'HIVE_REVIEW_FEEDBACK_RULE_TRUSTED_ASSOCIATIONS' },
  extra_phrases: { saved: null, effective: [], source: 'default', default: [], env_var: 'HIVE_REVIEW_FEEDBACK_RULE_EXTRA_PHRASES' },
  disabled_signals: { saved: null, effective: [], source: 'default', default: [], env_var: 'HIVE_REVIEW_FEEDBACK_RULE_DISABLED_SIGNALS' },
  associations: ['OWNER', 'MEMBER', 'COLLABORATOR'],
  signals: [{ name: 'source_of_truth', examples: 'X is the source of truth' }],
  max_phrases: 20,
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
    fnSrc('reviewRulesNode') + '\n' + fnSrc('reviewRulesSourceText') + '\n' + fnSrc('renderReviewRulesPanel') + '\nreturn renderReviewRulesPanel;')(
    doc, fetchStub, (m, k) => toasts.push([m, k]), () => ({}), async res => res.errorMessage);
  const host = doc.createElement('div');
  render(host, DATA);
  return { host, toasts, calls };
};

const saveButton = host => host.find(n => n.tag === 'button' && n.textContent === 'Save reviewer-rule settings')[0];

test('shows the four keys and says when an env var supplies a value', () => {
  const { host } = setup(() => { throw new Error('no save'); });
  for (const key of ['review_feedback.rules.enabled', 'review_feedback.rules.trusted_associations[]', 'review_feedback.rules.extra_phrases[]', 'review_feedback.rules.disabled_signals[]']) {
    assert.ok(host.textContent.includes(key), key);
  }
  assert.ok(host.textContent.includes('now: OWNER (from HIVE_REVIEW_FEEDBACK_RULE_TRUSTED_ASSOCIATIONS)'));
  assert.ok(host.textContent.includes('now: on (default)'));
  assert.strictEqual(saveButton(host).disabled, true);
});

test('Save PUTs the changed value to the owner-only endpoint', async () => {
  const { host, calls, toasts } = setup(() => ({ ok: true, json: async () => DATA }));
  const sel = host.find(n => n.tag === 'select')[0];
  sel.value = 'off';
  sel.fire('change');
  assert.strictEqual(saveButton(host).disabled, false);
  await saveButton(host).fire('click')[0];
  assert.strictEqual(calls.length, 1);
  assert.strictEqual(calls[0].url, '/api/config/review-feedback/rules');
  assert.strictEqual(calls[0].opts.method, 'PUT');
  assert.deepStrictEqual(calls[0].body, { enabled: false });
  assert.deepStrictEqual(toasts, [['Reviewer-rule settings saved', 'success']]);
});

test('a refusal is shown as-is and Save stays enabled', async () => {
  const { host, toasts } = setup(() => ({ ok: false, status: 400, errorMessage: 'unknown signal "x"' }));
  const sel = host.find(n => n.tag === 'select')[0];
  sel.value = 'on';
  sel.fire('change');
  await saveButton(host).fire('click')[0];
  assert.deepStrictEqual(toasts, []);
  assert.match(host.textContent, /unknown signal "x"/);
  assert.strictEqual(saveButton(host).disabled, false);
});
