package dashboard

import (
	"strings"
	"testing"
)

// The "Needs human" decision popover (owner request, 2026-10-28): hovering
// or focusing the "Needs human decision" chevron word / needs-human pill
// shows WHAT to decide, who asked and when, the blocker in plain words,
// and the applicable actions — "Let hive decide" first — down the row
// context menu's code path. The pure helpers run under node the way
// stage_tracker_row_actions_test.go does.

func stageTrackerDecisionPopPreamble(t *testing.T, html string) string {
	t.Helper()
	return strings.Join([]string{
		stageTrackerRowActionsPreamble(t, html),
		jsConstLine(t, html, "STAGE_TRACKER_DECISION_LEAVE_MS"),
		"var _stageTrackerDecisionPop = null;",
		jsFunc(t, html, "agentNowAgo"),
		jsFunc(t, html, "stageTrackerDecisionTriggerHtml"),
		jsFunc(t, html, "stageTrackerDecisionOptions"),
		jsFunc(t, html, "stageTrackerSayOnce"),
		jsFunc(t, html, "stageTrackerDecisionPopHtml"),
		jsFunc(t, html, "stageTrackerDecisionRun"),
	}, "\n")
}

const stageTrackerDecisionPopFixture = `
const decision = { question: 'Should the retry budget go to 5?', asked_by: 'a reviewer', since: '2026-10-27T10:30:00Z',
  blocker: 'A reviewer requested changes on b0f5f80 — needs a new commit. Approving cannot override that.', blocker_reason: 'contributor-lane:changes-requested',
  options: [
    { action: 'decide:approve', label: 'Approve', enabled: false, reason_if_disabled: 'A reviewer requested changes on b0f5f80 — needs a new commit. Approving cannot override that.' },
    { action: 'decide:delegate', label: 'Let hive decide', enabled: true },
    { action: 'decide:direction', label: 'Give direction', enabled: true },
    { action: 'decide:close-superseded', label: 'Close as superseded', enabled: true },
    { action: 'decide:close-wontfix', label: "Close as won't fix", enabled: true },
    { action: 'hold', label: 'Hold', enabled: true },
  ] };
const pr = { repo: 'o/r', kind: 'pr', number: 7, title: 'Raise retries', url: 'https://github.com/o/r/pull/7', stage: 85, stage_since: '2026-10-27T09:00:00Z', modifiers: ['needs-decision'], decision_needed: decision };
const issue = { repo: 'o/r', kind: 'issue', number: 3, title: 'Flaky retries', stage: 40, mirrored: true, modifiers: [], linked_prs: [ { number: 7, title: 'Raise retries', url: 'https://github.com/o/r/pull/7', stage: 85, modifiers: ['needs-decision'], decision_needed: decision } ] };
const plain = { repo: 'o/r', kind: 'pr', number: 8, title: 'Plain', stage: 60, modifiers: [] };
const ctx = { user: true, canHold: true, canHuman: true };
`

