// Inbox access (hivecommons/hive#18747): the input-methods row ("works
// today" and "coming soon" chips that only explain), the Quick note box in
// ＋ Add, and the hit-target CSS. Functions are extracted from the generated
// page and run in a sandbox with the browser pieces they touch injected.
// Run: node --test src/pkg/dashboard/*.test.js
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
const css = fs.readFileSync(path.join(__dirname, 'static-src', 'css', 'inbox-access.css'), 'utf8');
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
const lineSrc = prefix => {
  const at = html.indexOf(prefix);
  assert.ok(at >= 0, prefix + ' not found');
  return html.slice(at, html.indexOf('\n', at));
};
const escapeHtml = s => String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');

const METHODS = [
  { id: 'talon', name: 'Talon', kind: 'voice', state: 'works_today', featured: true, url: 'https://talonvoice.com/', note: 'Voice dictation and commands.' },
  { id: 'nerd-dictation', name: 'nerd-dictation', kind: 'voice', state: 'works_today', url: 'https://example.org/nd', note: 'Offline dictation.' },
  { id: 'neuralink', name: 'Neuralink', kind: 'neural', state: 'coming_soon', featured: true, url: 'https://neuralink.com/', note: 'Implanted interface in clinical trials.' },
  { id: 'synchron', name: 'Synchron', kind: 'neural', state: 'coming_soon', featured: true, url: 'javascript:alert(1)', note: 'Implanted interface in clinical trials.' },
  { id: 'odd', name: 'Odd', kind: 'neural', state: 'connected', featured: true, url: 'https://example.org/', note: 'Unknown state.' },
];

// ── Input methods ──
const INPUT_FNS = ['inboxInputMethods', 'inboxInputLink', 'inboxInputChipHtml', 'inboxInputNoteText', 'inboxInputNoteHtml', 'inboxInputMethodsHtml',
  'inboxInputSync', 'inboxInputExplain', 'inboxInputClose', 'inboxInputOnFocus'];
const inputHarness = (methods = METHODS) => {
  const calls = { fetches: 0, renders: 0, opened: 0 };
  const chips = methods.map(m => {
    const attrs = { 'data-input-id': m.id, 'aria-expanded': 'false' };
    return { attrs, getAttribute: k => attrs[k], setAttribute: (k, v) => { attrs[k] = v; }, closest: sel => (sel === '.inbox-input-chip' ? chips.find(c => c.attrs === attrs) : null) };
  });
  const note = { html: '', set outerHTML(v) { this.html = v; } };
  const row = { querySelectorAll: sel => (sel === '.inbox-input-chip' ? chips : []) };
  const document = { getElementById: id => (id === 'inbox-inputs' ? row : id === 'inbox-input-note' ? note : null) };
  const api = new Function('_inboxCache', 'document', 'escapeHtml', 'calls',
    'const fetch = () => { calls.fetches++; }; const renderInbox = () => { calls.renders++; }; const window = { open: () => { calls.opened++; } };\n' +
    ['INBOX_INPUT_DOCS_URL', 'INBOX_INPUT_WORKS', 'INBOX_INPUT_SOON', 'INBOX_INPUT_GROUP_LABELS'].map(n => lineSrc('const ' + n + ' =')).join('\n') + '\n' +
    'let _inboxInputOpen = "";\n' + INPUT_FNS.map(fnSrc).join('\n') +
    '\nreturn { ' + INPUT_FNS.join(', ') + ', open: () => _inboxInputOpen, docs: INBOX_INPUT_DOCS_URL };')({ input_methods: methods }, document, escapeHtml, calls);
  return { api, calls, chips, note };
};

