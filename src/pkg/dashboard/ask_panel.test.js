// Ask Hive help panel view model (#19184). The shared component in
// pkg/askai/web renders only what askViewModel returns, with textContent, so
// this pins what reaches the page: answers need an https source, everything
// else becomes a notice with the docs link.
const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');

const PANEL_PATH = path.join(__dirname, '..', 'askai', 'web', 'ask-panel.js');
const { askViewModel, answerBlocks, inlineTokens, sourceTitle, citationIndex } = require(PANEL_PATH);

test('an answer keeps only https sources and names their host', () => {
  const v = askViewModel({
    outcome: 'ok', answer: 'Open Settings.', uncertain: true, conversation: 'h1', docs_url: 'https://docs.hivecommons.dev/',
    sources: [{ title: 'Settings', url: 'https://docs.hivecommons.dev/settings/' }, { title: 'x', url: 'javascript:alert(1)' }, { url: 'http://insecure.example/' }],
  });
  assert.equal(v.kind, 'answer');
  assert.equal(v.text, 'Open Settings.');
  assert.equal(v.uncertain, true);
  assert.equal(v.conversation, 'h1');
  assert.deepEqual(v.sources, [{ label: 'Settings', page: 'Settings', section: '', host: 'docs.hivecommons.dev', href: 'https://docs.hivecommons.dev/settings/' }]);
});

test('an answer with no usable source is shown as no cited answer', () => {
  const v = askViewModel({ outcome: 'ok', answer: 'Trust me.', sources: [{ url: 'ftp://x' }] });
  assert.equal(v.kind, 'notice');
  assert.match(v.text, /No cited answer/);
  assert.equal(v.uncertain, false);
  assert.deepEqual(v.sources, []);
});

test('every other outcome is a notice carrying the backend message', () => {
  for (const outcome of ['rate_limited', 'breaker_open', 'upstream_error', 'refused_secret_shape', 'not_consented', 'relay_disabled']) {
    const v = askViewModel({ outcome, message: 'msg ' + outcome, retry_after_seconds: 7, answer: '<b>ignored</b>' });
    assert.equal(v.kind, 'notice');
    assert.equal(v.text, 'msg ' + outcome);
    assert.deepEqual(v.sources, []);
  }
  assert.equal(askViewModel({ outcome: 'rate_limited', retry_after_seconds: 7 }).retryAfter, 7);
  assert.equal(askViewModel(null).kind, 'notice');
  assert.match(askViewModel(undefined).text, /unavailable/);
});

// ── Answer markdown, citations and sources ───────────────────────────────
// A minimal DOM, enough for the panel to build itself and render answers.
function fakeDom() {
  const byId = {};
  class Node {
    constructor(tag, text) {
      this.tagName = tag; this.children = []; this.attrs = {}; this.listeners = {}; this.parentNode = null;
      this.hidden = false; this.className = ''; this._text = text || '';
      this.style = {}; this.scrollTop = 0; this.clientHeight = 0; this.scrollHeight = 0;
    }
    appendChild(c) { c.parentNode = this; this.children.push(c); if (c.attrs && c.attrs.id) byId[c.attrs.id] = c; return c; }
    removeChild(c) { this.children = this.children.filter(x => x !== c); c.parentNode = null; return c; }
    setAttribute(k, v) { this.attrs[k] = String(v); if (k === 'id') byId[v] = this; }
    getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
    removeAttribute(k) { delete this.attrs[k]; }
    addEventListener(t, fn) { (this.listeners[t] = this.listeners[t] || []).push(fn); }
    fire(t, ev) { (this.listeners[t] || []).forEach(fn => fn(Object.assign({ preventDefault() {}, stopPropagation() {} }, ev))); }
    focus() { doc.activeElement = this; }
    scrollIntoView(opts) { this.scrolled = opts; }
    get textContent() { return this.tagName === '#text' ? this._text : this._text + this.children.map(c => c.textContent).join(''); }
    set textContent(v) { this._text = String(v); this.children = []; }
    set innerHTML(v) { throw new Error('innerHTML must never be used'); }
    get href() { return this.attrs.href; }
    set href(v) { this.attrs.href = v; }
    get title() { return this.attrs.title; }
    set title(v) { this.attrs.title = v; }
    all() { return [this].concat(...this.children.map(c => c.all())); }
  }
  const classes = new Set();
  const doc = {
    activeElement: null,
    body: new Node('body'),
    head: new Node('head'),
    documentElement: { classList: { toggle(n, on) { if (on) classes.add(n); else classes.delete(n); }, contains: n => classes.has(n) } },
    createElement: tag => new Node(tag),
    createTextNode: t => new Node('#text', t),
    getElementById: id => byId[id] || null,
  };
  return { doc, byId, Node };
}