func TestStageTrackerDecisionPopoverContent(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerDecisionPopPreamble(t, html)+stageTrackerDecisionPopFixture+`
// The row's decision: the PR's own; an issue's is its parked nested PR's; a plain row has none.
assert.strictEqual(stageTrackerDecisionOf(pr).key, 'o/r/pr#7');
const viaIssue = stageTrackerDecisionOf(issue);
assert.strictEqual(viaIssue.key, 'o/r/pr#7');
assert.strictEqual(viaIssue.item.kind, 'pr');
assert.strictEqual(viaIssue.item.decision_needed.question, decision.question);
assert.strictEqual(stageTrackerDecisionOf(plain), null);
assert.strictEqual(stageTrackerDecisionOf(null), null);
// An issue row with one covering PR renders through an overlay (stageTrackerRowHtml's
// "shown"): the issue's number, the PR's decision_needed. Its trigger must key on the
// PR — the issue key looks up the real issue, which has no decision, and the popover
// never opened on stage-085 rows (hivecommons/hive#15096 N1).
const shown = Object.assign({}, issue, { via_pr: true, stage: 85, modifiers: ['needs-decision'], decision_needed: decision });
const viaShown = stageTrackerDecisionOf(shown);
assert.strictEqual(viaShown.key, 'o/r/pr#7');
assert.strictEqual(viaShown.item.kind, 'pr');
assert.strictEqual(viaShown.item.number, 7);
assert.strictEqual(viaShown.item.decision_needed.question, decision.question);
// An issue asking its own question (045 triage, hive hold) still keys on itself.
const own = Object.assign({}, issue, { stage: 45, linked_prs: [], decision_needed: decision });
assert.strictEqual(stageTrackerDecisionOf(own).key, 'o/r/issue#3');

// Options: server narrowed by the viewer's role; "Let hive decide" leads; disabled carry their reason.
let opts = stageTrackerDecisionOptions(pr, ctx);
assert.deepStrictEqual(opts.map(o => o.id), ['decide:delegate', 'decide:approve', 'decide:direction', 'decide:close-superseded', 'decide:close-wontfix', 'hold']);
assert.strictEqual(opts[0].enabled, true);
assert.strictEqual(opts[1].enabled, false);
assert.ok(opts[1].why.indexOf('requested changes') >= 0, opts[1].why);
assert.strictEqual(opts.find(o => o.id === 'decide:close-wontfix').danger, true);
// A read-only viewer sees every option greyed with the sign-in reason.
opts = stageTrackerDecisionOptions(pr, { user: false, canHold: false, canHuman: false });
assert.ok(opts.every(o => !o.enabled), JSON.stringify(opts));
assert.strictEqual(opts[0].why, STAGE_TRACKER_WHY_WRITE);

// Trigger: a button, no native title (the popover is the hover text), aria for the dialog.
const trig = stageTrackerDecisionTriggerHtml(stageTrackerDecisionOf(pr), '<span class="ham-detour">Needs human decision</span>', 'Merge gate — tip');
assert.ok(/^<button type="button"/.test(trig), trig);
assert.ok(trig.indexOf('class=" ham-decision-trigger"') >= 0, trig);
assert.ok(trig.indexOf('data-ham-decision="o/r/pr#7"') >= 0, trig);
assert.ok(trig.indexOf('aria-haspopup="dialog"') >= 0 && trig.indexOf('aria-expanded="false"') >= 0, trig);
assert.ok(trig.indexOf(' title=') < 0, trig);
assert.ok(trig.indexOf('data-ham-tip="Merge gate — tip"') >= 0, trig);
assert.ok(trig.indexOf('Needs human decision') >= 0, trig);

// Popover (hivecommons/hive#15394): dialog role, question → one primary action
// + "More ⋯" → who/when → a collapsed Details holding the blocker and the stage line.
const nowMs = Date.parse('2026-10-27T12:30:00Z');
const pop = stageTrackerDecisionPopHtml(pr, stageTrackerDecisionOptions(pr, ctx), nowMs, 'Merge gate — tip');
assert.ok(pop.indexOf('role="dialog"') >= 0 && pop.indexOf('aria-label="Decision needed on o/r#7"') >= 0, pop);
assert.ok(pop.indexOf('o/r#7 · Raise retries') >= 0, pop);
const at = s => { const i = pop.indexOf(s); assert.ok(i >= 0, 'missing ' + s + ' in ' + pop); return i; };
const iQ = at('Should the retry budget go to 5?'), iA = at('ham-decision-acts'), iM = at('Asked by a reviewer · 2 hours ago'), iD = at('<details class="ham-decision-details"><summary>Details</summary>'), iB = at('needs a new commit'), iS = at('Merge gate — tip');
assert.ok(iQ < iA && iA < iM && iM < iD && iD < iB && iB < iS, [iQ, iA, iM, iD, iB, iS].join(','));
// Buttons: exactly one action — the first the viewer may take, marked lead — plus "More ⋯" for the row menu.
const buttons = pop.match(/<button[^>]*>[^<]*<\/button>/g);
assert.strictEqual(buttons.length, 2, buttons.join('\n'));
assert.ok(/data-ham-act="decide:delegate"/.test(buttons[0]) && buttons[0].indexOf('ham-decision-lead') >= 0, buttons[0]);
assert.ok(buttons[0].indexOf('>Let hive decide<') >= 0, buttons[0]);
assert.ok(/data-ham-more="1"/.test(buttons[1]) && buttons[1].indexOf('ham-decision-more') >= 0 && buttons[1].indexOf('>More ⋯<') >= 0, buttons[1]);
assert.ok(pop.indexOf('ham-decision-consequences') < 0 && pop.indexOf('/lgtm') < 0, pop);
// Nothing the viewer may take: the lead option shows greyed with its reason.
const held = Object.assign({}, pr, { modifiers: ['needs-decision', 'hold'], decision_needed: Object.assign({}, decision, { options: decision.options.slice(0, 1) }) });
const heldPop = stageTrackerDecisionPopHtml(held, stageTrackerDecisionOptions(held, ctx), nowMs, '');
assert.ok(/aria-disabled="true"[^>]*title="A reviewer requested changes/.test(heldPop), heldPop);
assert.ok(heldPop.indexOf('data-ham-more') < 0, heldPop);
// Say it once: a sentence the question already said does not come back in Details.
const echo = Object.assign({}, pr, { decision_needed: Object.assign({}, decision, { question: 'Ready to merge. Hive won\'t merge it alone — @x isn\'t a trusted author yet.', blocker: '', detail: 'To trust @x: give them the trusted tier under 👥 Contributors.' }) });
const echoPop = stageTrackerDecisionPopHtml(echo, stageTrackerDecisionOptions(echo, ctx), nowMs, 'Ready for human merge — Hive won\'t merge it alone — @x isn\'t a trusted author yet · Next move: human — a maintainer merges it');
assert.strictEqual((echoPop.match(/isn&#39;t a trusted author yet|isn't a trusted author yet/g) || []).length, 1, echoPop);
assert.ok(echoPop.indexOf('To trust @x') >= 0 && echoPop.indexOf('a maintainer merges it') >= 0, echoPop);
// The visible first read (everything outside Details) is at most 40 words.
const visible = echoPop.replace(/<details[\s\S]*<\/details>/, '').replace(/<[^>]+>/g, ' ').replace(/&[a-z#0-9]+;/g, "'").trim().split(/\s+/).filter(Boolean);
assert.ok(visible.length <= 40, visible.length + ' words: ' + visible.join(' '));
// Plain words only: no reason slugs, stage codes, verdict enums, internal nouns or config keys reach the operator.
for (const bad of ['contributor-lane', 'requires_human', ' 085', 'sentinel', 'sweep', 'soak', 'audit marker', 'trust override']) assert.ok(pop.indexOf(bad) < 0 && echoPop.indexOf(bad) < 0, bad + ' in ' + pop);
assert.ok(!/\b[a-z_]+\.[a-z_]+\.[a-z_]+\b/.test(echoPop.replace(/https?:\/\/\S+/g, '')), echoPop);
// Escaped: a question with markup is text, not HTML.
const hostile = Object.assign({}, pr, { decision_needed: Object.assign({}, decision, { question: '<img src=x onerror=alert(1)>' }) });
assert.ok(stageTrackerDecisionPopHtml(hostile, [], nowMs, '').indexOf('<img') < 0);
`)
}

