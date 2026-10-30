// "Let hive decide" shows it is working (hivecommons/hive#15773). The owner:
// "it takes a few seconds to open the dialog with what hive would do. can
// we have something - that is consistent with the rest of the activity
// tracker, that shows the button is working?" The clicked button goes
// disabled + aria-busy and reads "Asking the hive…" behind the tracker's
// working glyph, the row wears the in-flight caption, and the preview
// opens AT ONCE as a skeleton (Cancel live, the primary disabled) that
// fills when the plan lands; a fetch failure shows inline with Retry; a
// second click while the preview is open is ignored; a plan prefetched as
// the row's options came into view answers the click without a request.
// The functions are lifted out of index.html so this runs the shipped source.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');

function fn(name) {
  const start = html.indexOf('function ' + name + '(');
  assert.ok(start >= 0, name + ' not found in index.html');
  let decl = start;
  if (html.slice(start - 6, start) === 'async ') decl -= 6;
  let depth = 0;
  for (let i = html.indexOf('{', start); i < html.length; i++) {
    if (html[i] === '{') depth++;
    else if (html[i] === '}' && --depth === 0) return html.slice(decl, i + 1);
  }
  assert.fail('unterminated ' + name);
}
function constLine(name) {
  const m = html.match(new RegExp('const ' + name + ' = [^\\n]*\\n'));
  assert.ok(m, name + ' not found in index.html');
  return m[0];
}

// The least DOM the preview needs: elements with classList, attributes,
// an innerHTML that materialises the buttons it names, querySelector by
// class, and a document that records appended overlays.
const dom = `
function el(tag) {
  const e = { tagName: tag, className: '', attrs: {}, children: [], parentNode: null, disabled: false, dataset: {}, _html: '', handlers: {},
    classList: { add(c) { if (!e.className.split(' ').includes(c)) e.className = (e.className + ' ' + c).trim(); }, remove(c) { e.className = e.className.split(' ').filter(x => x && x !== c).join(' '); }, contains(c) { return e.className.split(' ').includes(c); } },
    setAttribute(k, v) { e.attrs[k] = String(v); if (k === 'class') e.className = String(v); }, getAttribute(k) { return k in e.attrs ? e.attrs[k] : null; }, removeAttribute(k) { delete e.attrs[k]; },
    appendChild(c) { c.parentNode = e; e.children.push(c); return c; },
    remove() { if (e.parentNode) e.parentNode.children = e.parentNode.children.filter(c => c !== e); e.parentNode = null; },
    addEventListener(n, f) { e.handlers[n] = f; }, click() { if (e.disabled) return; if (e.handlers.click) e.handlers.click({ target: e }); },
    get firstElementChild() { return e.children[0] || null; },
    get textContent() { return e._html.replace(/<[^>]+>/g, ''); },
    querySelector(sel) { const cls = sel.replace(/^\\./, ''); const walk = n => { for (const c of n.children) { if (c.classList.contains(cls)) return c; const d = walk(c); if (d) return d; } return null; }; return walk(e); },
    querySelectorAll(sel) { const out = []; const walk = n => { for (const c of n.children) { if (sel.includes('[data-ham-key]') && c.attrs['data-ham-key'] != null) out.push(c); walk(c); } }; walk(e); return out; },
  };
  Object.defineProperty(e, 'innerHTML', { get() { return e._html; }, set(v) {
    e._html = String(v); e.children = [];
    const outer = e._html.match(/^<div class="(hive-dialog[^"]*)"[^>]*>([\\s\\S]*)<\\/div>$/);
    if (outer) { const d = el('div'); d.className = outer[1]; e.appendChild(d); d.innerHTML = outer[2]; return; }
    for (const m of e._html.matchAll(/<button([^>]*)>([\\s\\S]*?)<\\/button>/g)) {
      const b = el('button'); b.className = (m[1].match(/class="([^"]*)"/) || [, ''])[1]; b._html = m[2];
      b.disabled = /\\sdisabled\\b/.test(m[1]); if (/aria-disabled="true"/.test(m[1])) b.attrs['aria-disabled'] = 'true';
      if (/data-initial-focus/.test(m[1])) b.attrs['data-initial-focus'] = '';
      e.appendChild(b);
    }
  } });
  return e;
}
const body = el('body'); const panel = el('div');
global.document = { body, getElementById: id => id === 'activity-monitor-panel' ? panel : null, createElement: el, createTextNode: v => ({ nodeValue: v, parentNode: null }) };
global.hiveRememberModalOpen = x => x;
// The shared dialog wiring (tooltips-dialogs.js, covered by its own
// tests): Enter/click on the ok selector submits, Cancel closes null.
global.hiveWireDialog = (overlay, resolve, cancelValue, okSel, cancelSel, submit) => {
  const close = v => { overlay.remove(); resolve(v); };
  overlay.hiveDialogClose = () => close(cancelValue);
  const ok = overlay.querySelector(okSel); if (ok) ok.addEventListener('click', () => submit(close));
  const cancel = overlay.querySelector(cancelSel); if (cancel) cancel.addEventListener('click', () => close(cancelValue));
  return close;
};
function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'); }
const toasts = []; global.showToast = (t, k) => toasts.push({ t, k });
global.window = { _hiveUser: 'owner' };
var _stageTracker = { items: [] };
function stageTrackerItemKey(it) { return (it.repo || '') + '/' + (it.kind || '') + '#' + (Number(it.number) || 0); }
function stageTrackerRefOf(it) { return it.repo + '#' + it.number; }
function stageTrackerSentinelPreface() { return ''; }
function stageTrackerPRParked(it) { return Number(it.stage) === 85; }
function stageTrackerParkedNestedPRs() { return []; }
function stageTrackerNestedOpenPRs() { return []; }
function stageTrackerRunActions() { return { toasts: [], pending: [] }; }
function stageTrackerTargetKey(t) { return t ? stageTrackerItemKey(t) : ''; }
function stageTrackerItemState() { return ''; }
function stageTrackerItemByKey() { return null; }
function stageTrackerActionRequest(item, id) { return id === 'decide:delegate' ? { url: '/api/repos/' + item.repo + '/items/' + item.number + '/decide-delegated', body: { type: 'pr', head: item.head || '' } } : null; }
var _stageTrackerPending = [];
var _stageTrackerPlanPrefetch = {}; var _stageTrackerDelegateOpen = {};
`;