test('the row lists featured entries in two labelled groups, after the sources, with a link to the full list', () => {
  const { api } = inputHarness();
  const row = api.inboxInputMethodsHtml();
  assert.match(row, /Works today:<\/span> <button[^>]*data-input-id="talon"/);
  assert.match(row, /Neural input \(coming soon\):<\/span> <button[^>]*data-input-id="neuralink"/);
  assert.ok(row.indexOf('Works today') < row.indexOf('Neural input (coming soon)'));
  // Not featured, or in a state the page does not know: left out of the row.
  assert.ok(!row.includes('data-input-id="nerd-dictation"') && !row.includes('data-input-id="odd"'));
  assert.match(row, /aria-label="Input methods \(not connected sources\)"/);
  assert.ok(row.includes('href="' + api.docs + '"') && />more<\/a>/.test(row));
  assert.match(api.docs, /^https:\/\/github\.com\/hivecommons\/hive\/blob\/v5\/src\/docs\/inbox-input-methods\.md$/);
  assert.ok(fs.existsSync(path.join(__dirname, '..', '..', 'docs', 'inbox-input-methods.md')), 'the docs page the row links to exists');
  // Rendered after the real sources, outside the sources row.
  const render = fnSrc('renderInbox');
  assert.ok(render.indexOf('inbox-sources') < render.indexOf('inboxInputMethodsHtml()'));
});

test('no catalogue, or nothing featured: no row', () => {
  assert.strictEqual(inputHarness([]).api.inboxInputMethodsHtml(), '');
  assert.strictEqual(inputHarness([METHODS[1]]).api.inboxInputMethodsHtml(), '');
});

test('a chip is a quiet, labelled button: plain-text name, no logo, no status colour, no connect control', () => {
  const { api } = inputHarness();
  const soon = api.inboxInputChipHtml(METHODS[2]);
  assert.match(soon, /^<button type="button" class="kc-pill inbox-input-chip" data-input-state="coming_soon"/);
  assert.match(soon, /aria-label="Neuralink, coming soon: show details"/);
  assert.match(soon, /aria-expanded="false" aria-controls="inbox-input-note"/);
  assert.match(soon, />Neuralink<\/button>$/);
  assert.match(api.inboxInputChipHtml(METHODS[0]), /aria-label="Talon, works today: show details"/);
  const row = api.inboxInputMethodsHtml();
  assert.ok(!/data-status=|<img|<svg|background-image|style=/.test(row), 'no status colour, image or inline style');
  assert.ok(!/connect|sign in|kbOpenIntegrations|inboxOpenSettings|<input|<form/i.test(row.replace('not connected sources', '')), 'nothing to connect or configure');
  // Names are escaped.
  assert.ok(api.inboxInputChipHtml({ id: 'x', name: '<b>x</b>', state: 'coming_soon' }).includes('&lt;b&gt;x&lt;/b&gt;'));
});

test('click shows the explanation in place and does nothing else', () => {
  const { api, calls, chips, note } = inputHarness();
  assert.match(api.inboxInputNoteHtml(), /id="inbox-input-note" role="status" hidden/);
  api.inboxInputExplain('neuralink');
  assert.strictEqual(api.open(), 'neuralink');
  assert.strictEqual(chips[2].attrs['aria-expanded'], 'true');
  assert.strictEqual(chips[0].attrs['aria-expanded'], 'false');
  assert.ok(!/ hidden/.test(note.html));
  for (const want of [
    'Neuralink: Implanted interface in clinical trials.',
    'There is no public API or export of text or speech that hive can read yet, so it cannot be connected.',
    'Hive will add it as a source when one exists.',
    'What works today: anything that types or dictates into a text box can use ＋ Add, then Quick note.',
    'Hive is not affiliated with Neuralink; this listing reflects interest, not a commitment from them.',
  ]) assert.ok(note.html.includes(want), 'explanation says: ' + want);
  assert.match(note.html, /<a class="inbox-link" href="https:\/\/neuralink\.com\/" target="_blank" rel="noopener noreferrer">Neuralink site<\/a>/);
  // The only control in the note closes it.
  assert.deepStrictEqual(note.html.match(/data-action="([^"]+)"/g), ['data-action="inboxInputClose"']);
  assert.ok(!/partner|integration|official/i.test(note.html));
  // Inert: no request, no re-render of the Inbox, no window.
  assert.deepStrictEqual(calls, { fetches: 0, renders: 0, opened: 0 });
  api.inboxInputClose();
  assert.strictEqual(api.open(), '');
  assert.strictEqual(chips[2].attrs['aria-expanded'], 'false');
  assert.match(note.html, / hidden/);
});

