// Settings -> Repos -> Parent auto-close editor (hivecommons/hive#19070):
// renderParentAutoClosePanel paints one row per monitored repo with a select of
// the three policies, shows the effective value, and Save PUTs only the rows
// that changed to /api/config/repos/parent-autoclose. A server refusal is shown
// verbatim and the edits are kept.
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


const OPTIONS = [
  { value: 'true', label: 'Close finished parents (default)', description: 'close finished parents' },
  { value: 'epics_too', label: 'Also close epics and holds', description: 'close epics too' },
  { value: 'false', label: 'Never auto-close parents', description: 'never close' },
];

const setup = ({ repos, put }) => {
  const doc = makeDoc();
  const toasts = [];
  const calls = [];
  const fetchStub = async (url, opts) => {
    calls.push({ url, opts, body: JSON.parse(opts.body) });
    return put(calls[calls.length - 1]);
  };
  const render = new Function('document', 'fetch', 'showToast', '_inceptionAuthHeaders', 'saveErrorMessage',
    fnSrc('parentAutoCloseNode') + '\n' + fnSrc('renderParentAutoClosePanel') + '\nreturn renderParentAutoClosePanel;')(
    doc, fetchStub, (m, k) => toasts.push([m, k]), () => ({}), async res => res.errorMessage);
  const host = doc.createElement('div');
  render(host, { default: 'true', options: OPTIONS, repos });
  return { host, toasts, calls };
};

const rows = host => host.find(n => String(n.className).startsWith('config-list-item config-repo-row'));
const selects = node => node.find(n => n.tag === 'select');
const saveButton = host => host.find(n => n.tag === 'button' && n.textContent === 'Save parent auto-close')[0];
const choose = (row, value) => { const sel = selects(row)[0]; sel.value = value; sel.fire('change'); };

test('one row per monitored repo with three options, the effective value and the yaml hint', () => {
  const { host } = setup({ repos: [{ repo: 'acme/a', value: 'true', explicit: false }, { repo: 'acme/b', value: 'epics_too', explicit: true }], put: () => { throw new Error('no save'); } });
  const r = rows(host);
  assert.strictEqual(r.length, 2);
  assert.strictEqual(selects(r[0])[0].children.length, 3);
  assert.deepStrictEqual(selects(r[1])[0].children.map(o => o.selected), [false, true, false]);
  assert.match(r[0].find(n => n.className === 'rt-inherit')[0].textContent, /Close finished parents \(default\) \(inherited\)/);
  assert.match(r[1].find(n => n.className === 'rt-inherit')[0].textContent, /^now: Also close epics and holds$/);
  assert.ok(host.textContent.includes('repos.<key>.parent_autoclose'));
  assert.match(host.textContent, /never closed again/);
  assert.match(host.textContent, /never auto-closed/);
});

test('Save is disabled until a row changes, then PUTs only the changed rows', async () => {
  const { host, calls, toasts } = setup({
    repos: [{ repo: 'acme/a', value: 'true', explicit: false }, { repo: 'acme/b', value: 'false', explicit: true }],
    put: call => ({ ok: true, json: async () => ({ default: 'true', repos: [{ repo: call.body.repo, value: call.body.value, explicit: call.body.value !== 'true' }] }) }),
  });
  assert.strictEqual(saveButton(host).disabled, true);
  choose(rows(host)[0], 'epics_too');
  assert.strictEqual(saveButton(host).disabled, false);
  await saveButton(host).fire('click')[0];
  assert.strictEqual(calls.length, 1);
  assert.strictEqual(calls[0].url, '/api/config/repos/parent-autoclose');
  assert.strictEqual(calls[0].opts.method, 'PUT');
  assert.deepStrictEqual(calls[0].body, { repo: 'acme/a', value: 'epics_too' });
  assert.deepStrictEqual(toasts, [['Parent auto-close saved', 'success']]);
  assert.match(rows(host)[0].find(n => n.className === 'rt-inherit')[0].textContent, /^now: Also close epics and holds$/);
  assert.strictEqual(saveButton(host).disabled, true);
});

test('a refusal is shown as-is and the edit is kept', async () => {
  const { host, calls, toasts } = setup({
    repos: [{ repo: 'acme/a', value: 'true', explicit: false }],
    put: () => ({ ok: false, status: 400, errorMessage: 'value must be one of the three' }),
  });
  choose(rows(host)[0], 'false');
  await saveButton(host).fire('click')[0];
  assert.strictEqual(calls.length, 1);
  assert.deepStrictEqual(toasts, []);
  assert.match(host.textContent, /acme\/a: value must be one of the three/);
  assert.strictEqual(selects(rows(host)[0])[0].children.find(o => o.selected).value, 'false');
  assert.strictEqual(saveButton(host).disabled, false);
});

test('no monitored repos says so', () => {
  const { host } = setup({ repos: [], put: () => { throw new Error('no save'); } });
  assert.match(host.textContent, /No monitored repos configured/);
});
