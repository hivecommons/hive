// Inbox drag-and-drop ingest UI: the drop zone, ＋ Add, paste, per-file
// upload rows with progress and errors, and role gating. Functions are
// extracted from the generated page and run in a sandbox with the browser
// pieces they touch injected.
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
const lineSrc = prefix => {
  const at = html.indexOf(prefix);
  assert.ok(at >= 0, prefix + ' not found');
  return html.slice(at, html.indexOf('\n', at));
};

const escapeHtml = s => String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
const MB = 1048576;
const UPLOADS = { enabled: true, links: true, max_file_bytes: 25 * MB, max_files_per_drop: 3, extensions: ['.csv', '.json', '.md', '.txt'] };

const FNS = ['inboxViewerCanPlay', 'inboxUploadSettings', 'inboxCanAdd', 'inboxFmtBytes', 'inboxUploadPlan', 'inboxUploadRowHtml',
  'inboxUploadsHtml', 'inboxAddPanelHtml', 'inboxAnnounce', 'inboxUploadRowUpdate', 'inboxAddFiles', 'inboxUploadSend',
  'inboxDragHasFiles', 'inboxInSection', 'inboxSetDragOver', 'inboxOnDragOver', 'inboxOnDragLeave', 'inboxOnDrop', 'inboxOnPaste',
  'inboxPickFiles', 'inboxClearUploads', 'inboxAddUrl', 'inboxAddUrlFromField'];

// harness builds the upload functions with a fake document, XHR and the
// Inbox cache. replies is a list of {status, body} answered in order.
const harness = ({ role = 'owner', uploads = UPLOADS, replies = [], fetchReplies = [] } = {}) => {
  const calls = { sends: [], fetches: [], renders: 0, refreshes: 0, rows: {}, classes: {} };
  const urlField = { value: '' };
  const fetch = async (url, opts) => {
    calls.fetches.push({ url, opts });
    const r = fetchReplies.shift() || { status: 201, body: { results: [{ ok: true, source: 'upload', item_id: 'link1', title: 'Page' }] } };
    if (r.network) throw new Error('offline');
    return { ok: r.status >= 200 && r.status < 300, status: r.status, json: async () => r.body };
  };
  const status = { textContent: '' };
  const section = { classList: { toggle: (c, on) => { calls.classes[c] = on; } } };
  const inside = { closest: sel => (sel === '#inbox-section' ? section : null) };
  const outside = { closest: () => null };
  const document = {
    activeElement: outside,
    getElementById: id => (id === 'inbox-upload-status' ? status : id === 'inbox-section' ? section : id === 'inbox-add-url' ? urlField : null),
    querySelector: sel => {
      const m = /data-inbox-upload="(\d+)"/.exec(sel);
      if (!m) return null;
      return { set outerHTML(v) { calls.rows[m[1]] = v; } };
    },
  };
  class FakeXHR {
    constructor() { this.upload = {}; }
    open(method, url) { this.method = method; this.url = url; }
    send(form) {
      calls.sends.push({ method: this.method, url: this.url, form });
      const r = replies.shift() || { status: 201, body: { results: [{ ok: true, source: 'upload', item_id: 'id' + calls.sends.length, title: 'T' }] } };
      setImmediate(() => {
        if (this.upload.onprogress) this.upload.onprogress({ lengthComputable: true, loaded: 50, total: 100 });
        if (r.network) { this.onerror(); return; }
        this.status = r.status;
        this.responseText = typeof r.body === 'string' ? r.body : JSON.stringify(r.body);
        this.onload();
      });
    }
  }
  const ctx = { _inboxCache: { settings: { uploads } } };
  const api = new Function('ctx', 'window', 'document', 'escapeHtml', 'renderInbox', 'fetchInbox', 'calls', 'FakeXHR', 'fetch',
    'let _inboxUploads = []; let _inboxUploadSeq = 0; let _inboxAddOpen = true; let _inboxUploadQueue = Promise.resolve(); let _inboxPointerInside = false;\n' +
    'const _inboxCache = ctx._inboxCache;\n' +
    lineSrc('const INBOX_UPLOAD_LABELS = ') + '\n' + lineSrc('const INBOX_URL_RE = ') + '\n' +
    'function inboxNewXHR() { return new FakeXHR(); }\n' +
    'function inboxUploadForm(row) { return { file: row.file, name: row.name }; }\n' +
    FNS.map(fnSrc).join('\n') + '\n' +
    'return { ' + FNS.join(', ') + ', state: () => ({ uploads: _inboxUploads }), queue: () => _inboxUploadQueue, setPointerInside: v => { _inboxPointerInside = v; } };',
  )(ctx, { _hiveRole: role }, document, escapeHtml, () => { calls.renders++; }, async () => { calls.refreshes++; }, calls, FakeXHR, fetch);
  return { api, calls, status, inside, outside, document, urlField };
};

