// Settings -> Forge App -> Personal access token (hivecommons/hive#19633):
// renderGitHubTokenPanel paints a masked, write-only field over the owner-only
// /api/github-token routes. It shows the fingerprint, login and last
// verification, never the token, is absent on a GitHub App hive, and shows the
// server's 422/429/409 reason while keeping what was typed out of the page.
// Run: node --test src/pkg/dashboard/*.test.js
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
const fnSrc = name => {
  let start = html.indexOf('function ' + name + '(');
  assert.ok(start >= 0, name + ' not found');
  if (html.slice(start - 6, start) === 'async ') start -= 6;
  let depth = 0;
  for (let i = html.indexOf('{', start); i < html.length; i++) {
    if (html[i] === '{') depth++;
    else if (html[i] === '}' && --depth === 0) return html.slice(start, i + 1);
  }
  throw new Error('unbalanced ' + name);
};

const makeEl = tag => {
  const el = { tag, children: [], listeners: {}, className: '', title: '', _text: '', attrs: {}, value: '' };
  Object.defineProperty(el, 'textContent', {
    get() { return el._text + el.children.map(c => c.textContent).join(''); },
    set(v) { el._text = String(v); el.children = []; },
  });
  el.appendChild = c => { el.children.push(c); return c; };
  el.setAttribute = (k, v) => { el.attrs[k] = v; };
  el.addEventListener = (type, fn) => { (el.listeners[type] = el.listeners[type] || []).push(fn); };
  el.fire = type => Promise.all((el.listeners[type] || []).map(fn => fn({})));
  el.find = pred => {
    const out = [];
    const walk = n => { if (pred(n)) out.push(n); (n.children || []).forEach(walk); };
    walk(el);
    return out;
  };
  return el;
};

const setup = ({ status, put }) => {
  const toasts = [];
  const calls = [];
  let reloads = 0;
  const doc = { createElement: makeEl };
  const fetchStub = async (url, opts) => { calls.push({ url, opts, body: JSON.parse(opts.body) }); return put(); };
  const render = new Function('document', 'fetch', 'showToast', '_inceptionAuthHeaders', 'saveErrorMessage',
    fnSrc('referenceRepoNode') + '\n' + fnSrc('renderGitHubTokenPanel') + '\nreturn renderGitHubTokenPanel;')(
    doc, fetchStub, (m, k) => toasts.push([m, k]), () => ({}), async res => res.errorMessage);
  const host = makeEl('div');
  render(host, status, () => { reloads++; });
  return { host, toasts, calls, reloads: () => reloads };
};
const input = host => host.find(n => n.tag === 'input')[0];
const button = host => host.find(n => n.tag === 'button')[0];
const statusLine = host => host.find(n => String(n.className).includes('cfg-github-token-status'))[0];

test('a PAT hive shows fingerprint, login and last verified, with a masked input', () => {
  const { host } = setup({ status: { mode: 'pat', configured: true, fingerprint: 'abc123def456', login: 'octo', verified_at: '2026-01-02T03:04:05Z' }, put: () => { throw new Error('no save'); } });
  const state = host.find(n => String(n.className).includes('cfg-github-token-state'))[0].textContent;
  assert.match(state, /abc123def456/);
  assert.match(state, /octo/);
  assert.match(state, /last verified/);
  assert.strictEqual(input(host).type, 'password');
  assert.strictEqual(input(host).autocomplete, 'new-password');
});

test('an App hive draws nothing', () => {
  const { host } = setup({ status: { mode: 'app' }, put: () => { throw new Error('no save'); } });
  assert.strictEqual(host.children.length, 0);
});

test('Verify and save PUTs the token once, clears the input and reloads', async () => {
  const { host, calls, toasts, reloads } = setup({
    status: { mode: 'pat', configured: true, fingerprint: 'aaaaaaaaaaaa' },
    put: () => ({ ok: true, json: async () => ({ mode: 'pat', login: 'octo', sealed: true }) }),
  });
  input(host).value = '  ghp_new  ';
  await button(host).fire('click');
  assert.strictEqual(calls.length, 1);
  assert.strictEqual(calls[0].url, '/api/github-token');
  assert.strictEqual(calls[0].opts.method, 'PUT');
  assert.deepStrictEqual(calls[0].body, { token: 'ghp_new' });
  assert.strictEqual(input(host).value, '');
  assert.strictEqual(toasts[0][1], 'success');
  assert.doesNotMatch(toasts[0][0], /ghp_new/);
  assert.strictEqual(reloads(), 1);
});

for (const [code, msg] of [[422, 'the new token was not saved: bad credentials'], [429, 'a token was rejected recently; retry in 15 seconds'], [409, 'this hive authenticates as a GitHub App']]) {
  test('a ' + code + ' shows the server reason, re-enables the button and does not reload', async () => {
    const { host, toasts, reloads } = setup({
      status: { mode: 'pat', configured: false },
      put: () => ({ ok: false, status: code, errorMessage: msg }),
    });
    input(host).value = 'ghp_bad';
    await button(host).fire('click');
    assert.strictEqual(statusLine(host).textContent, msg);
    assert.match(statusLine(host).className, /text-danger/);
    assert.strictEqual(button(host).disabled, false);
    assert.strictEqual(reloads(), 0);
    assert.strictEqual(toasts.length, 0);
  });
}

test('an empty input is refused client-side without a request', async () => {
  const { host, calls } = setup({ status: { mode: 'pat', configured: false }, put: () => { throw new Error('no save'); } });
  await button(host).fire('click');
  assert.strictEqual(calls.length, 0);
  assert.match(statusLine(host).textContent, /Enter the new token/);
});
