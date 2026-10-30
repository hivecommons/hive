package dashboard

import (
	"strings"
	"testing"
)

// Adopt now / Reroute (hivecommons/hive#15038): a PR waiting on a
// contributor's clanker offers, in its ⋯ menu and in the Needs-human
// popover, "Adopt now — hive takes it over" with its consequence line and
// "Reroute to another clanker" with the online clankers as a submenu (plus
// "any available"); each is disabled with the server's reason; the confirms
// honour the don't-ask-again key; the requests hit the decision API
// (`adopt` + force / `reroute` + to); the toast reads the server's result;
// a rerouted row shows the "rerouted → @x" pill with the presence dot.

const stageTrackerTakeoverFixture = `
const clankers = [
  { login: 'Danathar', hive_name: 'danathar-hive', online_since: '2026-10-29T08:00:00Z', load: 1 },
  { login: 'bee2', hive_name: 'bee2#2', online_since: '2026-10-29T09:30:00Z', load: 0 },
];
const takeover = { author: 'Danathar', author_online: false, adopt_now: true, reroute: true, clankers, grace_elapsed: true };
const waiting = { repo: 'hivecommons/hive', kind: 'pr', number: 15038, title: 'Fix the thing', url: 'u15038', stage: 72, stage_slug: 'returned-to-contributor', caption: 'Returned to contributor · waiting on @Danathar', author: 'Danathar', modifiers: [], takeover };
const nobody = Object.assign({}, waiting, { number: 15039, url: 'u15039', takeover: { author: 'Danathar', adopt_now: true, reroute: false, reroute_reason: 'no clankers online — use Adopt now', clankers: [] } });
const held = Object.assign({}, waiting, { number: 15040, url: 'u15040', modifiers: ['hold'], takeover: { author: 'Danathar', adopt_now: false, adopt_now_reason: 'release the hold first', reroute: false, reroute_reason: 'release the hold first' } });
const rerouted = Object.assign({}, waiting, { number: 15041, url: 'u15041', takeover: Object.assign({}, takeover, { rerouted: { to: 'bee2', by: 'clubanderson', since: '2026-10-29T10:00:00Z', reason: 'reviewer requested changes', target_online: true, text: 'Rerouted to @bee2 by @clubanderson 5m ago — reviewer requested changes' } }) });
const hivePR = { repo: 'hivecommons/hive', kind: 'pr', number: 15042, url: 'u15042', stage: 72, modifiers: [] };
const issue = { repo: 'hivecommons/hive', kind: 'issue', number: 15000, url: 'u15000', stage: 72, mirrored: true, modifiers: [], linked_prs: [ { number: 15038, title: 'Fix the thing', url: 'u15038', stage: 72, modifiers: [], author: 'Danathar', takeover } ] };
const ctx = { user: true, canHold: true, canHuman: true, nowMs: Date.parse('2026-10-29T10:00:00Z') };
const ids = acts => acts.map(a => a.id);
`

