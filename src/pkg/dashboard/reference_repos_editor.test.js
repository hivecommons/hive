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

const setup = ({ entries, kinds = ['issues', 'prs'], put, identity = null, meta, api = () => { throw new Error('no identity call expected'); } }) => {
  const doc = makeDoc();
  const toasts = [];
  const calls = [];
  const reloads = [];
  const fetchStub = async (url, opts) => {
    if (String(url).startsWith('/api/github-owner-identity')) {
      calls.push({ url, opts, body: opts && opts.body ? JSON.parse(opts.body) : null });
      return api(calls[calls.length - 1]);
    }
    calls.push({ url, opts, body: JSON.parse(opts.body) });
    return put(calls[calls.length - 1]);
  };
  const render = new Function('document', 'window', 'fetch', 'showToast', '_inceptionAuthHeaders', 'saveErrorMessage', 'loadReferenceRepos', 'setTimeout',
    fnSrc('referenceRepoNode') + '\n' + fnSrc('referenceRepoYamlHint') + '\n' + fnSrc('referenceRepoDerivedLink') + '\n' +
    fnSrc('referenceRepoCredentialChip') + '\n' + fnSrc('referenceRepoWriteMeta') + '\n' + fnSrc('referenceRepoIdentityLine') + '\n' + fnSrc('renderWriteIdentityPanel') + '\n' +
    fnSrc('renderOwnerIdentityPanel') + '\n' + fnSrc('renderReferenceReposPanel') + '\nreturn renderReferenceReposPanel;')(
    doc, { _githubBaseUrl: 'https://github.example.com/' }, fetchStub, (m, k) => toasts.push([m, k]), () => ({}), async res => res.errorMessage,
    () => reloads.push(Date.now()), fn => fn());
  const host = doc.createElement('div');
  render(host, entries, kinds, identity, meta);
  return { doc, host, toasts, calls, reloads };
};
const identityPanel = host => host.find(n => String(n.className).includes('cfg-owner-identity') && !String(n.className).includes('-status'))[0];

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
      { repo: 'zeta/site', derived_from: 'cncf/techdocs#395', credential_state: 'read-only', credential_reason: 'the hive owner has not connected a GitHub account' },
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
  assert.strictEqual(zchip.title, 'the hive owner has not connected a GitHub account');
  assert.strictEqual(zeta.find(n => n.className.includes('cfg-reference-repo-reason'))[0].textContent, 'the hive owner has not connected a GitHub account');

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

// hivecommons/hive#18200: the identity panel shows who the hive writes to
// remote repos as, with Connect (device flow) and Disconnect; write entries
// show the login in their credential chip.
test('identity panel: unavailable store, not connected, connected and revoked states', () => {
  let { host } = setup({ entries: [], put: () => { throw new Error('no save'); } });
  assert.match(identityPanel(host).textContent, /Unavailable on this hive/);
  assert.strictEqual(buttons(host, 'Connect GitHub account').length, 0);

  ({ host } = setup({ entries: [], put: () => { throw new Error('no save'); }, identity: { connected: false, usable: false, reason: 'the hive owner has not connected a GitHub account', scope_required: 'public_repo' } }));
  assert.match(identityPanel(host).textContent, /Not connected: entries that would post as you are served read-only/);
  assert.strictEqual(buttons(host, 'Connect GitHub account').length, 1);
  assert.strictEqual(buttons(host, 'Disconnect').length, 0);

  ({ host } = setup({ entries: [], put: () => { throw new Error('no save'); }, identity: { connected: true, usable: true, login: 'nate-double-u', scope: 'public_repo' } }));
  const who = identityPanel(host).find(n => String(n.className).includes('cfg-reference-repo-state-write-tier'))[0];
  assert.strictEqual(who.textContent, 'writing as nate-double-u');
  assert.strictEqual(buttons(host, 'Disconnect').length, 1);
  assert.strictEqual(buttons(host, 'Connect GitHub account').length, 0);
  assert.doesNotMatch(host.textContent, /@nate-double-u/, 'never an @-mention');

  ({ host } = setup({ entries: [], put: () => { throw new Error('no save'); }, identity: { connected: true, usable: false, login: 'nate-double-u', reason: 'the hive owner\'s GitHub token is no longer valid; reconnect the account in Settings' } }));
  assert.match(identityPanel(host).textContent, /disconnected token for nate-double-u/);
  assert.strictEqual(buttons(host, 'Remove token').length, 1);
  assert.strictEqual(buttons(host, 'Reconnect GitHub account').length, 1);
});

