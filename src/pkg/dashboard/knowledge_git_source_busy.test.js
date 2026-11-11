// Knowledge Sources "Clone & Index" (hivecommons/hive#17610): the button is
// found through its data-action attribute so the busy state actually shows,
// a blank branch is sent blank (the server resolves the repo default), and a
// failed connect is kept as an error row instead of only an alert.
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

const setup = ({ fields, response }) => {
  const btn = { disabled: false, textContent: 'Clone & Index' };
  const inputs = {};
  for (const [id, value] of Object.entries(fields)) inputs[id] = { value };
  const selectors = [];
  const document = {
    getElementById: id => inputs[id] || null,
    querySelector: sel => { selectors.push(sel); return sel === '#kb-subs-content [data-action="kbAddGitSource"]' ? btn : null; },
  };
  const calls = { bodies: [], alerts: [], reloads: 0, stats: 0, fieldErrors: [] };
  let release;
  const gate = new Promise(r => { release = r; });
  const fetch = async (url, opts) => {
    calls.bodies.push(JSON.parse(opts.body));
    calls.busyDuringFetch = { disabled: btn.disabled, text: btn.textContent };
    await gate;
    return response;
  };
  // v6 keeps one failed-connect row (_kbGitSourceFailed) and relabels the
  // button through kbGitSourceSubmitLabel so an edit reads "Save".
  const h = new Function('document', 'fetch', '_inceptionAuthHeaders', 'hiveAlert', 'kbLoadSubscriptions', 'fetchKnowledgeStats', 'kbFieldError',
    'var _kbGitSourceFailed = null, _kbGitSourceEditing = null;\n' +
    'function kbGitSourceSubmitLabel() { return _kbGitSourceEditing ? "Save" : "Clone & Index"; }\n' +
    'async ' + fnSrc('kbAddGitSource') +
    '\nreturn { kbAddGitSource, failed: () => _kbGitSourceFailed };')(
    document, fetch, () => ({}), async m => { calls.alerts.push(m); }, () => { calls.reloads++; }, () => { calls.stats++; },
    (el, m) => { calls.fieldErrors.push(m); });
  return { h, btn, calls, selectors, release };
};

const fields = { 'kb-gs-url': 'https://github.com/org/repo', 'kb-gs-name': 'docs', 'kb-gs-subpath': '', 'kb-gs-branch': '', 'kb-gs-layer': 'project' };

test('Clone & Index shows the busy state while cloning and restores it on success', async () => {
  const { h, btn, calls, selectors, release } = setup({ fields, response: { ok: true, json: async () => ({ ok: true }) } });
  const p = h.kbAddGitSource();
  await new Promise(r => setImmediate(r));
  assert.deepStrictEqual(selectors, ['#kb-subs-content [data-action="kbAddGitSource"]']);
  assert.deepStrictEqual(calls.busyDuringFetch, { disabled: true, text: 'Cloning…' });
  release();
  await p;
  assert.strictEqual(btn.disabled, false);
  assert.strictEqual(btn.textContent, 'Clone & Index');
  assert.strictEqual(calls.bodies[0].branch, '', 'blank branch must not be guessed as main');
  assert.strictEqual(h.failed(), null);
  assert.strictEqual(calls.stats, 1);
});

test('a failed connect restores the button and leaves an error row with the reason', async () => {
  const { h, btn, calls, release } = setup({
    fields,
    response: { ok: false, status: 500, json: async () => ({ error: 'git clone: exit status 128' }) },
  });
  const p = h.kbAddGitSource();
  release();
  await p;
  assert.strictEqual(btn.disabled, false);
  assert.strictEqual(btn.textContent, 'Clone & Index');
  assert.deepStrictEqual(h.failed(), {
    url: 'https://github.com/org/repo', name: 'docs', subpath: '', branch: '', layer: 'project', error: 'git clone: exit status 128', editing: null,
  });
  assert.strictEqual(calls.reloads, 1);
  assert.match(calls.alerts[0], /exit status 128/);
});

test('the import button is looked up by data-action, not a stale onclick selector', () => {
  const src = fnSrc('kbImportDocFromModal');
  assert.ok(src.includes('[data-action="kbImportDocFromModal"]'));
  assert.ok(!html.includes('onclick="kbAddGitSource()"') && !html.includes('onclick="kbImportDocFromModal()"'));
});