test('focus shows the explanation too; Escape on the chip closes it', () => {
  const { api, chips, note } = inputHarness();
  api.inboxInputOnFocus({ target: chips[0] });
  assert.strictEqual(api.open(), 'talon');
  assert.ok(note.html.includes('It types into any text box, so it already reaches the Inbox: choose ＋ Add and use Quick note. There is nothing to connect. Hive is not affiliated with Talon.'));
  api.inboxInputOnFocus({ target: { closest: () => null } });
  assert.strictEqual(api.open(), 'talon', 'focus elsewhere leaves the note open');
  assert.match(lineSrc("document.addEventListener('focusin', inboxInputOnFocus"), /inboxInputOnFocus\)/);
  assert.match(api.inboxInputChipHtml(METHODS[0]), /data-keydown-action="inboxInputClose" data-keys="Escape"/);
});

test('only an https link to the project is rendered', () => {
  const { api, note } = inputHarness();
  api.inboxInputExplain('synchron');
  assert.ok(note.html.includes('Hive is not affiliated with Synchron'));
  assert.ok(!note.html.includes('javascript:') && !note.html.includes('Synchron site'));
});

// ── Quick note ──
const MB = 1048576;
const UPLOADS = { enabled: true, links: true, max_file_bytes: 1024, max_files_per_drop: 3, extensions: ['.md', '.txt'] };
const NOTE_FNS = ['inboxViewerCanPlay', 'inboxUploadSettings', 'inboxCanAdd', 'inboxFmtBytes', 'inboxUploadPlan', 'inboxUploadRowHtml', 'inboxUploadRowUpdate',
  'inboxAnnounce', 'inboxAddFiles', 'inboxUploadForm', 'inboxUploadSend', 'inboxInSection', 'inboxOnPaste', 'inboxAddUrl', 'inboxAddPanelHtml',
  'inboxNoteBytes', 'inboxNoteName', 'inboxNoteFile', 'inboxNoteHtml', 'inboxNoteInput', 'inboxAddNote', 'inboxPasteAsNote', 'inboxNoteKeepFocus'];
class FakeFile {
  constructor(parts, name, opts) { this.text = parts.join(''); this.name = name; this.type = (opts || {}).type || ''; this.size = Buffer.byteLength(this.text); }
}
class FakeBlob { constructor(parts) { this.size = Buffer.byteLength(parts.join('')); } }
class FakeFormData { constructor() { this.parts = []; } append(k, v, name) { this.parts.push({ k, v, name }); } }
const noteHarness = ({ role = 'owner', uploads = UPLOADS, replies = [] } = {}) => {
  const calls = { sends: [], fetches: [], renders: 0, refreshes: 0, focused: 0, selection: null };
  const status = { textContent: '' };
  const section = {};
  const inside = { closest: sel => (sel === '#inbox-section' ? section : null) };
  const field = { value: '', selectionStart: 2, selectionEnd: 4, focus: () => { calls.focused++; }, setSelectionRange: (a, b) => { calls.selection = [a, b]; } };
  const document = {
    activeElement: { closest: () => null },
    getElementById: id => (id === 'inbox-upload-status' ? status : id === 'inbox-add-note' ? field : null),
    querySelector: () => null,
  };
  class FakeXHR {
    constructor() { this.upload = {}; }
    open(method, url) { this.method = method; this.url = url; }
    send(form) {
      calls.sends.push({ method: this.method, url: this.url, form });
      const r = replies.shift() || { status: 201, body: { results: [{ ok: true, source: 'upload', item_id: 'n1', title: 'note' }] } };
      setImmediate(() => { this.status = r.status; this.responseText = JSON.stringify(r.body); this.onload(); });
    }
  }
  const api = new Function('_inboxCache', 'window', 'document', 'escapeHtml', 'calls', 'FakeXHR', 'File', 'Blob', 'FormData',
    'const renderInbox = () => { calls.renders++; }; const fetchInbox = async () => { calls.refreshes++; }; const fetch = async (u, o) => { calls.fetches.push({ u, o }); return { ok: true, status: 201, json: async () => ({}) }; };\n' +
    'let _inboxUploads = []; let _inboxUploadSeq = 0; let _inboxAddOpen = false; let _inboxUploadQueue = Promise.resolve(); let _inboxPointerInside = false;\n' +
    'let _inboxNoteDraft = ""; let _inboxNoteError = ""; let _inboxNoteBusy = false;\n' +
    ['INBOX_UPLOAD_LABELS', 'INBOX_URL_RE', 'INBOX_NOTE_FIELD_ID', 'INBOX_NOTE_MIME'].map(n => lineSrc('const ' + n + ' =')).join('\n') + '\n' +
    'function inboxNewXHR() { return new FakeXHR(); }\n' + NOTE_FNS.map(fnSrc).join('\n') +
    '\nreturn { ' + NOTE_FNS.join(', ') + ', draft: () => _inboxNoteDraft, error: () => _inboxNoteError, busy: () => _inboxNoteBusy, addOpen: () => _inboxAddOpen, uploads: () => _inboxUploads, setDraft: v => { _inboxNoteDraft = v; } };')(
    { settings: { uploads } }, { _hiveRole: role }, document, escapeHtml, calls, FakeXHR, FakeFile, FakeBlob, FakeFormData);
  return { api, calls, field, status, inside, document };
};

