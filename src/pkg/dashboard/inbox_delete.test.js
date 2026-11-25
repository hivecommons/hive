// Inbox 🗑 Delete on every item kind and Delete… in the bulk menu
// (hivecommons/hive#18694): who sees it, what the in-page confirmation says
// is removed, the Knowledge box, and the request it sends.
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
const escapeHtml = s => String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
const NAMES = [
  'fetchInbox', 'renderInbox', 'inboxRoleKnown', 'inboxCanShare', 'inboxFind', 'inboxItemKey', 'inboxShownItems',
  'inboxPruneSelection', 'inboxToggleProcessed', 'inboxSelectAll', 'inboxShareMenuClose', 'inboxIconSvg', 'inboxDeleteKind',
  'inboxDeleteKnowledgeRemovable', 'inboxCanDelete', 'inboxDeleteBusy', 'inboxDeleteRemovesText',
  'inboxDeleteKnowledgeText', 'inboxDeleteSharesText', 'inboxDeleteDialog', 'inboxDeleteButtonHtml',
  'inboxDeleteBulkGroups', 'inboxDeleteEligible', 'inboxDeleteRun', 'inboxDeleteItem', 'inboxDeleteSelected',
];

// Exercise the Inbox view and delete functions with the rest of the page and the server faked.
const harness = ({ role = 'owner', user = 'ana@example.com', items = [], selected = [], answer = true, status = 200, body } = {}) => {
  const calls = { confirms: [], fetches: [], toasts: [], refreshes: 0 };
  const ctx = { window: { _hiveRole: role, _hiveUser: user }, _inboxContent: {}, selected, items, activeElement: null };
  const panel = {
    innerHTML: '',
    querySelector(selector) {
      const action = /^\[data-action="([^"]+)"\]$/.exec(selector)?.[1];
      if (!action || !this.innerHTML.includes('data-action="' + action + '"')) return null;
      return { focus: () => { this.focusedAction = action; } };
    },
  };
  const readInbox = () => ({ items: ctx.items.map(it => ({ ...it })) });
  for (const it of items) ctx._inboxContent[it.source + '/' + it.id] = { transcript: 'cached' };
  const fetch = async (url, opts) => {
    if (url === '/api/inbox') {
      calls.refreshes++;
      return { ok: true, json: async () => readInbox() };
    }
    const payload = JSON.parse(opts.body);
    calls.fetches.push({ url, opts, body: payload });
    return { ok: status >= 200 && status < 300, status, json: async () => body || { deleted: payload.items.length, items: payload.items } };
  };
  const api = new Function('ctx', 'panel', 'initialInbox', 'fetch', 'escapeHtml', 'hiveConfirm', 'showToast', `
    const window = ctx.window, _inboxContent = ctx._inboxContent;
    let _inboxCache = initialInbox, _inboxBusyTimer;
    let _inboxSelected = new Set(ctx.selected), _inboxShowProcessed = false, _inboxShareMenu = null;
    const _inboxFocusKey = '';
    const noop = () => {};
    const document = {
      getElementById: id => id === 'inbox-panel' ? panel : null,
      get activeElement() { return ctx.activeElement; },
      removeEventListener: noop,
    };
    const refreshSectionCardShell = noop, setSectionSummary = noop, inboxApplyFocus = noop;
    const viewerIsOwner = () => window._hiveRole === 'owner';
    const inboxSourcesHtml = () => '', inboxCanAdd = () => false;
    const inboxRouteBusy = () => false, inboxTxAnyBusy = () => false;
    const inboxPreserveAudio = (_, paint) => paint();
    const inboxItemHtml = it => escapeHtml(it.title || it.id);
    const INBOX_DELETE_KNOWLEDGE_LABEL = 'Also remove the Knowledge copy';
    ${NAMES.map(fnSrc).join('\n')}
    return {
      ${NAMES.join(', ')},
      get selected() { return _inboxSelected; },
      set shareMenu(menu) { _inboxShareMenu = menu; },
    };
  `)(
    ctx, panel, readInbox(), fetch, escapeHtml,
    async cfg => { calls.confirms.push(cfg); return typeof answer === 'function' ? answer(cfg) : answer; },
    (m, k) => calls.toasts.push([m, k]));
  return { api, calls, ctx, panel };
};