const src = [
  dom,
  constLine('STAGE_TRACKER_PLAN_BUTTON_DELEGATE'), constLine('STAGE_TRACKER_PLAN_BUTTON_MERGE'), constLine('STAGE_TRACKER_PLAN_MERGE_ACTIONS'),
  constLine('STAGE_TRACKER_PLAN_TITLE'), constLine('STAGE_TRACKER_PLAN_EYEBROW'), constLine('STAGE_TRACKER_PLAN_NEXT_MAX'), constLine('STAGE_TRACKER_PLAN_TITLE_MAX'),
  constLine('STAGE_TRACKER_PLAN_RAILS'), constLine('STAGE_TRACKER_PLAN_MERGE_WHY'), constLine('STAGE_TRACKER_DELEGATE_ALL_MIN'),
  constLine('STAGE_TRACKER_PLAN_PREFETCH_TTL_MS'), constLine('STAGE_TRACKER_PLAN_ASKING'), constLine('STAGE_TRACKER_PLAN_RETRY'),
  fn('hiveDialogEscape'), fn('stageTrackerPlanSentence'), fn('stageTrackerPlanTruncate'),
  fn('stageTrackerDelegatePlanView'), fn('stageTrackerDelegatePlanHtml'), fn('stageTrackerDelegatePlanVisibleText'),
  fn('stageTrackerDelegatePlanMergeAction'), fn('stageTrackerDelegatePlanOffersMerge'), fn('stageTrackerDelegatePlanMergeLabel'),
  fn('stageTrackerDelegatePlanMergeable'), fn('stageTrackerDelegatePlanMergeReason'),
  fn('stageTrackerDecisionTarget'), fn('stageTrackerDelegateTargets'),
  fn('stageTrackerPlanPrefetchKey'), fn('stageTrackerRequestDelegatePlan'), fn('stageTrackerPlanPrefetched'),
  fn('stageTrackerFetchDelegatePlan'), fn('stageTrackerPrefetchDelegatePlans'),
  fn('stageTrackerDelegatePlanSkeletonHtml'), fn('stageTrackerDelegatePlanErrorHtml'), fn('stageTrackerDelegatePlanDialog'),
  fn('stageTrackerDelegateButtonBusy'), fn('stageTrackerDelegatePreview'),
  fn('stageTrackerPendingSync'), fn('stageTrackerSetPending'), fn('stageTrackerClearPending'),
  'module.exports = { stageTrackerDelegatePreview, stageTrackerPrefetchDelegatePlans, stageTrackerDelegatePlanSkeletonHtml, body, panel, toasts, pending: () => _stageTrackerPending, resetCache: () => { _stageTrackerPlanPrefetch = {}; } };',
].join('\n');
const m = { exports: {} };
new Function('module', 'require', src)(m, require);
const T = m.exports;