func TestStageTrackerTakeoverMenu(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`'decide:adopt-now': 'Adopt now — hive takes it over', 'decide:reroute': 'Reroute to another clanker',`,
		`.ham-badge-reroute {`, `.ham-badge-reroute .ham-presence-dot {`, `.ham-badge-reroute.ham-reroute-online .ham-presence-dot {`,
		`.ham-menu-text {`, `.ham-menu-note {`,
		`stageTrackerReroutePillHtml(item)`,
		`'decide:adopt-now': 'Adopting now…', 'decide:reroute': 'Rerouting…',`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing takeover wiring %q", want)
		}
	}
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+`
function agentNowAgo(ms) { return Math.round(ms / 60000) + 'm ago'; }
`+stageTrackerTakeoverFixture+`
// The waiting PR's menu: Adopt now (with the consequence line) and Reroute (submenu) sit after Hold.
let acts = stageTrackerRowActions(waiting, ctx);
assert.deepStrictEqual(ids(acts), ['decide:adopt-now', 'decide:reroute', 'hold', 'retry', 'needs-human', 'open', 'copy']);
const adoptNow = acts[0];
assert.strictEqual(adoptNow.label, 'Adopt now — hive takes it over…');
assert.strictEqual(adoptNow.note, STAGE_TRACKER_ADOPT_NOW_CONSEQUENCE);
assert.ok(adoptNow.note.indexOf("closes the contributor's PR after hive opens its copy") === 0, adoptNow.note);
assert.ok(adoptNow.note.indexOf('their commits + sign-off are preserved') > 0, adoptNow.note);
assert.strictEqual(adoptNow.enabled, true); assert.strictEqual(adoptNow.danger, true);
const reroute = acts[1];
assert.strictEqual(reroute.label, 'Reroute to another clanker');
assert.strictEqual(reroute.enabled, true);
assert.deepStrictEqual(ids(reroute.submenu), ['decide:reroute:Danathar', 'decide:reroute:bee2', 'decide:reroute:any']);
assert.strictEqual(reroute.submenu[0].label, '@Danathar · danathar-hive · online 120m ago · 1 in hand');
assert.strictEqual(reroute.submenu[1].label, '@bee2 · bee2#2 · online 30m ago · idle');
assert.strictEqual(reroute.submenu[2].label, 'any available (2 online)');
assert.ok(reroute.submenu.every(o => o.enabled));
// The menu markup: the consequence line under the label, the submenu nested.
const menu = stageTrackerMenuHtml(acts, 'hivecommons/hive#15038');
assert.ok(menu.indexOf('<span class="ham-menu-text"><span>Adopt now — hive takes it over…</span><span class="ham-menu-note">closes the contributor') >= 0, menu);
assert.ok(menu.indexOf('data-ham-act="decide:reroute" aria-haspopup="menu"') >= 0, menu);
assert.ok(menu.indexOf('data-ham-act="decide:reroute:bee2"') >= 0, menu);
assert.ok(menu.indexOf('data-ham-act="decide:reroute:any"') >= 0, menu);
assert.ok(menu.indexOf('style=') < 0, 'no inline styles');

// No clanker online: Reroute is disabled with the server's reason; Adopt now still offered.
acts = stageTrackerRowActions(nobody, ctx);
const noReroute = acts.find(a => a.id === 'decide:reroute');
assert.strictEqual(noReroute.enabled, false);
assert.strictEqual(noReroute.why, 'no clankers online — use Adopt now');
assert.ok(noReroute.submenu.every(o => !o.enabled && o.why === 'no clankers online — use Adopt now'));
assert.deepStrictEqual(ids(noReroute.submenu), ['decide:reroute:any']);
assert.strictEqual(acts.find(a => a.id === 'decide:adopt-now').enabled, true);
const noMenu = stageTrackerMenuHtml(acts, '');
assert.ok(noMenu.indexOf('data-ham-act="decide:reroute" aria-haspopup="menu" aria-expanded="false" tabindex="-1" aria-disabled="true" title="no clankers online — use Adopt now"') >= 0, noMenu);

// Held: both disabled with "release the hold first".
acts = stageTrackerRowActions(held, ctx);
assert.strictEqual(acts.find(a => a.id === 'decide:adopt-now').why, 'release the hold first');
assert.strictEqual(acts.find(a => a.id === 'decide:reroute').why, 'release the hold first');

// A read-only viewer sees both greyed with the sign-in reason.
acts = stageTrackerRowActions(waiting, { user: false, canHold: false, canHuman: false });
assert.strictEqual(acts.find(a => a.id === 'decide:adopt-now').why, STAGE_TRACKER_WHY_WRITE);
assert.strictEqual(acts.find(a => a.id === 'decide:reroute').why, STAGE_TRACKER_WHY_WRITE);

// A hive-authored 072 PR (no takeover) and an issue row offer neither.
assert.deepStrictEqual(ids(stageTrackerRowActions(hivePR, ctx)).filter(id => id.indexOf('decide:adopt-now') === 0 || id.indexOf('decide:reroute') === 0), []);
assert.deepStrictEqual(ids(stageTrackerRowActions(issue, ctx)).filter(id => id.indexOf('decide:adopt-now') === 0 || id.indexOf('decide:reroute') === 0), []);

// The takeover target: the PR row itself, or the issue's nested PR.
assert.strictEqual(stageTrackerTakeoverTarget(waiting), waiting);
assert.strictEqual(stageTrackerTakeoverTarget(hivePR), null);
_stageTracker = { items: [issue] };
assert.strictEqual(stageTrackerTakeoverTarget(issue).number, 15038);
assert.strictEqual(stageTrackerTakeoverTarget(issue).takeover, takeover);

// Action ids: "decide:reroute:<login>" acts as "decide:reroute".
assert.strictEqual(stageTrackerActionBase('decide:reroute:bee2'), 'decide:reroute');
assert.strictEqual(stageTrackerActionBase('decide:adopt-now'), 'decide:adopt-now');
assert.strictEqual(stageTrackerRerouteTargetOf('decide:reroute:bee2'), 'bee2');
assert.strictEqual(stageTrackerRerouteTargetOf('decide:reroute'), '');
assert.strictEqual(stageTrackerActionLabel('decide:reroute:bee2'), 'Reroute to another clanker');
assert.strictEqual(stageTrackerActingCaption('decide:reroute:bee2'), 'Rerouting…');
assert.strictEqual(stageTrackerActingCaption('decide:adopt-now'), 'Adopting now…');

// Requests: the decision API, attributed to the operator.
let req = stageTrackerActionRequest(waiting, 'decide:adopt-now', {});
assert.strictEqual(req.url, '/api/repos/hivecommons/hive/items/15038/decision');
assert.deepStrictEqual(req.body, { decision: 'adopt', force: true, comment: '', type: 'pr' });
req = stageTrackerActionRequest(waiting, 'decide:reroute:bee2', { to: 'bee2' });
assert.deepStrictEqual(req.body, { decision: 'reroute', to: 'bee2', comment: '', type: 'pr' });
req = stageTrackerActionRequest(waiting, 'decide:reroute:any', {});
assert.strictEqual(req.body.to, 'any');
req = stageTrackerActionRequest(waiting, 'decide:reroute', { to: 'Danathar' });
assert.strictEqual(req.body.to, 'Danathar');

// The rerouted pill: target + presence dot; nothing on a plain row.
const pill = stageTrackerReroutePillHtml(rerouted);
assert.ok(pill.indexOf('class="ham-badge ham-badge-reroute ham-reroute-online"') >= 0, pill);
assert.ok(pill.indexOf('<span class="ham-presence-dot" aria-hidden="true"></span>rerouted → @bee2</span>') >= 0, pill);
assert.ok(pill.indexOf('title="Rerouted to @bee2 by @clubanderson 5m ago') >= 0, pill);
assert.ok(pill.indexOf('(@bee2 online)') >= 0, pill);
assert.strictEqual(stageTrackerReroutePillHtml(waiting), '');
const offline = stageTrackerReroutePillHtml(Object.assign({}, rerouted, { takeover: Object.assign({}, rerouted.takeover, { rerouted: Object.assign({}, rerouted.takeover.rerouted, { target_online: false }) }) }));
assert.ok(offline.indexOf('ham-reroute-online') < 0 && offline.indexOf('(@bee2 offline)') >= 0, offline);
// A rerouted row's Reroute item says so.
assert.ok(stageTrackerRowActions(rerouted, ctx).find(a => a.id === 'decide:reroute').note.indexOf('rerouted → @bee2 already') === 0);

// Toasts: the server's result, the operator's own action (not "Hive decided").
const adoptedResult = { action: 'adopted', target: { repo: 'hivecommons/hive', kind: 'pr', number: 15038, url: 'u15038' }, outcome: "Adopted → #15050 (hive's copy), contributor PR will close when #15050 merges.", next: "The hive rebases and fixes CI on #15050; the author's commits and sign-off are preserved." };
let text = stageTrackerResultText(adoptedResult, waiting, { id: 'decide:adopt-now' });
assert.strictEqual(text.head, 'Adopt now — hive takes it over on hivecommons/hive#15038: adopted');
assert.strictEqual(text.outcome, "Adopted → #15050 (hive's copy), contributor PR will close when #15050 merges.");
assert.strictEqual(text.type, 'success');
text = stageTrackerResultText({ action: 'rerouted', target: adoptedResult.target, outcome: 'Rerouted → @bee2 (dispatched now, interrupt).', next: "@bee2's hive fixes the PR or opens a replacement carrying the author's commits and sign-off. If it has not picked the work up within 30m the hive falls back to Adopt now." }, waiting, { id: 'decide:reroute:bee2' });
assert.strictEqual(text.head, 'Reroute to another clanker on hivecommons/hive#15038: rerouted');
assert.ok(text.outcome.indexOf('Rerouted → @bee2') === 0, text.outcome);
assert.ok(text.next.indexOf('falls back to Adopt now') > 0, text.next);
// A delegated adoption still reads as the hive's decision: the outcome
// leads (#15126) and "Hive decided on …" is the second line.
const delegated = stageTrackerResultText(Object.assign({ rule: 'grace-elapsed-adopt-now' }, adoptedResult), waiting, { id: 'decide:delegate' });
assert.strictEqual(delegated.head, adoptedResult.outcome);
assert.ok(delegated.outcome.indexOf('Hive decided on') === 0, delegated.outcome);
console.log('takeover menu ok');
`)
}