const upload = { source: 'upload', id: 'u1', title: 'rfc-v7-teams', fact_slug: 'upload-u1', dismissed: true };
const recording = { source: 'hive-recording', id: 'r1', recorded_by: 'ana@example.com', fact_slug: 'hive-recording-r1' };
const mail = { source: 'email', id: 'e1' };
const meeting = { source: 'meeting', id: 'm1' };
const plaud = { source: 'plaud', id: 'p1', fact_slug: 'plaud-plaud-p1' };
const zoom = { source: 'zoom', id: 'z1' };

test('every kind of item is deletable by owner and read-write, never by read', () => {
  const { api } = harness();
  for (const it of [upload, mail, meeting, plaud, zoom]) {
    assert.strictEqual(api.inboxCanDelete(it, 'owner', 'bob@example.com'), true, it.source);
    assert.strictEqual(api.inboxCanDelete(it, 'read-write', 'bob@example.com'), true, it.source);
    assert.strictEqual(api.inboxCanDelete(it, 'read', 'bob@example.com'), false, it.source);
    assert.strictEqual(api.inboxCanDelete(it, undefined, ''), false, it.source);
  }
  assert.strictEqual(api.inboxCanDelete(null, 'owner', 'x'), false);
});

test('a hive recording keeps its rule: an owner, or the read-write member who recorded it', () => {
  const { api } = harness();
  const can = api.inboxCanDelete;
  assert.strictEqual(can(recording, 'owner', 'bob@example.com'), true);
  assert.strictEqual(can(recording, 'read-write', 'ana@example.com'), true);
  assert.strictEqual(can(recording, 'read-write', 'ANA@example.com'), true, 'logins compare case-insensitively');
  assert.strictEqual(can(recording, 'read-write', 'bob@example.com'), false);
  assert.strictEqual(can(recording, 'read-write', ''), false);
  assert.strictEqual(can(recording, 'read', 'ana@example.com'), false);
});

test('a mailed-in item waiting for approval is the owner\'s: Decline, not Delete', () => {
  const waiting = Object.assign({}, mail, { awaiting_approval: true });
  const { api } = harness();
  assert.strictEqual(api.inboxCanDelete(waiting, 'read-write', 'bob'), false);
  assert.strictEqual(api.inboxCanDelete(waiting, 'owner', 'bob'), true);
  assert.strictEqual(api.inboxDeleteButtonHtml(waiting), '', 'the row offers ✕ Decline instead');
});

test('the per-item control: on every kind, disabled while transcribing, absent for read', () => {
  const { api } = harness();
  for (const it of [upload, recording, mail, meeting, plaud]) {
    const out = api.inboxDeleteButtonHtml(it);
    assert.ok(out.includes('data-action="inboxDeleteItem" data-arg0="' + it.source + '" data-arg1="' + it.id + '"'), it.source);
    // An icon button (#18700): named by aria-label and a title, last in the row.
    assert.match(out, /class="hv-btn btn-icon btn-sm inbox-action inbox-action-danger inbox-delete"[^>]* aria-label="Delete"[^>]* title="[^"]+"><svg class="inbox-ic"[^]*?<\/svg><\/button>$/);
    assert.doesNotMatch(out, /🗑/);
    assert.doesNotMatch(out, /disabled/);
  }
  const busy = api.inboxDeleteButtonHtml(Object.assign({}, upload, { transcription: { status: 'running' } }));
  assert.match(busy, / disabled aria-disabled="true" title="Being transcribed: delete it once the transcription finishes"/);
  assert.strictEqual(harness({ role: 'read' }).api.inboxDeleteButtonHtml(upload), '');
  assert.strictEqual(harness({ role: 'read-write', user: 'bob@example.com' }).api.inboxDeleteButtonHtml(recording), '');
  // An id is escaped into the attributes.
  assert.ok(api.inboxDeleteButtonHtml({ source: 'plaud', id: 'a"b' }).includes('data-arg1="a&quot;b"'));
  // The item row renders the control.
  assert.match(fnSrc('inboxItemHtml'), /inboxDeleteButtonHtml\(it\)[^]*inbox-actions-sep/);
});