test('the Add panel has a labelled Quick note box that keeps the draft', () => {
  const { api } = noteHarness();
  api.setDraft('a <draft>');
  const panel = api.inboxAddPanelHtml.toString();
  assert.ok(panel.includes('inboxNoteHtml()'), 'the Add panel renders the note box');
  const box = api.inboxNoteHtml();
  assert.match(box, /<label class="inbox-note-label" for="inbox-add-note">Quick note<\/label>/);
  assert.match(box, /<textarea class="kb-search-box inbox-note-text" id="inbox-add-note"[^>]*data-input-action="inboxNoteInput" data-arg-types="v">a &lt;draft&gt;<\/textarea>/);
  assert.match(box, /data-action="inboxAddNote">Add note<\/button>/);
  assert.ok(box.includes('saved as a markdown file under your name (up to 1.0 KB)'));
  assert.ok(!/style=/.test(box));
  api.inboxNoteInput('typed');
  assert.strictEqual(api.draft(), 'typed');
});

test('Add note sends the text as one markdown file through the upload route and clears the box once accepted', async () => {
  const { api, calls, field } = noteHarness();
  field.value = '  Call the lab about the trial\nsecond line  ';
  const row = await api.inboxAddNote();
  assert.strictEqual(calls.sends.length, 1);
  assert.strictEqual(calls.sends[0].method, 'POST');
  assert.strictEqual(calls.sends[0].url, '/api/inbox/uploads');
  assert.strictEqual(calls.fetches.length, 0, 'no other route is called');
  const part = calls.sends[0].form.parts[0];
  assert.strictEqual(part.k, 'file');
  assert.match(part.name, /^note-\d{4}-\d{2}-\d{2}-\d{6}\.md$/);
  assert.strictEqual(part.v.type, 'text/markdown');
  assert.strictEqual(part.v.text, 'Call the lab about the trial\nsecond line\n');
  assert.strictEqual(row.status, 'done');
  assert.strictEqual(row.key, 'upload/n1');
  assert.strictEqual(api.draft(), '');
  assert.strictEqual(api.error(), '');
  assert.strictEqual(api.busy(), false);
  assert.strictEqual(calls.refreshes, 1, 'the Inbox is re-read so the new item shows');
});

test('the generated name is note-<date>-<time>.md in local time', () => {
  const { api } = noteHarness();
  assert.strictEqual(api.inboxNoteName(new Date(2026, 10, 7, 9, 5, 3)), 'note-2026-11-07-090503.md');
});