function withDom(fn) {
  const dom = fakeDom();
  global.document = dom.doc;
  try { return fn(dom); } finally { delete global.document; }
}

const SOURCES = [
  { label: 'Hive activity monitor \u2014 FAQ', page: 'Hive activity monitor \u2014 FAQ', section: '', host: 'docs.hivecommons.dev', href: 'https://docs.hivecommons.dev/activity-monitor/' },
  { label: 'Agent Configuration › Changing backend', page: 'Agent Configuration', section: 'Changing backend', host: 'docs.hivecommons.dev', href: 'https://docs.hivecommons.dev/agent-configuration/#changing-backend' },
];

test('blocks: headings, paragraphs, nested and loose lists, tables, quotes and fenced code', () => {
  const blocks = answerBlocks('## Setup\nOpen Settings.\nThen save.\n\n- one\n  - nested\n- two\n\n1. first\n\n2. second\n\n| key | meaning |\n|---|---|\n| `a` | b |\n\n> note\n\n```yaml\nkey: <b>v</b>\n\n  nested: 1\n```\nDone.');
  assert.deepEqual(blocks, [
    { type: 'h', level: 2, text: 'Setup' },
    { type: 'p', text: 'Open Settings. Then save.' },
    { type: 'list', ordered: false, start: 1, items: [{ depth: 0, ordered: false, text: 'one' }, { depth: 1, ordered: false, text: 'nested' }, { depth: 0, ordered: false, text: 'two' }] },
    { type: 'list', ordered: true, start: 1, items: [{ depth: 0, ordered: true, text: 'first' }, { depth: 0, ordered: true, text: 'second' }] },
    { type: 'table', head: ['key', 'meaning'], rows: [['`a`', 'b']] },
    { type: 'quote', text: 'note' },
    { type: 'code', text: 'key: <b>v</b>\n\n  nested: 1' },
    { type: 'p', text: 'Done.' },
  ]);
  assert.deepEqual(answerBlocks('```\nx = 1'), [{ type: 'code', text: 'x = 1' }]);
  assert.deepEqual(answerBlocks(''), []);
  assert.deepEqual(answerBlocks(null), []);
});

test('inline: code, bold, italics, https links only, javascript: dropped to its label', () => {
  assert.deepEqual(inlineTokens('Use `backend: copilot` and **save** it, *now*.'), [
    { type: 'text', text: 'Use ' }, { type: 'code', text: 'backend: copilot' }, { type: 'text', text: ' and ' },
    { type: 'strong', children: [{ type: 'text', text: 'save' }] }, { type: 'text', text: ' it, ' },
    { type: 'em', children: [{ type: 'text', text: 'now' }] }, { type: 'text', text: '.' },
  ]);
  assert.deepEqual(inlineTokens('[docs](https://docs.hivecommons.dev/a_(b))'), [{ type: 'link', href: 'https://docs.hivecommons.dev/a_(b)', children: [{ type: 'text', text: 'docs' }] }]);
  for (const bad of ['javascript:alert(1)', 'data:text/html,x', 'http://insecure.example/', '/relative']) {
    assert.deepEqual(inlineTokens('[click](' + bad + ')'), [{ type: 'text', text: 'click' }], bad);
  }
  assert.deepEqual(inlineTokens('snake_case_name stays'), [{ type: 'text', text: 'snake_case_name stays' }]);
});

test('source titles split on | into page and section', () => {
  assert.deepEqual(sourceTitle('Agent Configuration|Changing backend and model together: use the atomic endpoint'), {
    page: 'Agent Configuration', section: 'Changing backend and model together: use the atomic endpoint',
    label: 'Agent Configuration › Changing backend and model together: use the atomic endpoint',
  });
  assert.deepEqual(sourceTitle('Hive activity monitor \u2014 FAQ'), { page: 'Hive activity monitor \u2014 FAQ', section: '', label: 'Hive activity monitor \u2014 FAQ' });
  assert.equal(sourceTitle(' a | b | c ').label, 'a › b › c');
  const v = askViewModel({ outcome: 'ok', answer: 'x', sources: [{ title: 'Dashboard settings explained|Agents', url: 'https://github.com/hivecommons/hive/blob/v5/docs/settings.md' }] });
  assert.equal(v.sources[0].label, 'Dashboard settings explained › Agents');
  assert.equal(v.sources[0].host, 'github.com');
});