func TestStageTrackerDecisionPopoverRunsRowAction(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerDecisionPopPreamble(t, html)+stageTrackerDecisionPopFixture+`
// With the shared helper present (c4f4bb28's runRowAction), the popover calls it with (action, item).
const calls = [];
globalThis.runRowAction = (act, item) => { calls.push([act, item]); return 'ran'; };
assert.strictEqual(stageTrackerDecisionRun(pr, 'decide:delegate'), 'ran');
assert.deepStrictEqual(calls, [['decide:delegate', pr]]);
// Without it, the context menu's own path (stageTrackerDoAction) runs on the row.
globalThis.runRowAction = undefined; // the fixture declares the real one; a declared function cannot be deleted
const done = [];
globalThis.stageTrackerDoAction = (items, id) => { done.push([items, id]); return 'menu'; };
assert.strictEqual(stageTrackerDecisionRun(pr, 'decide:approve'), 'menu');
assert.deepStrictEqual(done, [[[pr], 'decide:approve']]);
`)
}

// The strip and chip renderers hand the popover trigger to the "Needs
// human decision" chevron word and the lead needs-human pill — and keep a
// plain row's native title.
func TestStageTrackerDecisionPopoverTriggersInRow(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`typeof stageTrackerDecisionOf === 'function' ? stageTrackerDecisionOf(item) : null`,
		`stageTrackerDecisionTriggerHtml(decision, word, tip)`,
		`stageTrackerDecisionTriggerHtml(decision, inner, tip, 'ham-badge ham-badge-mod ' + cls)`,
		`${decision ? '' : ` + "` title=\"${esc(tip)}\"`" + `}`,
		`if (typeof stageTrackerWireDecisionPop === 'function') stageTrackerWireDecisionPop(el);`,
		`decision_needed: lp.decision_needed || null`,
		`role="dialog"`,
		`.ham-decision-pop { position: fixed;`,
		`--cue-state: var(--status-attention);`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	// Every lp→PR item goes through the one builder (stageTrackerLinkedPRItem),
	// which carries decision_needed; the only other lp spread is the single
	// covering PR's overlay on its issue row (`shown`). Callers must not
	// rebuild a PR item by hand.
	if n := strings.Count(html, "decision_needed: lp.decision_needed || null"); n != 2 {
		t.Errorf("lp→PR constructions: stageTrackerLinkedPRItem + the one-PR row overlay only (found %d)", n)
	}
	for _, want := range []string{"stageTrackerItemByKey", "stageTrackerDecisionTarget", "stageTrackerDecisionOf", "stageTrackerSubRowHtml"} {
		fn := jsFunc(t, html, want)
		if !strings.Contains(fn, "stageTrackerLinkedPRItem(") && !strings.Contains(fn, "stageTrackerParkedNestedPRs(") {
			t.Errorf("%s must build nested PR items with stageTrackerLinkedPRItem", want)
		}
	}
}