test('an empty note and a note over the cap are refused before anything is sent, and the text is kept', async () => {
  const { api, calls, field } = noteHarness();
  field.value = '   \n ';
  assert.strictEqual(await api.inboxAddNote(), null);
  assert.strictEqual(api.error(), 'Type or dictate a note first.');
  field.value = 'x'.repeat(2000);
  assert.strictEqual(await api.inboxAddNote(), null);
  assert.match(api.error(), /^Too long: 2\.0 KB \(the limit is 1\.0 KB\)\./);
  assert.strictEqual(api.draft(), 'x'.repeat(2000));
  assert.strictEqual(calls.sends.length, 0);
  assert.match(api.inboxNoteHtml(), /role="alert">Too long/);
  // Bytes, not characters: multi-byte text counts in full.
  assert.strictEqual(api.inboxNoteBytes('é'), 2);
});

test('the hive enforces the cap too: a refused note stays in the box with the reason', async () => {
  const { api, calls, field } = noteHarness({ replies: [{ status: 413, body: { results: [{ ok: false, code: 'too_large', error: 'too large: the limit is 1.0 KB' }] } }] });
  field.value = 'kept text';
  const row = await api.inboxAddNote();
  assert.strictEqual(calls.sends.length, 1);
  assert.strictEqual(row.status, 'error');
  assert.strictEqual(api.draft(), 'kept text');
  assert.strictEqual(api.error(), 'The note was not added: too large: the limit is 1.0 KB Your text is still here.');
  assert.strictEqual(api.busy(), false);
});

test('read sessions and hives with uploads off cannot add a note', async () => {
  for (const h of [noteHarness({ role: 'read' }), noteHarness({ uploads: { enabled: false } })]) {
    h.field.value = 'note';
    assert.strictEqual(await h.api.inboxAddNote(), null);
    assert.strictEqual(h.calls.sends.length, 0);
    assert.strictEqual(h.api.inboxPasteAsNote('a\nb'), false);
  }
});

test('a multi-line paste onto the Inbox lands in the note box for review; nothing is created', () => {
  const { api, calls, status, inside } = noteHarness();
  let prevented = 0;
  const paste = text => api.inboxOnPaste({ target: inside, preventDefault: () => { prevented++; }, clipboardData: { files: [], getData: () => text } });
  paste('first line\nsecond line\n');
  assert.strictEqual(prevented, 1);
  assert.strictEqual(api.draft(), 'first line\nsecond line');
  assert.strictEqual(api.addOpen(), true, '＋ Add is opened so the box is visible');
  assert.strictEqual(calls.focused, 1);
  assert.strictEqual(calls.sends.length + calls.fetches.length, 0);
  assert.strictEqual(api.uploads().length, 0);
  assert.match(status.textContent, /Quick note box\. Review it, then choose Add note\./);
  // A second paste is appended after what is already there.
  paste('third\nfourth');
  assert.strictEqual(api.draft(), 'first line\nsecond line\n\nthird\nfourth');
  // A single line that is not a link is still left alone, and a single
  // link is still added as a link.
  paste('just one line');
  assert.strictEqual(prevented, 2);
  paste('https://example.org/page');
  assert.strictEqual(prevented, 3);
  assert.strictEqual(api.uploads().length, 1);
  assert.strictEqual(api.uploads()[0].link, 'https://example.org/page');
  assert.strictEqual(api.draft(), 'first line\nsecond line\n\nthird\nfourth');
});

test('pasting into a field stays a normal paste', () => {
  const { api } = noteHarness();
  let prevented = 0;
  const target = { closest: sel => (sel === '#inbox-section' ? {} : sel.startsWith('input') ? {} : null) };
  api.inboxOnPaste({ target, preventDefault: () => { prevented++; }, clipboardData: { files: [], getData: () => 'a\nb' } });
  assert.strictEqual(prevented, 0);
  assert.strictEqual(api.draft(), '');
});

