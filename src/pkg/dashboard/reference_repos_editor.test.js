// Settings → Repos → Reference repos editor (hivecommons/hive#18136):
// renderReferenceReposPanel paints one row per entry with write checkboxes
// and a remove button, shows an operator-set write_token_file as read-only
// text (never an input: the path is file-only), carries derived_from through
// to the PUT body so the server can drop derived entries, and shows the
// server's refusal verbatim without losing the edits. Derived entries
// (hivecommons/hive#18161) are read-only rows with a lock, a link to the
// request issue and a credential-state chip; a 409 is also toasted.
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

const setup = ({ entries, kinds = ['issues', 'prs'], put }) => {
  const doc = makeDoc();
  const toasts = [];
  const calls = [];
  const fetchStub = async (url, opts) => {
    calls.push({ url, opts, body: JSON.parse(opts.body) });
    return put(calls[calls.length - 1]);
  };
  const render = new Function('document', 'window', 'fetch', 'showToast', '_inceptionAuthHeaders', 'saveErrorMessage',
    fnSrc('referenceRepoNode') + '\n' + fnSrc('referenceRepoYamlHint') + '\n' + fnSrc('referenceRepoDerivedLink') + '\n' + fnSrc('renderReferenceReposPanel') + '\nreturn renderReferenceReposPanel;')(
    doc, { _githubBaseUrl: 'https://github.example.com/' }, fetchStub, (m, k) => toasts.push([m, k]), () => ({}), async res => res.errorMessage);
  const host = doc.createElement('div');
  render(host, entries, kinds);
  return { doc, host, toasts, calls };
};

const rows = host => host.find(n => String(n.className).startsWith('config-list-item config-repo-row'));
const inputs = (node, type) => node.find(n => n.tag === 'input' && n.type === type);
const buttons = (node, text) => node.find(n => n.tag === 'button' && n.textContent === text);

test('rows carry the repo, one checkbox per write kind, the tier and a remove button; no token input anywhere', () => {
  const { host } = setup({ entries: [{ repo: 'hivecommons/docs' }, { repo: 'acme/tools', write: ['issues'] }], put: () => { throw new Error('no save'); } });
  const r = rows(host);
  assert.strictEqual(r.length, 2);
  assert.strictEqual(r[0].find(n => n.className === 'config-repo-name')[0].textContent, 'hivecommons/docs');
  assert.strictEqual(inputs(r[0], 'checkbox').length, 2);
  assert.strictEqual(r[0].find(n => n.className === 'rt-inherit')[0].textContent, 'read-only');
  assert.strictEqual(r[1].find(n => n.className === 'rt-inherit')[0].textContent, 'issues');
  assert.deepStrictEqual(inputs(r[1], 'checkbox').map(b => b.checked), [true, false]);
  assert.strictEqual(inputs(host, 'text').length, 1, 'the only text input is the owner/repo add box');
  assert.strictEqual(buttons(r[0], '✕').length, 1);
  assert.ok(host.find(n => n.className === 'settings-yaml-hint').length >= 3, 'yaml hints for repo, write[] and write_token_file');
});

test('an operator-set write_token_file is shown as read-only text and sent back unchanged', async () => {
  const entries = [{ repo: 'acme/tools', write: ['issues'], write_token_file: '/etc/hive/acme.token' }];
  const { host, calls, toasts } = setup({
    entries,
    put: call => ({ ok: true, json: async () => ({ reference_repos: call.body.reference_repos, write_kinds: ['issues', 'prs'] }) }),
  });
  const token = host.find(n => (n.className || '').includes('cfg-reference-repo-token'));
  assert.strictEqual(token.length, 1);
  assert.strictEqual(token[0].tag, 'span', 'the path is text, not an editable input');
  assert.strictEqual(token[0].textContent, 'token file: /etc/hive/acme.token');
  assert.match(token[0].title, /config file/);
  await buttons(host, 'Save reference repos')[0].fire('click')[0];
  assert.strictEqual(calls.length, 1);
  assert.strictEqual(calls[0].opts.method, 'PUT');
  assert.deepStrictEqual(calls[0].body, { reference_repos: [{ repo: 'acme/tools', write: ['issues'], write_token_file: '/etc/hive/acme.token' }] });
  assert.deepStrictEqual(toasts, [['Reference repos saved', 'success']]);
});

test('a derived entry keeps derived_from in the PUT body; a hand-written one has none', async () => {
  const entries = [
    { repo: 'hivecommons/docs' },
    { repo: 'acme/docs', write: ['issues', 'prs'], derived_from: 'cncf/techdocs#395', credential_state: 'write-tier' },
  ];
  const { host, calls } = setup({
    entries,
    put: call => ({ ok: true, json: async () => ({ reference_repos: call.body.reference_repos }) }),
  });
  await buttons(host, 'Save reference repos')[0].fire('click')[0];
  assert.deepStrictEqual(calls[0].body.reference_repos, [
    { repo: 'hivecommons/docs' },
    { repo: 'acme/docs', write: ['issues', 'prs'], derived_from: 'cncf/techdocs#395' },
  ]);
});

