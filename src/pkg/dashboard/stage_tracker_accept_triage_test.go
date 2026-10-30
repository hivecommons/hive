package dashboard

import (
	"strings"
	"testing"
)

// The activity monitor row menu's "Accept triage" (Hub Admin request): shown
// only on an issue the chevron calls "Triage needed" (015, the needs-triage
// pill on another stage, or a label-less 000-new issue the reporter-trust
// gate holds for a maintainer — hivecommons/hive#15215), never on a PR; it
// heads the menu, its hint says what it writes and as whom, it posts to
// /items/{n}/accept-triage and re-renders the row as 010 Actionable (a
// 000-new row: waiting on the hive) with the 👤 next-move gone before the
// tracker's next payload confirms it. The pure helpers run under node like
// the rest of the row-actions suite.

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
const BT = String.fromCharCode(96);
const NOW = Date.parse('2026-10-28T20:00:00Z');
const ctx = { user: true, canHold: true, canHuman: true, login: 'octo', nowMs: NOW, governor: { nextKickAt: '2026-10-28T20:13:20Z' } };

// The primary case: an issue at 015 "Triage needed".
const triage = { repo: 'o/r', kind: 'issue', number: 1, title: 'From an untrusted reporter', url: 'https://github.com/o/r/issues/1', stage: 15, modifiers: [] };
let as = stageTrackerRowActions(triage, ctx);
assert.deepStrictEqual(ids(as), ['accept-triage', 'hold', 'retry', 'needs-human', 'open', 'copy'], 'Accept triage heads the menu');
const accept = as.find(a => a.id === 'accept-triage');
assert.strictEqual(accept.label, 'Accept triage');
assert.strictEqual(accept.enabled, true);
assert.strictEqual(accept.hint, 'adds ' + BT + 'triage/accepted' + BT + ' as @octo; the hive takes it on its next pass (≈13 min)', 'the hint names the label, the actor and the next pass');
assert.strictEqual(as.find(a => a.id === 'hold').sep, true, 'a separator parts the lead from the generic items');

// Without a login or a published next kick the hint still reads.
assert.strictEqual(stageTrackerTriageHint(triage, { user: true, canHold: true }, NOW, null), 'adds ' + BT + 'triage/accepted' + BT + ' as you; the hive takes it on its next pass');
assert.strictEqual(stageTrackerNextPassWord(-5), 'due now');
assert.strictEqual(stageTrackerNextPassWord(20000), '≈1 min');
assert.strictEqual(stageTrackerNextPassWord(null), '');

// The needs-triage pill on another stage (a claimed issue the gate still holds) also offers it.
assert.ok(ids(stageTrackerRowActions({ ...triage, stage: 20, modifiers: ['needs-triage'] }, ctx)).includes('accept-triage'));

// A label-less 000-new issue the reporter-trust gate holds for a maintainer
// (hivecommons/hive#15215) offers it too; its hint reads the configured
// label and the row's own next pass. A 000-new issue the hive moves next
// (reporter admitted) does not: nothing to accept.
const fresh = { ...triage, stage: 0, stage_slug: '000-new', actor: 'maintainer', caption: 'Triage needed — accept or close',
  new: { next_mover: 'maintainer', next_mover_text: 'a maintainer\'s triage (add ' + BT + 'triage/ok' + BT + ')', next_pass_at: '2026-10-28T20:07:00Z', gates: [{ name: 'triage', state: 'next', text: 'a maintainer accepts triage (triage/ok)' }, { name: 'actionable', state: 'later', text: 'the stage-label pass marks it 010 actionable' }, { name: 'claimed', state: 'later', text: 'the scanner claims it (020)' }], text: 'Triage needed — accept or close' } };
as = stageTrackerRowActions(fresh, ctx);
assert.strictEqual(ids(as)[0], 'accept-triage', 'a 000-new reporter-trust hold leads with Accept triage');
assert.strictEqual(as[0].hint, 'adds ' + BT + 'triage/ok' + BT + ' as @octo; the hive takes it on its next pass (≈7 min)');
assert.ok(!ids(stageTrackerRowActions({ ...fresh, new: { ...fresh.new, next_mover: 'hive' } }, ctx)).includes('accept-triage'), 'an admitted reporter\'s new issue needs no triage');
assert.ok(!ids(stageTrackerRowActions({ ...fresh, kind: 'pr', new: { next_mover: 'maintainer' } }, ctx)).includes('accept-triage'));
assert.ok(!ids(stageTrackerRowActions({ ...fresh, new: null }, ctx)).includes('accept-triage'), 'a plain Open row without the narration is not a triage hold');
const freshAfter = stageTrackerTriageAccepted(fresh);
assert.strictEqual(freshAfter.stage, 0, 'no stage label to move yet');
assert.strictEqual(freshAfter.new.next_mover, 'hive'); assert.strictEqual(freshAfter.actor, 'hive');
assert.strictEqual(freshAfter.caption, 'New · waiting for the stage-label pass (→ 010 actionable), then the scanner');
assert.deepStrictEqual(freshAfter.new.gates.map(g => g.name + ':' + g.state), ['triage:done', 'actionable:next', 'claimed:later']);
assert.strictEqual(freshAfter.new.gates[0].text, 'triage accepted (triage/ok)');
assert.strictEqual(fresh.new.next_mover, 'maintainer', 'the input is not mutated');
assert.ok(!stageTrackerTriageNeeded(freshAfter), 'once accepted the menu no longer offers it');

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
  assert.strictEqual(toasts[toasts.length - 1], 'done:error:Accepted 1 of 2 issues. Failed: o/r#2 — write permission required', 'the failure is toasted, never a native dialog');
})().catch(e => { console.error(e); process.exit(1); });
`)
}