test('a background re-render keeps the caret in the note box', () => {
  const { api, calls, field, document } = noteHarness();
  let painted = 0;
  api.inboxNoteKeepFocus(() => { painted++; });
  assert.deepStrictEqual([painted, calls.focused], [1, 0], 'not focused: nothing to restore');
  document.activeElement = field;
  api.inboxNoteKeepFocus(() => { painted++; });
  assert.deepStrictEqual([painted, calls.focused, calls.selection], [2, 1, [2, 4]]);
  assert.ok(fnSrc('renderInbox').includes('inboxNoteKeepFocus(paint)'));
});

// ── Hit targets ──
test('hit-target CSS: every small Inbox control reaches --tap-target without growing', () => {
  const rule = sel => {
    const at = css.indexOf(sel + ' {');
    assert.ok(at >= 0, sel + ' rule not found');
    return css.slice(at, css.indexOf('}', at));
  };
  // The item checkbox: its label carries a tap-target square.
  assert.match(rule('.inbox-check::before'), /width: var\(--tap-target\); height: var\(--tap-target\)/);
  assert.match(rule('.inbox-toggle'), /min-height: var\(--tap-target\)/);
  // Small buttons (desktop too), pills, "+N / less" and text links: one
  // shared pseudo-element that only ever extends outwards.
  const shared = css.slice(css.indexOf('.inbox-panel .btn-sm::before'));
  const selectors = shared.slice(0, shared.indexOf('{'));
  for (const sel of ['.inbox-panel .btn-sm::before', '.inbox-rec-panel .btn-sm::before', '[id="inbox-mail-log"] .btn-sm::before', '.inbox-pills a::before', '.inbox-link::before', '.inbox-more::before', '.inbox-fact-link::before', '.kc-pill.inbox-input-chip::before']) {
    assert.ok(selectors.includes(sel), sel + ' gets the larger hit area');
  }
  assert.match(shared, /position: absolute;\s*inset: min\(0px, calc\(\(100% - var\(--tap-target\)\) \/ 2\)\);/);
  assert.match(html, /class="[^"]*inbox-share-more"/, 'the +N / less link exists');
  assert.ok(fnSrc('inboxSharePillsHtml').includes('inbox-share-more') && fnSrc('inboxItemHtml').includes('class="inbox-pills"'), '+N / less sits inside .inbox-pills');
  // Not scoped to a media query, and the visible size is untouched.
  // The three Inbox regions that hold buttons exist under those names.
  for (const hook of ['class="inbox-panel"', 'class="inbox-rec-panel"', 'id="inbox-mail-log"']) assert.ok(html.includes(hook), hook);
  // The dashboard pins its notices before the first section by searching
  // for this attribute, so the stylesheet must not spell it.
  assert.ok(!css.includes('data-dashboard-section'));
  assert.ok(!css.includes('@media'));
  assert.ok(!/\.btn-sm[^{]*\{[^}]*(min-height|padding|font-size)/.test(css), '.btn-sm keeps its size');
});

test('the new CSS uses tokens only: no raw colours, no hash, no inline styles in the markup', () => {
  assert.ok(!css.includes('#'), 'no # (hex colour or id selector), comments included');
  assert.ok(!/\b(rgb|rgba|hsl|hsla|color-mix)\(/.test(css), 'no colour functions');
  assert.ok(!/:\s*(white|black|red|green|blue|gray|grey|orange|yellow)\b/.test(css), 'no named colours');
  for (const m of css.matchAll(/(?:^|[\s;{])(color|background|border-color|outline|border)\s*:\s*([^;}]+)/g)) {
    assert.ok(/var\(--|transparent|inherit/.test(m[2]), m[1] + ' uses a token: ' + m[2]);
  }
  assert.ok(css.includes('var(--tap-target)'));
  for (const name of ['inboxInputMethodsHtml', 'inboxInputChipHtml', 'inboxInputNoteHtml', 'inboxNoteHtml']) {
    assert.ok(!/style=/.test(fnSrc(name)), name + ' has no inline style');
  }
  const js = ['inbox-input-methods.js', 'inbox-note.js'].map(f => fs.readFileSync(path.join(__dirname, 'static-src', 'js', f), 'utf8')).join('\n');
  assert.ok(!/setInterval|setTimeout/.test(js), 'no timers');
});