test('connect runs the device flow: start, show code and URL, poll until complete, then reload', async () => {
  let polls = 0;
  const { host, calls, toasts, reloads } = setup({
    entries: [], put: () => { throw new Error('no save'); },
    identity: { connected: false, usable: false },
    api: call => {
      if (call.url === '/api/github-owner-identity/start') return { ok: true, json: async () => ({ user_code: 'ABCD-1234', verification_uri: 'https://github.com/login/device', expires_in: 900, interval: 0, flow_id: 'f1', scope: 'public_repo' }) };
      if (call.url === '/api/github-owner-identity/poll') {
        assert.deepStrictEqual(call.body, { flow_id: 'f1' });
        polls++;
        return { ok: true, json: async () => (polls < 3 ? { status: polls === 1 ? 'pending' : 'slow_down' } : { status: 'complete', login: 'nate-double-u' }) };
      }
      throw new Error('unexpected ' + call.url);
    },
  });
  await buttons(host, 'Connect GitHub account')[0].fire('click')[0];
  await new Promise(r => setImmediate(r));
  await new Promise(r => setImmediate(r));
  await new Promise(r => setImmediate(r));
  const status = host.find(n => String(n.className).includes('cfg-owner-identity-status'))[0];
  assert.strictEqual(polls, 3);
  assert.strictEqual(calls[0].opts.method, 'POST');
  assert.deepStrictEqual(toasts, [['GitHub account nate-double-u connected; write-tier reference repos are live', 'success']]);
  assert.strictEqual(reloads.length, 1, 'the section reloads after a completed consent');
  assert.ok(status, 'status line exists');
});

test('a refused consent shows GitHub\'s error and re-enables Connect; disconnect calls DELETE and reloads', async () => {
  let { host, toasts, reloads } = setup({
    entries: [], put: () => { throw new Error('no save'); },
    identity: { connected: false, usable: false },
    api: call => call.url.endsWith('/start')
      ? { ok: true, json: async () => ({ user_code: 'X', verification_uri: 'https://github.com/login/device', expires_in: 900, interval: 0, flow_id: 'f1' }) }
      : { ok: true, json: async () => ({ status: 'error', error: 'GitHub granted scope read:user; the hive needs public_repo' }) },
  });
  const connect = buttons(host, 'Connect GitHub account')[0];
  await connect.fire('click')[0];
  await new Promise(r => setImmediate(r));
  const status = host.find(n => String(n.className).includes('cfg-owner-identity-status'))[0];
  assert.match(status.textContent, /needs public_repo/);
  assert.ok(status.className.includes('text-danger'));
  assert.strictEqual(connect.disabled, false);
  assert.strictEqual(toasts.length, 0);
  assert.strictEqual(reloads.length, 0);

  ({ host, toasts, reloads } = setup({
    entries: [], put: () => { throw new Error('no save'); },
    identity: { connected: true, usable: true, login: 'nate-double-u' },
    api: call => { assert.strictEqual(call.opts.method, 'DELETE'); return { ok: true, json: async () => ({ status: 'disconnected' }) }; },
  }));
  await buttons(host, 'Disconnect')[0].fire('click')[0];
  assert.strictEqual(toasts.length, 1);
  assert.match(toasts[0][0], /disconnected/);
  assert.strictEqual(reloads.length, 1);
});

test('a hand-written write entry shows its credential login; a read-only one shows the plain tier', () => {
  const { host } = setup({
    entries: [
      { repo: 'hivecommons/hive', write: ['issues', 'prs'], credential_state: 'write-tier', credential_login: 'nate-double-u' },
      { repo: 'hivecommons/docs' },
      { repo: 'acme/docs', write: ['issues', 'prs'], derived_from: 'cncf/techdocs#395', credential_state: 'write-tier', credential_login: 'nate-double-u' },
    ],
    put: () => { throw new Error('no save'); },
    identity: { connected: true, usable: true, login: 'nate-double-u' },
  });
  const [hive, docs, acme] = rows(host);
  const chip = hive.find(n => String(n.className).includes('cfg-reference-repo-state'))[0];
  assert.strictEqual(chip.textContent, 'issues+prs \u00b7 write-tier as nate-double-u');
  assert.match(chip.title, /appear under nate-double-u/);
  assert.strictEqual(inputs(hive, 'checkbox').length, 2, 'hand-written rows stay editable');
  assert.strictEqual(docs.find(n => n.className === 'rt-inherit')[0].textContent, 'read-only');
  assert.strictEqual(acme.find(n => String(n.className).includes('cfg-reference-repo-state'))[0].textContent, 'issues+prs \u00b7 write-tier as nate-double-u');
});

// hivecommons/hive#18516: "Post on other projects as" offers the three
// values, marks the current one, PUTs a change and reloads; every write
// entry says which identity it will actually post as and why.
const identityChoices = host => host.find(n => String(n.className).includes('cfg-write-identity-choices'))[0].children;