const file = (name, size) => ({ name, size });
const dragEvent = (target, types, files) => {
  const e = { target, relatedTarget: null, dataTransfer: { types, files: files || [], dropEffect: 'none' }, prevented: false };
  e.preventDefault = () => { e.prevented = true; };
  return e;
};

test('the drop zone, ＋ Add and paste are for owner and read-write only', async () => {
  for (const [role, want] of [['owner', true], ['read-write', true], ['read', false], ['read-only', false], ['', false]]) {
    const { api, calls, inside } = harness({ role });
    assert.strictEqual(api.inboxCanAdd(), want, 'role ' + role);
    const e = dragEvent(inside, ['Files'], [file('a.md', 3)]);
    api.inboxOnDragOver(e);
    assert.strictEqual(e.prevented, want, 'dragover taken for ' + role);
    api.inboxOnDrop(e);
    await api.inboxAddFiles([file('b.md', 3)]);
    assert.strictEqual(calls.sends.length, want ? 2 : 0, 'uploads sent for ' + role);
  }
  // Disabled on the hive (no store): nothing either.
  const { api } = harness({ uploads: { enabled: false } });
  assert.strictEqual(api.inboxCanAdd(), false);
  // The toolbar button and the panels are gated in renderInbox.
  const render = fnSrc('renderInbox');
  assert.match(render, /inboxCanAdd\(\) \? '<button type="button" class="hv-btn btn-primary btn-sm" data-action="inboxToggleAdd"/);
  assert.match(render, /if \(inboxCanAdd\(\)\) html \+= inboxAddPanelHtml\(\) \+ inboxUploadsHtml\(\);/);
});

test('drop overlay shows while files are dragged over the Inbox, and only then', () => {
  const { api, calls, inside, outside } = harness();
  const text = dragEvent(inside, ['text/plain']);
  api.inboxOnDragOver(text);
  assert.strictEqual(text.prevented, false, 'a text drag is left to the browser');
  assert.strictEqual(calls.classes['inbox-dragover'], undefined);
  const away = dragEvent(outside, ['Files']);
  api.inboxOnDragOver(away);
  assert.strictEqual(away.prevented, false, 'outside the Inbox nothing happens');
  const over = dragEvent(inside, ['Files']);
  api.inboxOnDragOver(over);
  assert.ok(over.prevented);
  assert.strictEqual(over.dataTransfer.dropEffect, 'copy');
  assert.strictEqual(calls.classes['inbox-dragover'], true);
  const leaveInside = dragEvent(inside, ['Files']);
  leaveInside.relatedTarget = inside;
  api.inboxOnDragLeave(leaveInside);
  assert.strictEqual(calls.classes['inbox-dragover'], true, 'moving between children keeps the overlay');
  const leave = dragEvent(inside, ['Files']);
  leave.relatedTarget = outside;
  api.inboxOnDragLeave(leave);
  assert.strictEqual(calls.classes['inbox-dragover'], false);
});

test('a multi-file drop gets one row per file with progress, then a refresh', async () => {
  const { api, calls, status, inside } = harness({
    replies: [
      { status: 201, body: { results: [{ ok: true, source: 'upload', item_id: 'u1', title: 'Plan' }] } },
      { status: 422, body: { results: [{ ok: false, code: 'parse_failed', error: 'the file is not valid JSON (line 3: x)' }] } },
    ],
  });
  const drop = dragEvent(inside, ['Files'], [file('plan.md', 10), file('data.json', 20), file('big.md', 26 * MB), file('tool.exe', 4), file('extra.md', 1)]);
  api.inboxOnDrop(drop);
  assert.ok(drop.prevented);
  assert.strictEqual(calls.classes['inbox-dragover'], false, 'the overlay hides on drop');
  const rows = api.state().uploads;
  assert.deepStrictEqual(rows.map(r => r.status), ['queued', 'queued', 'error', 'error', 'error']);
  assert.match(rows[2].error, /Too large: 26\.0 MB \(the limit is 25\.0 MB\)/);
  assert.strictEqual(rows[3].code, 'too_many_files', 'past max_files_per_drop');
  assert.strictEqual(rows[4].code, 'too_many_files');
  assert.match(status.textContent, /Adding 2 files to the Inbox; 3 not added/);
  await api.queue();
  assert.strictEqual(calls.sends.length, 2, 'only accepted files are sent');
  assert.strictEqual(calls.sends[0].url, '/api/inbox/uploads');
  assert.strictEqual(calls.sends[0].method, 'POST');
  assert.strictEqual(calls.sends[0].form.name, 'plan.md');
  assert.strictEqual(rows[0].status, 'done');
  assert.strictEqual(rows[0].key, 'upload/u1');
  assert.strictEqual(rows[1].status, 'error');
  assert.match(rows[1].error, /not valid JSON \(line 3/);
  assert.strictEqual(calls.refreshes, 1, 'the Inbox is re-read once');
  assert.match(status.textContent, /1 of 5 files added to the Inbox/);
  // Rows are redrawn in place: progress, then the outcome.
  assert.match(calls.rows['1'], /data-status="done"/);
  assert.match(calls.rows['1'], /data-action="inboxFocusItem" data-prevent="1" data-arg0="upload\/u1"/);
  assert.match(calls.rows['2'], /role="alert">the file is not valid JSON/);
  const list = api.inboxUploadsHtml();
  assert.match(list, /Clear finished/);
  assert.strictEqual((list.match(/<li class="inbox-upload"/g) || []).length, 5);
  api.inboxClearUploads();
  assert.strictEqual(api.state().uploads.length, 0);
});

test('upload rows: progress bar while uploading, escaped names and errors', () => {
  const { api } = harness();
  const up = api.inboxUploadRowHtml({ id: 7, name: '<b>x</b>.md', size: 2048, status: 'uploading', progress: 0.42 });
  assert.match(up, /Uploading 42%/);
  assert.match(up, /<progress class="kc-progress-bar inbox-upload-bar" max="100" value="42" aria-label="Uploading &lt;b&gt;x&lt;\/b&gt;\.md">/);
  assert.match(up, /2\.0 KB/);
  assert.doesNotMatch(up, /<b>x<\/b>/);
  const err = api.inboxUploadRowHtml({ id: 8, name: 'a.exe', size: 3, status: 'error', error: 'Unsupported <type>' });
  assert.match(err, /data-status="error"/);
  assert.match(err, /Not added/);
  assert.match(err, /Unsupported &lt;type&gt;/);
  assert.doesNotMatch(err, /<progress/);
});

test('client checks: empty, unsupported extension, files without an extension go to the hive', () => {
  const { api } = harness();
  const rows = api.inboxUploadPlan([file('e.md', 0), file('a.EXE', 9), file('README', 9), file('ok.MD', 9)], Object.assign({}, UPLOADS, { max_files_per_drop: 10 }));
  assert.deepStrictEqual(rows.map(r => r.code || r.status), ['empty', 'unsupported_type', 'queued', 'queued']);
  assert.match(rows[1].error, /Unsupported type \.exe: the Inbox reads \.csv \.json \.md \.txt/);
});

test('network errors and non-JSON replies become per-file errors', async () => {
  const { api } = harness({ replies: [{ network: true }, { status: 500, body: 'oops' }] });
  const rows = await api.inboxAddFiles([file('a.md', 1), file('b.md', 1)]);
  assert.strictEqual(rows[0].status, 'error');
  assert.match(rows[0].error, /network error/);
  assert.strictEqual(rows[1].error, 'HTTP 500');
});

test('paste adds clipboard files only while the Inbox has focus', async () => {
  const { api, calls, inside, outside, document } = harness();
  const paste = (target, files) => {
    const e = { target, clipboardData: { files }, prevented: false };
    e.preventDefault = () => { e.prevented = true; };
    api.inboxOnPaste(e);
    return e;
  };
  assert.strictEqual(paste(outside, [file('a.md', 1)]).prevented, false, 'elsewhere: left alone');
  assert.strictEqual(paste(inside, []).prevented, false, 'text paste: left alone');
  assert.strictEqual(paste(inside, [file('a.md', 1)]).prevented, true);
  document.activeElement = inside;
  assert.strictEqual(paste(outside, [file('b.md', 1)]).prevented, true, 'focus inside the Inbox counts');
  document.activeElement = outside;
  api.setPointerInside(true);
  assert.strictEqual(paste(outside, [file('c.md', 1)]).prevented, true, 'last click inside the Inbox counts');
  await api.queue();
  assert.strictEqual(calls.sends.length, 3);
});

test('＋ Add panel: keyboard-operable picker, limits and privacy note', () => {
  const { api } = harness();
  const panel = api.inboxAddPanelHtml();
  assert.match(panel, /<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxChooseFiles">Choose files…<\/button>/);
  assert.match(panel, /<input type="file" multiple hidden/);
  assert.match(panel, /accept="\.csv,\.json,\.md,\.txt"/);
  assert.match(panel, /up to 3 files of 25\.0 MB each/);
  assert.match(panel, /Files stay on this hive/);
  const picked = { files: [file('p.md', 2)], value: 'C:\\fakepath\\p.md' };
  api.inboxPickFiles(picked);
  assert.strictEqual(picked.value, '', 'the picker is reset so the same file can be picked again');
  assert.strictEqual(api.state().uploads.length, 1);
  assert.match(fnSrc('inboxToggleAdd'), /aria-expanded|focus\(\)/);
});

test('links: the Add field and a pasted URL are fetched by the hive, errors stay per row', async () => {
  const { api, calls, inside, urlField, status } = harness({ fetchReplies: [
    { status: 201, body: { results: [{ ok: true, source: 'upload', item_id: 'l1', title: 'Docs' }] } },
    { status: 400, body: { results: [{ ok: false, code: 'link_refused', error: 'link refused: 10.0.0.5 is a private address' }] } },
    { network: true },
  ] });
  urlField.value = '  https://docs.example/page  ';
  await api.inboxAddUrlFromField();
  assert.strictEqual(urlField.value, '', 'the field is cleared');
  assert.strictEqual(calls.fetches[0].url, '/api/inbox/uploads/url');
  assert.deepStrictEqual(JSON.parse(calls.fetches[0].opts.body), { url: 'https://docs.example/page' });
  let rows = api.state().uploads;
  assert.strictEqual(rows[0].status, 'done');
  assert.strictEqual(rows[0].key, 'upload/l1');
  assert.strictEqual(rows[0].name, 'docs.example');
  assert.match(status.textContent, /docs\.example added to the Inbox/);
  assert.strictEqual(calls.refreshes, 1);
  // Pasting a URL while the Inbox has focus adds it; in a text field it stays a paste.
  const paste = (target, text) => {
    const e = { target, clipboardData: { files: [], getData: () => text }, prevented: false };
    e.preventDefault = () => { e.prevented = true; };
    api.inboxOnPaste(e);
    return e;
  };
  const field = { closest: sel => (sel === '#inbox-section' ? {} : sel.startsWith('input') ? {} : null) };
  assert.strictEqual(paste(field, 'https://x.example/').prevented, false, 'paste into a field is left alone');
  assert.strictEqual(paste(inside, 'not a link').prevented, false);
  assert.strictEqual(paste(inside, 'https://a.example/ and more').prevented, false, 'only a single URL');
  assert.strictEqual(paste(inside, 'http://intranet.example/').prevented, true);
  await api.queue();
  rows = api.state().uploads;
  assert.strictEqual(rows[1].status, 'error');
  assert.strictEqual(rows[1].code, 'link_refused');
  assert.match(api.inboxUploadRowHtml(rows[1]), /role="alert">link refused: 10\.0\.0\.5 is a private address/);
  assert.doesNotMatch(api.inboxUploadRowHtml(rows[1]), /<progress/, 'links have no byte progress');
  await api.inboxAddUrl('https://c.example/');
  assert.match(api.state().uploads[2].error, /network error/);
  // Non-http links never leave the browser.
  const n = calls.fetches.length;
  const bad = await api.inboxAddUrl('ftp://x.example/');
  assert.strictEqual(bad.status, 'error');
  assert.strictEqual(calls.fetches.length, n);
  // Links off on the hive, or a read session: nothing.
  assert.strictEqual(await harness({ uploads: Object.assign({}, UPLOADS, { links: false }) }).api.inboxAddUrl('https://d.example/'), null);
  assert.strictEqual(await harness({ role: 'read' }).api.inboxAddUrl('https://d.example/'), null);
  assert.match(api.inboxAddPanelHtml(), /id="inbox-add-url"[^>]*data-enter-action="inboxAddUrlFromField"/);
  assert.match(api.inboxUploadRowHtml({ id: 9, name: 'e.example', link: 'https://e.example/', status: 'uploading' }), /Fetching/);
});

test('audio and video use the recording cap and are accepted by extension', () => {
  const { api } = harness();
  const limits = Object.assign({}, UPLOADS, { max_files_per_drop: 10, media_extensions: ['.m4a', '.mp3', '.mp4'], max_media_bytes: 200 * MB });
  const rows = api.inboxUploadPlan([file('memo.m4a', 120 * MB), file('talk.mp4', 201 * MB), file('notes.md', 26 * MB), file('song.flac', 3)], limits);
  assert.deepStrictEqual(rows.map(r => r.code || r.status), ['queued', 'too_large', 'too_large', 'unsupported_type']);
  assert.match(rows[1].error, /the limit is 200\.0 MB/);
  assert.match(rows[2].error, /the limit is 25\.0 MB/);
});