const PLAN = { would: 'approve', summary: 'Approve o/r#8 and let the merge queue take it.', why: 'CI is green and a reviewer approved it.', consequences: ['Hive posts /lgtm.'], mergeable_as_is: false, alternatives: [] };
const item = { repo: 'o/r', kind: 'pr', number: 8, stage: 85, head: 'abc' };
function button() { const b = T.body.appendChild(Object.assign(document.createElement('button'), {})); b.className = 'ham-lead-act'; b.innerHTML = 'Let hive decide'; return b; }
function deferredFetch() {
  const calls = []; let release;
  const gate = new Promise(r => { release = r; });
  global.fetch = async (url, init) => { calls.push({ url, body: JSON.parse(init.body) }); const r = await gate; return r; };
  return { calls, release: (ok, data) => release({ ok, status: ok ? 200 : 502, json: async () => data }) };
}
const tick = () => new Promise(r => setImmediate(r));
const overlay = () => T.body.children.find(c => c.className === 'hive-dialog-overlay') || null;

test.beforeEach(() => { T.body.children = []; T.resetCache(); T.toasts.length = 0; });

test('click: busy button, in-flight row caption and an instant skeleton that fills with the plan', async () => {
  const f = deferredFetch();
  const btn = button();
  const p = T.stageTrackerDelegatePreview([item], { button: btn });
  await tick();
  assert.strictEqual(btn.disabled, true, 'the button is disabled while the plan is fetched');
  assert.strictEqual(btn.getAttribute('aria-busy'), 'true');
  assert.ok(btn.innerHTML.includes('Asking the hive…') && btn.innerHTML.includes('cue-acting-spin'), 'the label swaps to the working glyph + text: ' + btn.innerHTML);
  assert.ok(btn.classList.contains('ham-asking'));
  assert.deepStrictEqual(T.pending().map(x => x.caption), ['Asking the hive…'], 'the row wears the tracker\'s in-flight caption');
  const ov = overlay();
  assert.ok(ov, 'the preview opens before the plan is back');
  const dialog = ov.firstElementChild;
  assert.ok(dialog.innerHTML.includes('ham-plan-skeleton') && dialog.innerHTML.includes('aria-busy="true"'), 'it opens as a skeleton');
  assert.ok(dialog.innerHTML.includes('Let hive decide') && dialog.innerHTML.includes('o/r#8'), 'title and subject are real');
  assert.ok(dialog.innerHTML.includes('ham-plan-placeholder'), 'placeholder lines stand where the plan goes');
  assert.strictEqual(dialog.querySelector('.primary').disabled, true, 'Confirm is disabled on the skeleton');
  assert.strictEqual(dialog.querySelector('.cancel').disabled, false, 'Cancel is live on the skeleton');
  assert.strictEqual(f.calls.length, 1);
  assert.strictEqual(f.calls[0].body.head, 'abc', 'the head rides the request for the server cache key');
  f.release(true, PLAN);
  await tick(); await tick();
  assert.ok(!dialog.innerHTML.includes('ham-plan-skeleton'), 'the skeleton gives way to the plan');
  assert.ok(dialog.innerHTML.includes('Approve o/r#8'), dialog.innerHTML);
  assert.strictEqual(dialog.querySelector('.primary').disabled, false, 'Confirm is live once the plan is in');
  assert.strictEqual(btn.disabled, false, 'the button restores');
  assert.strictEqual(btn.getAttribute('aria-busy'), null);
  assert.strictEqual(btn.innerHTML, 'Let hive decide');
  assert.deepStrictEqual(T.pending(), [], 'the row caption lifts');
  dialog.querySelector('.primary').click();
  assert.deepStrictEqual(await p, { chosen: 'Let hive do that · previewed approve' });
  assert.strictEqual(overlay(), null, 'the dialog closes');
});