test('citation markers map to sources by name or number, and unknown ones are dropped', () => {
  assert.equal(citationIndex('activity monitor', SOURCES), 0);
  assert.equal(citationIndex('agent configuration', SOURCES), 1);
  assert.equal(citationIndex('Changing backend', SOURCES), 1);
  assert.equal(citationIndex('2', SOURCES), 1);
  assert.equal(citationIndex('9', SOURCES), -1);
  assert.equal(citationIndex('inference backends', SOURCES), -1);
  const toks = inlineTokens('Set the backend [agent configuration]. See logs [inference backends].', SOURCES);
  assert.deepEqual(toks, [
    { type: 'text', text: 'Set the backend' },
    { type: 'cite', n: 2, href: SOURCES[1].href, title: SOURCES[1].label },
    { type: 'text', text: '. See logs.' },
  ]);
});

test('rendered markdown has headings, a code block and a table, and injection stays text', () => withDom(({ Node }) => {
  const { renderAnswer } = require(PANEL_PATH);
  const md = '## Switching backend\n\nRun it [agent configuration] <script>alert(1)</script> <img src=x onerror=alert(1)>\n\n### 1. Set the backend\n\n```yaml\nbackend: copilot\n```\n\n| `seat.state` | Meaning |\n|---|---|\n| active | ok |\n\n[bad](javascript:alert(1)) [good](https://docs.hivecommons.dev/x)';
  const box = renderAnswer(md, SOURCES);
  const nodes = box.all();
  const tags = nodes.map(n => n.tagName);
  for (const want of ['h3', 'h4', 'pre', 'code', 'table', 'thead', 'th', 'td', 'sup']) assert.ok(tags.includes(want), 'missing ' + want);
  for (const banned of ['script', 'img', 'iframe']) assert.ok(!tags.includes(banned), banned + ' element built from answer text');
  assert.match(box.textContent, /<script>alert\(1\)<\/script>/, 'markup in an answer shows as literal text');
  assert.ok(!/\[agent configuration\]|\[inference/.test(box.textContent), 'no literal citation brackets');
  const links = nodes.filter(n => n.tagName === 'a');
  assert.ok(links.length >= 2);
  for (const a of links) {
    assert.match(a.attrs.href, /^https:\/\//);
    assert.equal(a.attrs.rel, 'noopener noreferrer');
    assert.equal(a.attrs.target, '_blank');
  }
  assert.ok(nodes.every(n => !Object.keys(n.attrs).some(k => /^on/i.test(k))), 'no event-handler attributes');
}));

test('the status region announces "Answer received." and never holds the answer body', async () => {
  const answer = '## Switching an Agent to the Copilot Backend\n\nSet `backend: copilot` [agent configuration].\n\n```yaml\nbackend: copilot\n```';
  const dom = fakeDom();
  global.document = dom.doc;
  global.window = { matchMedia: () => ({ matches: false }) };
  global.fetch = async (url, init) => {
    if (url.endsWith('/status')) return { ok: true, json: async () => ({ enabled: true, consented: true, disclosure: 'n', docs_url: 'https://docs.hivecommons.dev/', text_version: '1' }) };
    return { ok: true, status: 200, json: async () => ({ outcome: 'ok', answer, sources: [{ title: 'Agent Configuration|Changing backend', url: 'https://docs.hivecommons.dev/agent-configuration/' }] }) };
  };
  try {
    const slot = dom.doc.createElement('div');
    slot.setAttribute('id', 'slot');
    const { mount } = require(PANEL_PATH);
    const panel = await mount({ slot, base: '/api/ask' });
    assert.ok(panel, 'panel built');
    const all = () => panel.all();
    const input = dom.byId['hive-ask-input'];
    const send = all().find(n => n.className && n.className.split(' ').includes('hive-ask-send'));
    const live = all().find(n => n.attrs.role === 'status');
    input.value = 'how do I configure agents to use copilot?';
    const form = all().find(n => n.tagName === 'form');
    form.fire('submit');
    assert.equal(send.disabled, true, 'Ask is disabled while waiting');
    assert.equal(input.readOnly, true, 'the question is read-only while waiting');
    assert.match(send.textContent, /Asking\.\.\./);
    assert.match(live.textContent, /up to 30 seconds/);
    for (let i = 0; i < 10; i++) await new Promise(r => setImmediate(r));
    assert.equal(live.textContent, 'Answer received.');
    assert.equal(send.disabled, false, 'Ask is enabled again');
    assert.equal(input.readOnly, false);
    assert.equal(send.textContent, 'Ask');
    const withBody = all().filter(n => n.tagName !== '#text' && n._text.includes('Switching an Agent'));
    assert.equal(withBody.length, 0, 'no element holds the raw answer text as its own text');
    const headings = all().filter(n => n.tagName === 'h3');
    assert.equal(headings.length, 1, 'the answer is rendered exactly once');
    assert.ok(!live.textContent.includes('copilot'));
    const entry = all().find(n => n.className === 'hive-ask-entry');
    assert.deepEqual(entry.scrolled, { block: 'start' }, 'a new answer scrolls to its top');
    // Answer, then sources, then one disclaimer line.
    const parts = entry.children[1].children.map(n => n.className);
    assert.deepEqual(parts, ['hive-ask-text hive-ask-md', 'hive-ask-sources-block', 'hive-ask-ai']);
  } finally {
    delete global.document; delete global.window; delete global.fetch;
  }
});

test('a failed question shows Retry and re-enables the form', async () => {
  const dom = fakeDom();
  global.document = dom.doc;
  global.window = { matchMedia: () => ({ matches: false }) };
  let calls = 0;
  global.fetch = async (url) => {
    if (url.endsWith('/status')) return { ok: true, json: async () => ({ enabled: true, consented: true, docs_url: 'https://docs.hivecommons.dev/' }) };
    calls++;
    return { ok: false, status: 502, json: async () => ({}) };
  };
  try {
    const slot = dom.doc.createElement('div');
    const { mount } = require(PANEL_PATH);
    const panel = await mount({ slot });
    const input = dom.byId['hive-ask-input'];
    input.value = 'q';
    panel.all().find(n => n.tagName === 'form').fire('submit');
    for (let i = 0; i < 10; i++) await new Promise(r => setImmediate(r));
    const send = panel.all().find(n => n.className === 'hive-ask-send');
    assert.equal(send.disabled, false, 'never left disabled after an error');
    const retry = panel.all().find(n => n.className === 'hive-ask-retry');
    assert.ok(retry, 'an inline Retry button');
    retry.fire('click');
    assert.equal(calls, 2, 'Retry asks again');
    for (let i = 0; i < 10; i++) await new Promise(r => setImmediate(r));
  } finally {
    delete global.document; delete global.window; delete global.fetch;
  }
});

test('opening locks page scroll and closing (button or Escape) unlocks it', async () => {
  const dom = fakeDom();
  global.document = dom.doc;
  global.window = { matchMedia: () => ({ matches: true }) };
  global.fetch = async () => ({ ok: true, json: async () => ({ enabled: true, consented: false, docs_url: 'https://docs.hivecommons.dev/' }) });
  try {
    const slot = dom.doc.createElement('div');
    const api = require(PANEL_PATH);
    const panel = await api.mount({ slot });
    const cl = dom.doc.documentElement.classList;
    const opener = slot.children[0];
    opener.fire('click');
    assert.ok(cl.contains('hive-ask-scroll-lock'), 'open adds the scroll lock');
    panel.fire('keydown', { key: 'Escape' });
    assert.ok(!cl.contains('hive-ask-scroll-lock'), 'Escape removes it');
    opener.fire('click');
    assert.ok(cl.contains('hive-ask-scroll-lock'));
    panel.all().find(n => n.className === 'hive-ask-close').fire('click');
    assert.ok(!cl.contains('hive-ask-scroll-lock'), 'close removes it');
    // At the top of the panel, a downward drag on the sheet is not passed on.
    let prevented = false;
    let stopped = false;
    panel.fire('touchstart', { touches: [{ clientX: 0, clientY: 0 }] });
    panel.fire('touchmove', { touches: [{ clientX: 0, clientY: 40 }], cancelable: true, preventDefault() { prevented = true; }, stopPropagation() { stopped = true; } });
    assert.ok(prevented && stopped, 'touch scroll past the top is stopped at the panel');
  } finally {
    delete global.document; delete global.window; delete global.fetch;
  }
});