func TestStageTrackerTakeoverConfirmsAndPopover(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerDecisionPopPreamble(t, html)+stageTrackerConfirmOptOutJS(t, html)+stageTrackerTakeoverFixture+`
(async () => {
  // Adopt now: a confirm carrying the consequence line and the don't-ask-again box; keyed hive.tracker.confirm.decide:adopt-now.
  assert.deepStrictEqual(await stageTrackerActionPayload([waiting], 'decide:adopt-now'), {});
  let d = dialogs.pop();
  assert.strictEqual(d.kind, 'confirm'); assert.strictEqual(d.cfg.title, 'Adopt now'); assert.strictEqual(d.cfg.confirmLabel, 'Adopt now');
  assert.ok(d.cfg.message.indexOf(STAGE_TRACKER_ADOPT_NOW_CONSEQUENCE) > 0, d.cfg.message);
  assert.ok(d.cfg.message.indexOf('without waiting for the offline grace window or the maintainer policy') > 0, d.cfg.message);
  assert.ok(d.cfg.message.indexOf('clanker is online') < 0, 'offline author: no online line');
  assert.strictEqual(d.cfg.check.label, STAGE_TRACKER_CONFIRM_CHECK_LABEL);
  // An online author: the dialog says the comment tells its clanker politely.
  await stageTrackerActionPayload([Object.assign({}, waiting, { takeover: Object.assign({}, takeover, { author_online: true }) })], 'decide:adopt-now');
  assert.ok(dialogs.pop().cfg.message.indexOf("The author's clanker is online: the adoption comment tells it, politely") > 0);
  // Ticked: the key is stored and the next Adopt now runs without a dialog.
  dialogAnswer = { checked: true };
  await stageTrackerActionPayload([waiting], 'decide:adopt-now'); dialogs.length = 0;
  assert.strictEqual(store['hive.tracker.confirm.decide:adopt-now'], 'skip');
  assert.deepStrictEqual(await stageTrackerActionPayload([waiting], 'decide:adopt-now'), {});
  assert.strictEqual(dialogs.length, 0, 'skipped confirm raises no dialog');
  dialogAnswer = true;
  // Cancelled: null.
  dialogAnswer = null;
  assert.strictEqual(await stageTrackerActionPayload([waiting], 'decide:reroute:bee2'), null);
  dialogAnswer = true;

  // Reroute from the submenu: one confirm (keyed decide:reroute for every target), payload carries the target.
  assert.deepStrictEqual(await stageTrackerActionPayload([waiting], 'decide:reroute:bee2'), { to: 'bee2' });
  d = dialogs.pop();
  assert.strictEqual(d.kind, 'confirm'); assert.strictEqual(d.cfg.title, 'Reroute to another clanker'); assert.strictEqual(d.cfg.confirmLabel, 'Reroute');
  assert.ok(d.cfg.message.indexOf("to @bee2's clanker") > 0, d.cfg.message);
  assert.ok(d.cfg.message.indexOf('@Danathar is told by comment') > 0, d.cfg.message);
  assert.ok(d.cfg.message.indexOf('falls back to Adopt now') > 0, d.cfg.message);
  assert.strictEqual(d.cfg.check.label, STAGE_TRACKER_CONFIRM_CHECK_LABEL);
  assert.deepStrictEqual(await stageTrackerActionPayload([waiting], 'decide:reroute:any'), { to: 'any' });
  assert.ok(dialogs.pop().cfg.message.indexOf('to any available clanker') > 0);
  dialogAnswer = { checked: true };
  await stageTrackerActionPayload([waiting], 'decide:reroute:Danathar'); dialogs.length = 0;
  assert.strictEqual(store['hive.tracker.confirm.decide:reroute'], 'skip');
  dialogAnswer = true;
  assert.deepStrictEqual(await stageTrackerActionPayload([waiting], 'decide:reroute:bee2'), { to: 'bee2' });
  assert.strictEqual(dialogs.length, 0);
  assert.deepStrictEqual(stageTrackerSkippedConfirms().sort(), ['decide:adopt-now', 'decide:reroute']);
  stageTrackerResetConfirms();

  // Reroute from the popover (no target in the id): a required prompt listing the online clankers — never skipped — then the confirm.
  dialogAnswer = 'bee2';
  assert.deepStrictEqual(await stageTrackerActionPayload([waiting], 'decide:reroute'), { to: 'bee2' });
  const confirm = dialogs.pop(), prompt = dialogs.pop();
  assert.strictEqual(prompt.kind, 'prompt');
  assert.deepStrictEqual(prompt.cfg.options.map(o => o.value), ['Danathar', 'bee2', 'any']);
  assert.strictEqual(prompt.cfg.validate('@bee2'), true);
  assert.strictEqual(prompt.cfg.validate('any'), true);
  assert.strictEqual(prompt.cfg.validate(''), true);
  assert.strictEqual(prompt.cfg.validate('stranger'), '@stranger is not online now — pick a listed clanker or "any"');
  assert.strictEqual(prompt.cfg.check, undefined, 'required input: no opt-out box');
  assert.strictEqual(confirm.kind, 'confirm');
  stageTrackerSetConfirmSkip('decide:reroute', true);
  dialogAnswer = '';
  assert.deepStrictEqual(await stageTrackerActionPayload([waiting], 'decide:reroute'), { to: 'any' }, 'an empty answer means any');
  assert.strictEqual(dialogs.length, 1, 'the prompt still shows when the confirm is skipped');
  dialogs.length = 0; dialogAnswer = true; stageTrackerResetConfirms();

  // The Needs-human popover: the server's waiting-on-contributor options resolve against the row's takeover.
  const decision = { question: 'Waiting on @Danathar — adopt now, reroute, or wait?', asked_by: 'the hive', since: '2026-10-29T09:00:00Z', blocker: 'Returned to contributor; the offline grace has elapsed.',
    options: [
      { action: 'decide:adopt-now', label: 'Adopt now — hive takes it over', enabled: true },
      { action: 'decide:reroute', label: 'Reroute to another clanker', enabled: true },
      { action: 'decide:delegate', label: 'Let hive decide', enabled: true },
      { action: 'hold', label: 'Hold', enabled: true },
    ] };
  const asked = Object.assign({}, waiting, { modifiers: ['needs-decision'], decision_needed: decision });
  let opts = stageTrackerDecisionOptions(asked, ctx);
  assert.deepStrictEqual(opts.map(o => o.id), ['decide:delegate', 'decide:adopt-now', 'decide:reroute', 'hold']);
  assert.ok(opts.every(o => o.enabled), JSON.stringify(opts));
  assert.strictEqual(opts[1].danger, true);
  // No clanker online: the popover's Reroute is greyed with the reason the server gave.
  const askedNobody = Object.assign({}, nobody, { modifiers: ['needs-decision'], decision_needed: Object.assign({}, decision, { options: decision.options.map(o => o.action === 'decide:reroute' ? Object.assign({}, o, { enabled: false, reason_if_disabled: 'no clankers online — use Adopt now' }) : o) }) });
  opts = stageTrackerDecisionOptions(askedNobody, ctx);
  assert.strictEqual(opts.find(o => o.id === 'decide:reroute').enabled, false);
  assert.strictEqual(opts.find(o => o.id === 'decide:reroute').why, 'no clankers online — use Adopt now');
  assert.strictEqual(opts.find(o => o.id === 'decide:adopt-now').enabled, true);
  // The issue row's popover resolves the nested PR's takeover options.
  const askedIssue = Object.assign({}, issue, { modifiers: ['needs-decision'], decision_needed: decision });
  _stageTracker = { items: [askedIssue] };
  opts = stageTrackerDecisionOptions(askedIssue, ctx);
  assert.strictEqual(opts.find(o => o.id === 'decide:adopt-now').enabled, true, JSON.stringify(opts));
  const pop = stageTrackerDecisionPopHtml(asked, stageTrackerDecisionOptions(asked, ctx), Date.now(), '');
  // One primary action (Let hive decide) and "More ⋯" for adopt / reroute via the row menu.
  assert.ok(pop.indexOf('data-ham-act="decide:delegate"') >= 0 && pop.indexOf('data-ham-more="1"') >= 0, pop);
  assert.ok(pop.indexOf('data-ham-act="decide:adopt-now"') < 0 && pop.indexOf('data-ham-act="decide:reroute"') < 0, pop);
  console.log('takeover confirms ok');
})().catch(e => { console.error(e); process.exit(1); });
`)
}
