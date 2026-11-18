// hivecommons/hive#18916: the per-kick outcome UI. The quoted agent text is
// escaped, a read-only session gets no history control, and the one-line
// summary reads "last kick: opened PR #N 6m ago".
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const src = fs.readFileSync(path.join(__dirname, 'static-src', 'js', 'kick-outcomes.js'), 'utf8');
const esc = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
// pillAge and formatDashboardDateTime are the page's own formatters: stubbed.
// fetch and document are absent from the node harness, so only the pure
// builders run.
const api = new Function('esc', 'pillAge', 'formatDashboardDateTime',
  src + '\nreturn { _kickOutcomes, kickOutcomeLastText, kickOutcomeRowHtml, kickOutcomeSlotHtml };')(esc, () => '6m ago', t => 'T(' + t + ')');

test('one-line summary', () => {
  assert.strictEqual(api.kickOutcomeLastText({ summary: 'opened PR #12', at: '2026-11-17T12:00:00Z' }), 'last kick: opened PR #12 6m ago');
  assert.strictEqual(api.kickOutcomeLastText(null), '');
});

test('a quoted stand-down is escaped and labelled as the agent\'s words', () => {
  const h = api.kickOutcomeRowHtml({ kind: 'stood_down', summary: 'stood down', started_at: '2026-11-17T12:00:00Z', trigger_kind: 'governor', quote: '<img src=x onerror=alert(1)> "quoted"' });
  assert.ok(!h.includes('<img'), h);
  assert.ok(h.includes('&lt;img'), h);
  assert.ok(h.includes('agent said:'), h);
  assert.ok(h.includes('<q class="ko-quote">'), h);
});

test('a refusal shows its reason code and reason', () => {
  const h = api.kickOutcomeRowHtml({ kind: 'refused', summary: 'refused (paused)', refusal_code: 'paused', trigger_kind: 'dashboard', trigger_actor: 'alice', quote: 'it is paused', started_at: '2026-11-17T12:00:00Z' });
  assert.ok(h.includes('dashboard (alice)') && h.includes('<span class="ko-code">paused</span>') && h.includes('reason:'), h);
});

test('read-only: the line shows, the history control does not', () => {
  api._kickOutcomes.latest = { scanner: { kind: 'opened_pr', summary: 'opened PR #12', at: '2026-11-17T12:00:00Z' } };
  api._kickOutcomes.detail = false;
  const h = api.kickOutcomeSlotHtml('scanner');
  assert.ok(h.includes('last kick: opened PR #12 6m ago'), h);
  assert.ok(!h.includes('toggleKickOutcomes') && !h.includes('<ul'), h);
});

test('owner: toggle, then the last kicks', () => {
  api._kickOutcomes.detail = true;
  assert.ok(api.kickOutcomeSlotHtml('scanner').includes('data-action="toggleKickOutcomes"'));
  api._kickOutcomes.open.scanner = true;
  assert.ok(api.kickOutcomeSlotHtml('scanner').includes('Loading'));
  api._kickOutcomes.kicks.scanner = [{ kind: 'opened_pr', summary: 'opened PR #12', trigger_kind: 'governor', started_at: '2026-11-17T12:00:00Z' }];
  assert.ok(api.kickOutcomeSlotHtml('scanner').includes('<ul class="kick-outcome-list">'));
  assert.strictEqual(api.kickOutcomeSlotHtml('nobody'), '');
});

test('the slot name is escaped', () => {
  api._kickOutcomes.latest['a"b'] = { kind: 'refused', summary: 'refused (paused)', at: '2026-11-17T12:00:00Z' };
  assert.ok(!api.kickOutcomeSlotHtml('a"b').includes('"a"b"'));
});

test('no new interval timer', () => {
  assert.ok(!/setInterval\s*\(/.test(src));
});