test('write identity control: three choices, default bot, provenance line', () => {
  const { host } = setup({ entries: [], put: () => { throw new Error('no save'); } });
  const choices = identityChoices(host);
  assert.deepStrictEqual(choices.map(c => c.textContent), ['Hive bot, falling back to me', 'Hive bot only', 'Me']);
  assert.ok(choices[0].className.includes('btn-primary'), 'bot is the default when the server sends nothing');
  assert.ok(!choices[1].className.includes('btn-primary') && !choices[2].className.includes('btn-primary'));
  assert.match(host.textContent, /Post on other projects as:/);
  assert.match(host.textContent, /came from the hive you commissioned, not from you personally/);
  assert.match(host.textContent, /never asks anyone to install it/);
});

test('choosing an identity PUTs it, toasts and reloads; a refusal is shown and nothing reloads', async () => {
  let { host, calls, toasts, reloads } = setup({
    entries: [], meta: { write_identity: 'bot', write_identities: ['bot', 'bot_only', 'owner'], bot_login: 'hivecommons-hive[bot]' },
    put: call => { assert.strictEqual(call.url, '/api/config/reference-repos/write-identity'); assert.strictEqual(call.opts.method, 'PUT'); return { ok: true, json: async () => ({}) }; },
  });
  assert.match(host.textContent, /hive bot \(hivecommons-hive\[bot\]\)/);
  await identityChoices(host)[2].fire('click')[0];
  assert.deepStrictEqual(calls[0].body, { write_identity: 'owner' });
  assert.deepStrictEqual(toasts, [['Now posting on other projects as: Me', 'success']]);
  assert.strictEqual(reloads.length, 1);

  ({ host, calls, toasts, reloads } = setup({
    entries: [], meta: { write_identity: 'owner', write_identities: ['bot', 'bot_only', 'owner'] },
    put: () => ({ ok: false, status: 400, errorMessage: 'project.reference_repos acme/site: write: [issues] has no credential' }),
  }));
  await identityChoices(host)[1].fire('click')[0];
  assert.deepStrictEqual(calls[0].body, { write_identity: 'bot_only' });
  assert.strictEqual(reloads.length, 0);
  assert.match(host.find(n => String(n.className).includes('cfg-write-identity-status'))[0].textContent, /has no credential/);
  identityChoices(host).forEach(b => assert.strictEqual(b.disabled, false));
});

test('per-repo line says which identity posts there and why', () => {
  const entries = [
    { repo: 'hivecommons/docs', write: ['issues', 'prs'], derived_from: 'cncf/techdocs#395', credential_state: 'write-tier', credential_login: 'hivecommons-hive[bot]', credential_identity: 'bot' },
    { repo: 'acme/site', write: ['issues', 'prs'], derived_from: 'cncf/techdocs#395', credential_state: 'write-tier', credential_login: 'nate-double-u', credential_identity: 'owner' },
    { repo: 'zeta/site', derived_from: 'cncf/techdocs#395', credential_state: 'read-only', credential_reason: 'the hive owner has not connected a GitHub account' },
    { repo: 'hand/written', write: ['issues'], credential_state: 'write-tier', credential_login: 'hivecommons-hive[bot]', credential_identity: 'bot' },
    { repo: 'plain/readonly' },
  ];
  const line = row => (row.find(n => String(n.className).includes('cfg-reference-repo-identity'))[0] || {}).textContent;
  let { host } = setup({ entries, put: () => { throw new Error('no save'); }, meta: { write_identity: 'bot', write_identities: ['bot', 'bot_only', 'owner'] } });
  let r = rows(host);
  assert.strictEqual(line(r[0]), 'posts as hivecommons-hive[bot]: App installed on hivecommons');
  assert.strictEqual(line(r[1]), 'posts as nate-double-u: App not installed on acme');
  assert.strictEqual(line(r[2]), 'read-only: connect your GitHub account');
  assert.strictEqual(line(r[3]), 'posts as hivecommons-hive[bot]: App installed on hand');
  assert.strictEqual(line(r[4]), undefined, 'a plain read-only entry has no identity line');

  ({ host } = setup({ entries: [{ repo: 'acme/site', derived_from: 'cncf/techdocs#395', credential_state: 'read-only', credential_reason: 'x' }], put: () => { throw new Error('no save'); }, meta: { write_identity: 'bot_only', write_identities: ['bot', 'bot_only', 'owner'] } }));
  assert.strictEqual(line(rows(host)[0]), 'read-only: bot only, App not installed on acme');

  ({ host } = setup({ entries: [entries[1]], put: () => { throw new Error('no save'); }, meta: { write_identity: 'owner', write_identities: ['bot', 'bot_only', 'owner'] } }));
  assert.strictEqual(line(rows(host)[0]), 'posts as nate-double-u: set to post as you');
  assert.doesNotMatch(host.textContent, /@nate-double-u/, 'never an @-mention');
});
