// Settings -> Features -> Review -> Rules from review feedback
// (hivecommons/hive#19138): renderReviewFeedbackRulesPanel paints the enabled
// switch, the trusted associations, the extra phrases and the built-in signals,
// and Save PUTs them to /api/config/review-feedback/rules. An environment
// override is called out, a server refusal is shown verbatim and the edits are
// kept.
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
    el.setAttribute = () => {};
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
  enabled: true,
  trusted_associations: [],
  extra_phrases: ['house rule'],
  disabled_signals: ['knowledge-base'],
  effective_enabled: true,
  effective_trusted_associations: ['OWNER', 'MEMBER', 'COLLABORATOR'],
  enabled_from_env: false,
  trusted_from_env: false,
  default_trusted_associations: ['OWNER', 'MEMBER', 'COLLABORATOR'],
  associations: ['OWNER', 'MEMBER', 'COLLABORATOR', 'CONTRIBUTOR'],
  signals: [{ name: 'never', description: '"should never"' }, { name: 'knowledge-base', description: '"knowledge base"' }],
};

const setup = ({ data = DATA, put }) => {
  const doc = makeDoc();
  const toasts = [];
  const calls = [];
  const fetchStub = async (url, opts) => {
    calls.push({ url, opts, body: JSON.parse(opts.body) });
    return put(calls[calls.length - 1]);
  };
  const render = new Function('document', 'fetch', 'showToast', '_inceptionAuthHeaders', 'saveErrorMessage',
    "const REVIEW_FEEDBACK_RULES_URL = '/api/config/review-feedback/rules';\n" +
    fnSrc('rfrNode') + '\n' + fnSrc('reviewFeedbackRulesPhrases') + '\n' + fnSrc('renderReviewFeedbackRulesPanel') + '\nreturn renderReviewFeedbackRulesPanel;')(
    doc, fetchStub, (m, k) => toasts.push([m, k]), () => ({}), async res => res.errorMessage);
  const host = doc.createElement('div');
  render(host, data);
  return { host, toasts, calls };
};

const boxes = host => host.find(n => n.tag === 'input');
const boxFor = (host, label) => host.find(n => n.tag === 'label').find(l => l.textContent.startsWith(label)).find(n => n.tag === 'input')[0];
const flip = (box, checked) => { box.checked = checked; box.fire('change'); };
const saveButton = host => host.find(n => n.tag === 'button' && n.textContent === 'Save review-feedback rules')[0];
const textarea = host => host.find(n => n.tag === 'textarea')[0];
const ok = json => ({ ok: true, json: async () => json });

test('shows the enabled switch, the default trusted set, the phrases and the signals', () => {
  const { host } = setup({ put: () => { throw new Error('no save'); } });
  assert.strictEqual(boxFor(host, 'Capture rules').checked, true);
  assert.deepStrictEqual(['OWNER', 'MEMBER', 'COLLABORATOR', 'CONTRIBUTOR'].map(a => boxFor(host, a).checked), [true, true, true, false]);
  assert.strictEqual(textarea(host).value, 'house rule');
  assert.strictEqual(boxFor(host, 'never').checked, true);
  assert.strictEqual(boxFor(host, 'knowledge-base').checked, false);
  assert.ok(host.textContent.includes('review_feedback.rules.enabled'));
  assert.strictEqual(saveButton(host).disabled, true);
});

test('environment overrides are called out', () => {
  const { host } = setup({
    data: Object.assign({}, DATA, { enabled_from_env: true, effective_enabled: false, trusted_from_env: true, effective_trusted_associations: ['MEMBER'] }),
    put: () => { throw new Error('no save'); },
  });
  assert.match(host.textContent, /HIVE_REVIEW_FEEDBACK_RULES environment variable is set.*currently off/);
  assert.match(host.textContent, /HIVE_REVIEW_FEEDBACK_RULE_TRUSTED_ASSOCIATIONS environment variable is set \(currently MEMBER\)/);
});

test('Save PUTs the edited values; the default trusted set is sent as an empty list', async () => {
  const { host, calls, toasts } = setup({ put: () => ok(Object.assign({}, DATA, { enabled: false })) });
  flip(boxFor(host, 'Capture rules'), false);
  flip(boxFor(host, 'never'), false);
  textarea(host).value = ' house rule \n\nas a rule\n';
  textarea(host).fire('input');
  assert.strictEqual(saveButton(host).disabled, false);
  await saveButton(host).fire('click')[0];
  assert.strictEqual(calls.length, 1);
  assert.strictEqual(calls[0].url, '/api/config/review-feedback/rules');
  assert.strictEqual(calls[0].opts.method, 'PUT');
  assert.deepStrictEqual(calls[0].body, {
    enabled: false,
    trusted_associations: [],
    extra_phrases: ['house rule', 'as a rule'],
    disabled_signals: ['knowledge-base', 'never'].sort(),
  });
  assert.deepStrictEqual(toasts, [['Review-feedback rules saved', 'success']]);
});

test('a changed trusted set is sent as chosen', async () => {
  const { host, calls } = setup({ put: () => ok(DATA) });
  flip(boxFor(host, 'COLLABORATOR'), false);
  flip(boxFor(host, 'CONTRIBUTOR'), true);
  await saveButton(host).fire('click')[0];
  assert.deepStrictEqual(calls[0].body.trusted_associations.sort(), ['CONTRIBUTOR', 'MEMBER', 'OWNER']);
});

test('unticking every association asks instead of silently restoring the default', async () => {
  const { host, calls } = setup({ put: () => { throw new Error('no save'); } });
  ['OWNER', 'MEMBER', 'COLLABORATOR'].forEach(a => flip(boxFor(host, a), false));
  await saveButton(host).fire('click');
  assert.strictEqual(calls.length, 0);
  assert.match(host.textContent, /Tick at least one association/);
});

test('a refusal is shown as-is and the edits are kept', async () => {
  const { host, toasts } = setup({ put: () => ({ ok: false, status: 400, errorMessage: 'extra_phrases: "abc" must be 4 to 100 characters' }) });
  textarea(host).value = 'abc';
  textarea(host).fire('input');
  await saveButton(host).fire('click')[0];
  assert.deepStrictEqual(toasts, []);
  assert.match(host.textContent, /must be 4 to 100 characters/);
  assert.strictEqual(textarea(host).value, 'abc');
  assert.strictEqual(saveButton(host).disabled, false);
});
