package dashboard

import (
	"strings"
	"testing"
)

// The activity monitor row menu's "Accept triage" (Hub Admin request): shown
// only on an issue the chevron calls "Triage needed" (015, or the
// needs-triage pill on another stage), never on a PR; it posts to
// /items/{n}/accept-triage and re-renders the row as 010 Actionable with the
// 👤 next-move gone before the tracker's next payload confirms it. The pure
// helpers run under node like the rest of the row-actions suite.

func TestStageTrackerAcceptTriageMenuVisibility(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`'/items/' + n + '/accept-triage'`,
		`id: 'accept-triage', label: 'Accept triage'`,
		`'accept-triage': 'Accept triage'`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html lacks %q", want)
		}
	}
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+`
const ids = as => as.map(a => a.id);
const ctx = { user: true, canHold: true, canHuman: true };

// The primary case: an issue at 015 "Triage needed".
const triage = { repo: 'o/r', kind: 'issue', number: 1, title: 'From an untrusted reporter', url: 'https://github.com/o/r/issues/1', stage: 15, modifiers: [] };
let as = stageTrackerRowActions(triage, ctx);
assert.deepStrictEqual(ids(as), ['hold', 'retry', 'accept-triage', 'needs-human', 'open', 'copy'], 'Accept triage sits above Mark needs-human…');
const accept = as.find(a => a.id === 'accept-triage');
assert.strictEqual(accept.label, 'Accept triage');
assert.strictEqual(accept.enabled, true);
assert.strictEqual(accept.hint, 'triage/accepted', 'the hint names the label the click applies');

// The needs-triage pill on another stage (a claimed issue the gate still holds) also offers it.
assert.ok(ids(stageTrackerRowActions({ ...triage, stage: 20, modifiers: ['needs-triage'] }, ctx)).includes('accept-triage'));

// Not on a triaged issue, a parked issue, an open issue, or any PR.
assert.ok(!ids(stageTrackerRowActions({ ...triage, stage: 10 }, ctx)).includes('accept-triage'), '010 Actionable is already triaged');
assert.ok(!ids(stageTrackerRowActions({ ...triage, stage: 45, modifiers: ['needs-decision'] }, ctx)).includes('accept-triage'));
assert.ok(!ids(stageTrackerRowActions({ ...triage, stage: 0 }, ctx)).includes('accept-triage'));
assert.ok(!ids(stageTrackerRowActions({ ...triage, kind: 'pr', stage: 15 }, ctx)).includes('accept-triage'), 'a PR has no triage state');
assert.ok(!ids(stageTrackerRowActions({ ...triage, kind: 'pr', stage: 60, modifiers: ['needs-triage'] }, ctx)).includes('accept-triage'));
assert.strictEqual(stageTrackerTriageNeeded(null), false);

// A reader sees it disabled with the write-access reason (same gate as Hold).
as = stageTrackerRowActions(triage, { user: false, canHold: false, canHuman: false });
const ro = as.find(a => a.id === 'accept-triage');
assert.strictEqual(ro.enabled, false); assert.strictEqual(ro.why, STAGE_TRACKER_WHY_WRITE);

// Menu markup carries the item.
const menu = stageTrackerMenuHtml(stageTrackerRowActions(triage, ctx), 'o/r#1');
assert.ok(menu.includes('data-ham-act="accept-triage"'));
assert.ok(menu.indexOf('data-ham-act="accept-triage"') < menu.indexOf('data-ham-act="needs-human"'));

// Endpoint mapping: issues only, no body to fill in.
assert.deepStrictEqual(stageTrackerActionRequest(triage, 'accept-triage', {}), { url: '/api/repos/o/r/items/1/accept-triage', body: {} });
assert.strictEqual(stageTrackerActionRequest({ ...triage, kind: 'pr' }, 'accept-triage', {}), null);
assert.strictEqual(stageTrackerActionLabel('accept-triage'), 'Accept triage');

// The optimistic row: 015 → 010 Actionable, pill and 👤 gone; other stages keep their stage and only drop the pill.
const after = stageTrackerTriageAccepted({ ...triage, actor: 'human', caption: 'Triage needed', modifiers: ['needs-triage'] });
assert.strictEqual(after.stage, 10); assert.strictEqual(after.stage_slug, 'actionable'); assert.strictEqual(after.stage_label, 'stage/010-actionable');
assert.strictEqual(after.actor, ''); assert.strictEqual(after.caption, '');
assert.deepStrictEqual(after.modifiers, []);
assert.strictEqual(triage.stage, 15, 'the input is not mutated');
const claimed = stageTrackerTriageAccepted({ ...triage, stage: 20, modifiers: ['needs-triage', 'hold'] });
assert.strictEqual(claimed.stage, 20); assert.deepStrictEqual(claimed.modifiers, ['hold']);

// A click: one POST, the item in the live payload is patched and re-rendered before the refetch; an error leaves it alone and toasts.
const calls = [];
let rendered = 0; function renderStageTracker(d) { rendered++; }
_stageTracker = { items: [{ ...triage }, { repo: 'o/r', kind: 'issue', number: 2, stage: 15, modifiers: [] }] };
global.fetch = async (url, init) => { calls.push({ url, init }); const fail = url.includes('/items/2/'); return { ok: !fail, status: fail ? 403 : 200, json: async () => fail ? { error: 'write permission required' } : { ok: true, minStatusSeq: 5, added: ['triage/accepted'], removed: ['needs-triage'] } }; };
const toasts = [];
global.showToast = (msg, type) => { toasts.push(type + ':' + msg); return null; }; global.dismissToast = () => {};
function stageTrackerProgressToast(label, n) { return { update() {}, done(msg, type) { toasts.push('done:' + type + ':' + msg); } }; }
let refreshed = 0; function stageTrackerAfterActions() { refreshed++; }
(async () => {
  const res = await stageTrackerRunActions([_stageTracker.items[0], _stageTracker.items[1]], 'accept-triage', {});
  assert.deepStrictEqual(calls.map(c => c.url), ['/api/repos/o/r/items/1/accept-triage', '/api/repos/o/r/items/2/accept-triage']);
  assert.ok(calls.every(c => c.init.method === 'POST'));
  assert.deepStrictEqual(res.map(r => r.ok), [true, false]);
  assert.strictEqual(_stageTracker.items[0].stage, 10, 'the accepted row reads Actionable at once');
  assert.strictEqual(_stageTracker.items[1].stage, 15, 'the refused row is left as it was');
  assert.strictEqual(rendered, 1, 'one optimistic re-render for the success');
  assert.strictEqual(refreshed, 1, 'then the usual refetch');
  assert.strictEqual(toasts[toasts.length - 1], 'done:error:Accept triage: 1/2 done. Failed: o/r#2 — write permission required', 'the failure is toasted, never a native dialog');
})().catch(e => { console.error(e); process.exit(1); });
`)
}