test('the confirmation says what is removed for each kind', () => {
  const { api } = harness();
  const text = it => api.inboxDeleteDialog([it]).message;
  assert.match(text(upload), /^This deletes the file stored on this hive, its extracted text or machine transcript record, and this Inbox item\./);
  assert.match(text(recording), /^This deletes the recording's audio stored on this hive, its machine transcript record and this Inbox item\./);
  assert.match(text(mail), /^This deletes the mailed-in file stored on this hive/);
  assert.match(text(zoom), /^This removes the item from the Inbox only, and the next sync does not bring it back\. The recording itself stays in zoom\./);
  for (const it of [upload, recording, mail, meeting, plaud, zoom]) {
    const d = api.inboxDeleteDialog([it]);
    assert.strictEqual(d.title, 'Delete this item?');
    assert.match(d.message, /This cannot be undone\.$/, it.source);
  }
});

test('the confirmation says whether the Knowledge copy is kept, and offers the box only where it can go', () => {
  const { api } = harness();
  for (const it of [upload, recording]) {
    const d = api.inboxDeleteDialog([it]);
    assert.strictEqual(d.knowledge, true, it.source);
    assert.match(d.message, /Its Knowledge copy is kept, unless you tick the box below\./, it.source);
  }
  // A synced source's fact belongs to its sync: kept, and no box.
  const synced = api.inboxDeleteDialog([plaud]);
  assert.strictEqual(synced.knowledge, false);
  assert.match(synced.message, /Its Knowledge fact is kept: the plaud sync maintains it\./);
  // No Knowledge copy, nothing said about one.
  for (const it of [mail, meeting, zoom, Object.assign({}, upload, { fact_slug: '' })]) {
    const d = api.inboxDeleteDialog([it]);
    assert.strictEqual(d.knowledge, false, it.source);
    assert.doesNotMatch(d.message, /Knowledge/, it.source);
  }
});

test('the confirmation for a meeting says the meeting record stays', () => {
  const { api } = harness();
  const d = api.inboxDeleteDialog([meeting]);
  assert.match(d.message, /^This removes the meeting from the Inbox only\. The meeting record, with its transcript and notes, stays in the Meetings section; delete it there to erase it\./);
});

test('the confirmation says shared copies stay only when there is share history', () => {
  const { api } = harness();
  assert.doesNotMatch(api.inboxDeleteDialog([upload]).message, /already shared/);
  const shared = api.inboxDeleteDialog([Object.assign({}, upload, { share_history: [{ destination: 'dibs' }, { destination: 'spek_rfc' }] })]);
  assert.match(shared.message, /What was already shared \(2 shares: RFCs, issues, DIBS ideas, spek seeds\) stays where it is\./);
  assert.match(api.inboxDeleteDialog([Object.assign({}, plaud, { share_history: [{}] })]).message, /\(1 share: /);
});

test('the bulk confirmation counts each kind', () => {
  const { api } = harness();
  const d = api.inboxDeleteDialog([upload, recording, mail, meeting, plaud, zoom]);
  assert.strictEqual(d.title, 'Delete 6 items?');
  assert.strictEqual(d.knowledge, true);
  assert.match(d.message, /^This deletes 6 Inbox items\./);
  assert.match(d.message, /3 are recordings, uploads or mailed-in files stored on this hive: the stored file and its transcript record are deleted\./);
  assert.match(d.message, /1 is a meeting hive attended: the meeting record, with its transcript and notes, stays in the Meetings section\./);
  assert.match(d.message, /2 come from a synced source: the recording stays with its source, and the next sync does not bring the item back\./);
  assert.match(d.message, /Knowledge copies are kept, unless you tick the box below\./);
  assert.match(d.message, /1 Knowledge fact of synced sources is kept: the sync maintains it\./);
  const synced = api.inboxDeleteDialog([plaud, zoom]);
  assert.strictEqual(synced.knowledge, false);
  assert.doesNotMatch(synced.message, /tick the box|stored on this hive|meeting/);
});

test('delete uses the in-page dialog and posts the item; the Knowledge copy stays unless the box is ticked', async () => {
  let h = harness({ items: [upload], selected: ['upload/u1'], answer: { checked: false } });
  await h.api.inboxDeleteItem('upload', 'u1');
  assert.strictEqual(h.calls.confirms.length, 1);
  const cfg = h.calls.confirms[0];
  assert.strictEqual(cfg.title, 'Delete this item?');
  assert.strictEqual(cfg.confirmLabel, 'Delete');
  assert.strictEqual(cfg.danger, true);
  assert.deepStrictEqual(cfg.check, { label: 'Also remove the Knowledge copy', checked: false }, 'the box starts unticked');
  assert.strictEqual(h.calls.fetches[0].url, '/api/inbox/delete');
  assert.strictEqual(h.calls.fetches[0].opts.method, 'POST');
  assert.deepStrictEqual(h.calls.fetches[0].body, { items: [{ source: 'upload', item_id: 'u1' }], remove_knowledge: false });
  assert.deepStrictEqual(h.calls.toasts[0], ['Item deleted', 'success']);
  assert.strictEqual(h.calls.refreshes, 1);
  assert.strictEqual(h.api.selected.has('upload/u1'), false, 'the deleted item leaves the selection');
  assert.strictEqual(h.ctx._inboxContent['upload/u1'], undefined, 'and its cached content is dropped');

  h = harness({ items: [upload], answer: { checked: true } });
  await h.api.inboxDeleteItem('upload', 'u1');
  assert.strictEqual(h.calls.fetches[0].body.remove_knowledge, true);

  // No removable Knowledge copy: no box, and never remove_knowledge.
  h = harness({ items: [plaud], answer: true });
  await h.api.inboxDeleteItem('plaud', 'p1');
  assert.strictEqual(h.calls.confirms[0].check, undefined);
  assert.strictEqual(h.calls.fetches[0].body.remove_knowledge, false);

  // Cancel: nothing is sent.
  h = harness({ items: [upload], answer: false });
  await h.api.inboxDeleteItem('upload', 'u1');
  assert.strictEqual(h.calls.fetches.length, 0);
  assert.strictEqual(h.calls.refreshes, 0);

  // An unknown item: no dialog.
  h = harness({ items: [] });
  await h.api.inboxDeleteItem('upload', 'gone');
  assert.strictEqual(h.calls.confirms.length, 0);
});

test('a failed delete is reported and the Inbox is re-read', async () => {
  let h = harness({ items: [upload], answer: { checked: false }, status: 409, body: { error: 'this item is being transcribed; delete it once the transcription finishes' } });
  await h.api.inboxDeleteItem('upload', 'u1');
  assert.deepStrictEqual(h.calls.toasts[0], ['Could not delete: this item is being transcribed; delete it once the transcription finishes', 'error']);
  assert.strictEqual(h.calls.refreshes, 1);
  h = harness({ items: [upload, plaud], selected: ['upload/u1', 'plaud/p1'], answer: { checked: false },
    body: { deleted: 1, items: [{ source: 'upload', item_id: 'u1' }], failed: [{ source: 'plaud', item_id: 'p1', error: 'no such inbox item' }] } });
  await h.api.inboxDeleteSelected();
  assert.deepStrictEqual(h.calls.toasts[0], ['1 deleted, 1 not deleted: no such inbox item', 'error']);
  assert.deepStrictEqual([...h.api.selected], ['plaud/p1'], 'only the deleted item leaves the selection');
});

test('Delete… in the bulk menu deletes the selected items the viewer may delete', async () => {
  let h = harness({ role: 'read-write', user: 'bob@example.com', items: [upload, recording, plaud, Object.assign({}, zoom, { transcription: { status: 'queued' } })],
    selected: ['upload/u1', 'hive-recording/r1', 'plaud/p1', 'zoom/z1', 'plaud/gone'], answer: { checked: false } });
  assert.deepStrictEqual(h.api.inboxDeleteBulkGroups(), [{ id: 'manage', label: 'Manage', items: [{ id: 'delete', label: 'Delete…', disabled: false, hint: '' }] }]);
  await h.api.inboxDeleteSelected();
  assert.strictEqual(h.calls.confirms[0].title, 'Delete 2 items?', 'someone else\'s recording and the item being transcribed are left out');
  assert.deepStrictEqual(h.calls.fetches[0].body.items, [{ source: 'upload', item_id: 'u1' }, { source: 'plaud', item_id: 'p1' }]);
  assert.deepStrictEqual(h.calls.toasts[0], ['2 items deleted', 'success']);

  h = harness({ role: 'read-write', user: 'bob@example.com', items: [recording], selected: ['hive-recording/r1'] });
  await h.api.inboxDeleteSelected();
  assert.strictEqual(h.calls.confirms.length, 0);
  assert.deepStrictEqual(h.calls.toasts[0], ['None of the selected items can be deleted by you right now', 'error']);
  h = harness({ items: [] });
  await h.api.inboxDeleteSelected();
  assert.deepStrictEqual(h.calls.toasts[0], ['Nothing selected', 'error']);

  assert.deepStrictEqual(harness({ role: 'read' }).api.inboxDeleteBulkGroups(), []);
  // The bulk menu appends the group and routes its entry to the delete.
  const menu = fnSrc('inboxBulkMenu');
  assert.match(menu, /inboxBulkMenuGroups\(\)\.concat\(manage\)/);
  assert.match(menu, /d\.inboxShareGroup === 'manage' \? inboxDeleteSelected\(\) : inboxBulkMenuRun\(d\.inboxShareAct\)/);
});

test('delete never uses a native browser dialog', () => {
  for (const name of ['inboxDeleteRun', 'inboxDeleteItem', 'inboxDeleteSelected']) {
    assert.doesNotMatch(fnSrc(name), /window\.confirm|\balert\(|[^.A-Za-z]confirm\(/, name);
  }
  assert.match(fnSrc('inboxDeleteRun'), /await hiveConfirm\(cfg\)/);
});

test('bulk delete skips selections hidden by the filter or removed by a refresh', async () => {
  const visible = { source: 'upload', id: 'visible', title: 'Visible notes' };
  const hidden = { source: 'upload', id: 'hidden', title: 'Processed notes', processed: true };
  const processing = { source: 'upload', id: 'processing', title: 'Newly processed notes' };
  const removed = { source: 'upload', id: 'removed', title: 'Removed notes' };
  const h = harness({ items: [visible, hidden, processing, removed] });

  h.api.inboxToggleProcessed();
  h.api.inboxSelectAll();
  assert.match(h.panel.innerHTML, /Hive \(4\)/);
  h.api.inboxToggleProcessed();
  assert.match(h.panel.innerHTML, /Hive \(3\)/);
  assert.deepStrictEqual([...h.api.selected], ['upload/visible', 'upload/processing', 'upload/removed']);

  h.ctx.items = [visible, hidden, { ...processing, processed: true }];
  await h.api.fetchInbox();
  assert.match(h.panel.innerHTML, /Hive \(1\)/);
  h.api.inboxToggleProcessed();
  assert.deepStrictEqual([...h.api.selected], ['upload/visible']);

  await h.api.inboxDeleteSelected();
  assert.match(h.calls.confirms[0].message, /Visible notes/);
  assert.doesNotMatch(h.calls.confirms[0].message, /Processed notes|Newly processed notes|Removed notes/);
  assert.deepStrictEqual(h.calls.fetches[0].body.items, [{ source: 'upload', item_id: 'visible' }]);
});

test('bulk delete aborts the whole request if a target changes while confirmation is open', async () => {
  const changes = [
    h => { h.ctx.items[1] = { ...h.ctx.items[1], processed: true }; },
    h => { h.ctx.items.pop(); },
    h => { h.ctx.items[1] = { ...h.ctx.items[1], transcription: { status: 'running' } }; },
    h => { h.api.selected.delete('plaud/p1'); },
  ];
  for (const change of changes) {
    let confirm;
    const answer = new Promise(resolve => { confirm = resolve; });
    const h = harness({ items: [upload, plaud], selected: ['upload/u1', 'plaud/p1'], answer });
    const pending = h.api.inboxDeleteSelected();
    assert.strictEqual(h.calls.confirms[0].title, 'Delete 2 items?');
    change(h);
    await h.api.fetchInbox();
    confirm(true);
    await pending;
    assert.deepStrictEqual(h.calls.fetches, []);
    assert.deepStrictEqual(h.calls.toasts, [['The Inbox selection changed. Review it before deleting.', 'error']]);
  }
});

test('a refresh that closes the focused bulk menu returns focus to the toolbar', async () => {
  for (const [items, expected] of [[[upload], 'inboxBulkMenu'], [[], 'inboxToggleProcessed']]) {
    const h = harness({ items: [upload, plaud], selected: ['upload/u1', 'plaud/p1'] });
    const focusedItem = {};
    h.ctx.activeElement = focusedItem;
    const menu = {
      btn: { dataset: { action: 'inboxBulkMenu' }, setAttribute() {} },
      el: { contains: el => el === focusedItem, remove() { this.removed = true; } },
    };
    h.api.shareMenu = menu;
    h.ctx.items = items;
    await h.api.fetchInbox();
    assert.ok(menu.el.removed);
    assert.strictEqual(h.panel.focusedAction, expected);
  }
});

test('a changed Knowledge copy requires a fresh confirmation when removal is selected', async () => {
  const second = { source: 'upload', id: 'u2', title: 'Second upload' };
  for (const checked of [true, false]) {
    const h = harness({
      items: [upload, second], selected: ['upload/u1', 'upload/u2'],
      answer: async () => {
        h.ctx.items = [upload, { ...second, fact_slug: 'upload-u2', layer: 'inbox' }];
        await h.api.fetchInbox();
        return { checked };
      },
    });
    await h.api.inboxDeleteSelected();
    assert.ok(h.calls.confirms[0].check, 'the first item has a removable Knowledge copy');
    if (checked) {
      assert.deepStrictEqual(h.calls.fetches, []);
      assert.deepStrictEqual(h.calls.toasts, [['A Knowledge copy changed. Review the items before deleting.', 'error']]);
    } else {
      assert.strictEqual(h.calls.fetches[0].body.remove_knowledge, false);
    }
  }
});

test('bulk delete starts at the scrollable target list and cleans up on close', async () => {
  const h = harness({ items: [upload, plaud], selected: ['upload/u1', 'plaud/p1'], answer: false });
  await h.api.inboxDeleteSelected();
  let onKey;
  let bodyFocused = false;
  let observers = 0;
  class Observer {
    observe() { observers++; }
    disconnect() { observers--; }
  }
  const resizeListeners = new Set();
  const window = {
    addEventListener: (_, handler) => resizeListeners.add(handler),
    removeEventListener: (_, handler) => resizeListeners.delete(handler),
  };
  const message = {
    tagName: 'DIV',
    dataset: {},
    classList: { contains: () => true, toggle() {} },
    focus: () => { bodyFocused = true; },
    addEventListener() {},
    removeEventListener() {},
  };
  const overlay = {
    classList: { add() {} },
    insertBefore() {},
    addEventListener() {},
    remove() { this.removed = true; },
    querySelectorAll(selector) {
      return selector === '.modal-scroll' && this.innerHTML.includes('long modal-scroll') ? [message] : [];
    },
    querySelector(selector) {
      if (selector === '[data-initial-focus]' && this.innerHTML.includes('tabindex="0" data-initial-focus')) return message;
      return null;
    },
  };
  const document = {
    createElement: tag => tag === 'div' ? overlay : { setAttribute() {}, addEventListener() {} },
    body: { appendChild() {} },
    addEventListener: (_, handler) => { onKey = handler; },
    removeEventListener() {},
  };
  const dialogFunctions = [
    'hiveDialogEscape', 'hiveNormalizeDialogOptions', 'hiveConfirmIsDanger', 'hiveModalScrollSync', 'hiveModalScrollInit',
    'hiveModalSheetDecorate', 'hiveWireDialog', 'hiveConfirm',
  ];
  const confirm = new Function('document', 'window', 'ResizeObserver', 'MutationObserver', 'hiveRememberModalOpen', 'setTimeout',
    'const HIVE_MODAL_SCROLL_EPS = 2;\n' + dialogFunctions.map(fnSrc).join('\n') + '\nreturn hiveConfirm;')(
    document, window, Observer, Observer, () => {}, fn => fn());
  const pending = confirm(h.calls.confirms[0]);
  assert.match(overlay.innerHTML, /class="hive-dialog-body long modal-scroll" tabindex="0" data-initial-focus/);
  assert.ok(bodyFocused);
  assert.strictEqual(resizeListeners.size, 1);
  assert.strictEqual(observers, 2);
  onKey({ key: 'Enter', target: message, preventDefault() {} });
  assert.ok(!overlay.removed);
  onKey({ key: 'Escape', preventDefault() {} });
  assert.strictEqual(await pending, false);
  assert.strictEqual(resizeListeners.size, 0);
  assert.strictEqual(observers, 0);
});

test('per-item delete checks current eligibility without requiring selection or visibility', async () => {
  const h = harness({ items: [upload], answer: async () => {
    h.ctx.items[0] = { ...upload, processed: true };
    await h.api.fetchInbox();
    return true;
  } });
  await h.api.inboxDeleteItem('upload', 'u1');
  assert.deepStrictEqual(h.calls.fetches[0].body.items, [{ source: 'upload', item_id: 'u1' }]);

  const gone = harness({ items: [upload], answer: async () => {
    gone.ctx.items.pop();
    await gone.api.fetchInbox();
    return true;
  } });
  await gone.api.inboxDeleteItem('upload', 'u1');
  assert.deepStrictEqual(gone.calls.fetches, []);
  assert.deepStrictEqual(gone.calls.toasts, [['This item can no longer be deleted. Review the Inbox.', 'error']]);
});