test('add, toggle and remove edit the list; a refused save shows the server message and keeps the edits', async () => {
  const message = 'project.reference_repos[0]: "myorg/repo1" is already in project.repos';
  const { host, calls, toasts } = setup({
    entries: [{ repo: 'old/one' }],
    put: () => ({ ok: false, status: 400, errorMessage: message }),
  });
  const add = inputs(host, 'text')[0];
  add.value = 'https://github.com/myorg/repo1.git';
  buttons(host, '+ Add')[0].fire('click');
  let r = rows(host);
  assert.deepStrictEqual(r.map(x => x.find(n => n.className === 'config-repo-name')[0].textContent), ['old/one', 'myorg/repo1']);
  const prs = inputs(r[1], 'checkbox')[1];
  prs.checked = true;
  prs.fire('change');
  r = rows(host);
  assert.strictEqual(r[1].find(n => n.className === 'rt-inherit')[0].textContent, 'prs');
  buttons(r[0], '✕')[0].fire('click');
  r = rows(host);
  assert.strictEqual(r.length, 1);

  await buttons(host, 'Save reference repos')[0].fire('click')[0];
  assert.deepStrictEqual(calls[0].body, { reference_repos: [{ repo: 'myorg/repo1', write: ['prs'] }] });
  const status = host.find(n => n.className === 'fs-sm text-danger');
  assert.strictEqual(status.length, 1);
  assert.strictEqual(status[0].textContent, message);
  assert.strictEqual(toasts.length, 0);
  assert.strictEqual(rows(host).length, 1, 'edits survive a refused save');
  assert.strictEqual(buttons(host, 'Save reference repos')[0].disabled, false);
});

test('a derived entry is a read-only row: lock, source issue link, state chip and reason, no controls', () => {
  const { host } = setup({
    entries: [
      { repo: 'acme/docs', write: ['issues', 'prs'], derived_from: 'cncf/techdocs#395', credential_state: 'write-tier' },
      { repo: 'zeta/site', derived_from: 'cncf/techdocs#395', credential_state: 'read-only', credential_reason: 'no App installation on zeta' },
      { repo: 'hand/written', write: ['issues'] },
    ],
    put: () => { throw new Error('no save'); },
  });
  const r = rows(host);
  assert.strictEqual(r.length, 3);
  const [docs, zeta, hand] = r;
  assert.ok(docs.className.includes('cfg-reference-repo-derived'));
  assert.strictEqual(docs.find(n => n.className === 'cfg-reference-repo-lock').length, 1);
  assert.strictEqual(inputs(docs, 'checkbox').length, 0, 'no write checkboxes on a derived row');
  assert.strictEqual(buttons(docs, '\u2715').length, 0, 'no remove button on a derived row');
  const link = docs.find(n => n.tag === 'a')[0];
  assert.strictEqual(link.textContent, 'from cncf/techdocs#395');
  assert.strictEqual(link.href, 'https://github.example.com/cncf/techdocs/issues/395');
  assert.strictEqual(link.target, '_blank');
  const chip = docs.find(n => n.className.includes('cfg-reference-repo-state'))[0];
  assert.strictEqual(chip.textContent, 'issues+prs \u00b7 write-tier');
  assert.ok(chip.className.includes('cfg-reference-repo-state-write-tier'));
  assert.strictEqual(docs.find(n => n.className.includes('cfg-reference-repo-reason')).length, 0);

  const zchip = zeta.find(n => n.className.includes('cfg-reference-repo-state'))[0];
  assert.strictEqual(zchip.textContent, 'read-only \u00b7 read-only');
  assert.strictEqual(zchip.title, 'no App installation on zeta');
  assert.strictEqual(zeta.find(n => n.className.includes('cfg-reference-repo-reason'))[0].textContent, 'no App installation on zeta');

  assert.ok(!hand.className.includes('cfg-reference-repo-derived'));
  assert.strictEqual(inputs(hand, 'checkbox').length, 2, 'hand-written rows keep their controls');
  assert.strictEqual(buttons(hand, '\u2715').length, 1);
});

test('a 409 on save is toasted as an error and shown in the status line; the list is kept', async () => {
  const message = 'reference repo "acme/docs" is derived from request issue cncf/techdocs#395 and cannot be removed here; edit or close that issue instead';
  const { host, calls, toasts } = setup({
    entries: [{ repo: 'acme/docs', write: ['issues', 'prs'], derived_from: 'cncf/techdocs#395', credential_state: 'write-tier' }, { repo: 'hand/written' }],
    put: () => ({ ok: false, status: 409, errorMessage: message }),
  });
  await buttons(host, 'Save reference repos')[0].fire('click')[0];
  assert.deepStrictEqual(calls[0].body.reference_repos, [
    { repo: 'acme/docs', write: ['issues', 'prs'], derived_from: 'cncf/techdocs#395' },
    { repo: 'hand/written' },
  ]);
  assert.deepStrictEqual(toasts, [[message, 'error']]);
  assert.strictEqual(host.find(n => n.className === 'fs-sm text-danger')[0].textContent, message);
  assert.strictEqual(rows(host).length, 2);
});
