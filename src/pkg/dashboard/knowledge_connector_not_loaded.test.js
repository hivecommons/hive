// Knowledge Integrations connector card when the backend syncer is not running
// (hivecommons/hive#18093): the status response's `runtime: false` turns the
// `pending` badge into a distinct `not loaded` badge with a one-line hint and
// a docs link, and disables Connect / Check / Sync with the same hint. A loaded
// connector awaiting sign-in keeps `pending`; a disabled entry keeps `disabled`.
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
// The two string constants next to the helpers, lifted from the source so the
// test does not duplicate the badge text or hint.
const constSrc = name => {
  const m = html.match(new RegExp('const ' + name + " = '([^']*)';"));
  assert.ok(m, name + ' not found');
  return m[1];
};

const NOT_LOADED = constSrc('KC_STATUS_NOT_LOADED');
const HINT = constSrc('KC_NOT_LOADED_HINT');
const OWNER = true;

const render = ({ runtime, c, row, type = 'plaud', owner = OWNER }) => {
  const calls = { authLines: 0 };
  const escapeHtml = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;');
  const h = new Function('_kcState', '_kbPlaudAuth', 'escapeHtml', 'kcAgo', 'kbPlaudAuthLine', 'kbPlaudPasteHtml', 'kbWisprFlowSourceLine',
    fnSrc('kbConnectorNotLoaded') + '\n' + fnSrc('kbConnectorNotLoadedHtml') + '\n' + fnSrc('kbPlaudAuthButtons') + '\n' + fnSrc('kbConnectorRow') +
    "\nconst KC_STATUS_NOT_LOADED = '" + NOT_LOADED + "'; const KC_NOT_LOADED_HINT = '" + HINT + "';" +
    '\nreturn kbConnectorRow;')(
    { runtime, rows: row ? [row] : [], publish: null }, {}, escapeHtml, () => '—',
    () => { calls.authLines++; return '<div data-kb-plaud-auth></div>'; }, () => '', () => '');
  return { html: h(type, c, owner), calls };
};

const badge = out => { const m = out.match(/<span class="kc-pill" data-status="([^"]*)">([^<]*)<\/span>/); assert.ok(m, 'badge missing'); assert.strictEqual(m[1], m[2]); return m[1]; };
const buttons = out => [...out.matchAll(/<button [^>]*data-action="([^"]+)"[^>]*>/g)].map(m => ({ action: m[1], disabled: / disabled\b/.test(m[0]), title: (m[0].match(/title="([^"]*)"/) || [])[1] }));

test('runtime:false shows a not-loaded badge, the hint and a docs link instead of pending', () => {
  const c = { name: 'plaud-voice', type: 'plaud', enabled: true };
  const { html: out, calls } = render({ runtime: false, c, row: { name: 'plaud-voice', status: 'pending' } });
  assert.strictEqual(badge(out), NOT_LOADED);
  assert.ok(!out.includes('>pending<'), 'pending badge must not appear');
  assert.ok(out.includes('data-kb-not-loaded'), 'hint line rendered');
  assert.ok(out.includes(HINT), 'hint text rendered');
  assert.ok(out.includes('href="/docs/knowledge-connectors.md"'), 'docs link rendered');
  assert.strictEqual(calls.authLines, 0, 'the Plaud auth probe (which would 404) is not rendered');
});

test('runtime:false disables Connect, Check and Sync and explains why in the title', () => {
  const c = { name: 'plaud-voice', type: 'plaud', enabled: true };
  const { html: out } = render({ runtime: false, c, row: null });
  const b = buttons(out);
  const byAction = Object.fromEntries(b.map(x => [x.action, x]));
  for (const action of ['kbPlaudConnect', 'kbPlaudRefreshAuth', 'kbIntegrationSync']) {
    assert.ok(byAction[action], action + ' rendered');
    assert.ok(byAction[action].disabled, action + ' disabled');
    assert.strictEqual(byAction[action].title, HINT, action + ' carries the hint');
  }
  assert.ok(!byAction.kbIntegrationEdit.disabled, 'Edit stays enabled');
  assert.ok(!byAction.kbIntegrationToggle.disabled, 'Enable/Disable stays enabled');
});

test('runtime:true keeps pending for a loaded connector awaiting sign-in, with live buttons', () => {
  const c = { name: 'plaud-voice', type: 'plaud', enabled: true };
  const { html: out, calls } = render({ runtime: true, c, row: { name: 'plaud-voice', status: 'pending' } });
  assert.strictEqual(badge(out), 'pending');
  assert.ok(!out.includes('data-kb-not-loaded'));
  assert.strictEqual(calls.authLines, 1);
  const b = buttons(out);
  assert.ok(b.some(x => x.action === 'kbPlaudRefreshAuth' && !x.disabled), 'Check enabled');
  assert.ok(b.some(x => x.action === 'kbIntegrationSync' && !x.disabled), 'Sync enabled');
});

test('a disabled entry keeps the disabled badge even when the runtime is not loaded', () => {
  const c = { name: 'wiki', type: 'confluence', enabled: false };
  const { html: out } = render({ runtime: false, c, row: { name: 'wiki', status: 'disabled' }, type: 'confluence' });
  assert.strictEqual(badge(out), 'disabled');
  assert.ok(!out.includes('data-kb-not-loaded'));
});

test('non-owner view still shows the not-loaded badge and hint without action buttons', () => {
  const c = { name: 'wiki', type: 'confluence', enabled: true };
  const { html: out } = render({ runtime: false, c, row: null, type: 'confluence', owner: false });
  assert.strictEqual(badge(out), NOT_LOADED);
  assert.ok(out.includes('data-kb-not-loaded'));
  assert.strictEqual(buttons(out).length, 0);
});