test('a second click while the preview is open is ignored', async () => {
  const f = deferredFetch();
  const btn = button();
  const first = T.stageTrackerDelegatePreview([item], { button: btn });
  const second = T.stageTrackerDelegatePreview([item], { button: btn });
  assert.strictEqual(await second, null, 'the double click runs nothing');
  await tick();
  assert.strictEqual(f.calls.length, 1, 'one plan request');
  assert.strictEqual(T.body.children.filter(c => c.className === 'hive-dialog-overlay').length, 1, 'one dialog');
  f.release(true, PLAN);
  await tick(); await tick();
  overlay().querySelector('.cancel').click();
  assert.strictEqual(await first, null);
  // Once closed, the row can be asked again (cache cleared so it fetches).
  T.resetCache();
  const f2 = deferredFetch();
  const again = T.stageTrackerDelegatePreview([item], { button: btn });
  await tick();
  assert.strictEqual(f2.calls.length, 1);
  f2.release(true, PLAN); await tick(); await tick();
  overlay().querySelector('.cancel').click();
  assert.strictEqual(await again, null);
});

test('a failed plan shows inline with Retry; Retry fetches again and fills', async () => {
  const f = deferredFetch();
  const btn = button();
  const p = T.stageTrackerDelegatePreview([item], { button: btn });
  await tick();
  f.release(false, { error: 'GitHub 502' });
  await tick(); await tick();
  const dialog = overlay().firstElementChild;
  assert.ok(dialog.innerHTML.includes('ham-plan-error') && dialog.innerHTML.includes('Could not preview: o/r#8 (GitHub 502)'), dialog.innerHTML);
  assert.ok(dialog.querySelector('.ham-plan-retry'), 'Retry is offered');
  assert.ok(dialog.querySelector('.cancel'), 'Cancel stays');
  assert.strictEqual(btn.disabled, false, 'the button restores on error');
  assert.strictEqual(T.toasts.length, 0, 'the error is in the dialog, not a toast');
  const f2 = deferredFetch();
  dialog.querySelector('.ham-plan-retry').click();
  await tick();
  assert.strictEqual(f2.calls.length, 1, 'Retry asks again');
  assert.ok(dialog.innerHTML.includes('ham-plan-skeleton'), 'Retry shows the skeleton again');
  f2.release(true, PLAN);
  await tick(); await tick();
  assert.ok(dialog.innerHTML.includes('Approve o/r#8'));
  dialog.querySelector('.primary').click();
  assert.deepStrictEqual(await p, { chosen: 'Let hive do that · previewed approve' });
});

test('a plan prefetched as the options came into view answers the click without a request', async () => {
  const f = deferredFetch();
  T.stageTrackerPrefetchDelegatePlans([item]);
  await tick();
  assert.strictEqual(f.calls.length, 1);
  assert.strictEqual(f.calls[0].body.prefetch, true, 'the prefetch says so');
  T.stageTrackerPrefetchDelegatePlans([item]);
  assert.strictEqual(f.calls.length, 1, 'a warm row is not asked twice');
  f.release(true, PLAN);
  await tick(); await tick();
  const btn = button();
  const p = T.stageTrackerDelegatePreview([item], { button: btn });
  await tick(); await tick();
  assert.strictEqual(f.calls.length, 1, 'the click reuses the prefetched plan');
  const dialog = overlay().firstElementChild;
  assert.ok(dialog.innerHTML.includes('Approve o/r#8'), 'the plan is in at once');
  dialog.querySelector('.cancel').click();
  assert.strictEqual(await p, null);
  // A new head is a new plan.
  const f2 = deferredFetch();
  T.stageTrackerPrefetchDelegatePlans([Object.assign({}, item, { head: 'def' })]);
  await tick();
  assert.strictEqual(f2.calls.length, 1, 'a pushed head is fetched afresh');
  f2.release(true, PLAN);
});

test('the skeleton is honest markup: real title, status role for the working line, disabled primary', () => {
  const h = T.stageTrackerDelegatePlanSkeletonHtml([item], '');
  assert.ok(h.includes('class="hive-dialog-title">Let hive decide<'));
  assert.ok(h.includes('role="status"') && h.includes('toast-spinner cue-acting-spin'), 'the working line reuses the tracker\'s acting glyph');
  assert.ok(/class="[^"]*primary" disabled aria-disabled="true"/.test(h));
  assert.ok(!/class="[^"]*cancel"[^>]*disabled/.test(h), 'Cancel is never disabled');
  assert.ok(!h.includes('style='), 'no inline styles');
});
