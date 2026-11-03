package dashboard

import (
	"strings"
	"testing"
	"time"

	stageengineprstage "github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Let hive decide answers at once, a covered issue defers to its PR's row,
// and a person's move says why (hivecommons/hive#15773).

// The busy button keeps the tracker's working colour: the busy rule also
// carries aria-disabled (the click guard reads it), so it must be the
// compound selector and sit after the plain disabled rule, or the faint
// dashed disabled look wins on equal specificity.
func TestStageTrackerLetHiveDecideBusyCSS(t *testing.T) {
	html := indexHTML(t)
	busy := strings.Index(html, `.ham-lead-act[aria-busy="true"][aria-disabled="true"] {`)
	disabled := strings.Index(html, `.ham-lead-act[aria-disabled="true"] {`)
	if busy < 0 || disabled < 0 {
		t.Fatalf("index.html lacks the busy (%d) or disabled (%d) lead rule", busy, disabled)
	}
	if busy < disabled {
		t.Error("the busy lead rule must come after the disabled one so it wins the cascade")
	}
	rule := html[busy : busy+strings.Index(html[busy:], "}")]
	for _, want := range []string{"color: var(--cue-work)", "cursor: progress", "opacity: 1", "border-style: solid"} {
		if !strings.Contains(rule, want) {
			t.Errorf("busy lead rule lacks %q: %s", want, rule)
		}
	}
}

// The preview's pend is the one tagged to busy the button; the confirmed
// action's pend is not.
func TestStageTrackerLetHiveDecidePreviewTagsPend(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"const caption = stageTrackerActingCaption('decide:delegate:preview');",
		"caption, undefined, 'decide:delegate');",
		`'decide:delegate:preview': 'Asking the hive…'`,
		"picked = await stageTrackerDelegatePlanDialog(() => Promise.all(items.map(stageTrackerFetchDelegatePlan)), stageTrackerSentinelPreface(items), { subject: stageTrackerDelegatePlanSubject(items), caption, onSettled: () => stageTrackerClearPending(pend) });",
		"stageTrackerLeadBusySync(n.querySelector(':scope > .ham-row-head .ham-lead-act[data-ham-act]'), busy[key] ? busy[key].busy : '', busy[key] ? busy[key].caption : '');",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}

// stageTrackerPendingSync under node: only the tagged pend busies the lead,
// and only the lead it names; a disabled lead comes back disabled with its
// reason once the pend clears.
func TestStageTrackerLetHiveDecideBusyRestoreNode(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+`
const now = Date.now();
function hamStubLead(act, disabledWhy) {
  const head = hamStubEl('div'); head.classList.add('ham-row-head');
  const b = hamStubEl('button'); b.classList.add('ham-lead-act'); b.setAttribute('data-ham-act', act); b.textContent = 'Let hive decide';
  if (disabledWhy) { b.setAttribute('aria-disabled', 'true'); b.setAttribute('title', disabledWhy); } else b.setAttribute('title', 'delegate');
  head.appendChild(b); return { head, b };
}
const K = 'hivecommons/hive/pr#15773';
// 1. The preview's tagged pend: busy + spinner + the pend's caption ("Asking the hive…", the row caption's phrase), not clickable; restore puts the label and hint back.
let row = hamStubRow(K); let { head, b } = hamStubLead('decide:delegate'); row.appendChild(head); hamPanel.children.length = 0; hamPanel.appendChild(row);
let pend = stageTrackerSetPending([K], stageTrackerActingCaption('decide:delegate:preview'), now, 'decide:delegate');
assert.strictEqual(b.getAttribute('aria-busy'), 'true', 'tagged pend busies the button');
assert.strictEqual(b.getAttribute('aria-disabled'), 'true', 'busy is not clickable (the click guard reads aria-disabled)');
assert.strictEqual(b.getAttribute('title'), null, 'the hint does not show while busy');
assert.ok(b.children.some(c => c.classList && c.classList.contains('cue-acting-spin')), 'spinner');
assert.strictEqual(b.children[b.children.length - 1].nodeValue, ' Asking the hive…', 'the button reads the pend caption, one phrase with the row');
assert.strictEqual(stageTrackerActingCaption('decide:delegate:preview'), 'Asking the hive…'); assert.strictEqual(stageTrackerActingCaption('decide:delegate'), 'Hive deciding…', 'the confirmed action keeps its own caption');
assert.ok(row.classList.contains('cue-acting'), 'the row still wears the acting caption');
stageTrackerPendingSync(hamPanel);
assert.strictEqual(b.children.filter(c => c.classList && c.classList.contains('cue-acting-spin')).length, 1, 'a re-sync adds no second spinner');
stageTrackerClearPending(pend);
assert.strictEqual(b.getAttribute('aria-busy'), null); assert.strictEqual(b.getAttribute('aria-disabled'), null, 'an enabled lead is clickable again');
assert.strictEqual(b.getAttribute('title'), 'delegate', 'the hint is back'); assert.strictEqual(b.textContent, 'Let hive decide');
// 2. The confirmed action's pend (no tag, "Adopting…" etc.) never touches the button, even on a decide:delegate lead.
pend = stageTrackerSetPending([K], 'Adopting…', now);
assert.strictEqual(b.getAttribute('aria-busy'), null, 'an untagged pend leaves the lead alone: ' + JSON.stringify(b.attrs));
assert.ok(row.classList.contains('cue-acting'), 'but the row caption shows');
stageTrackerClearPending(pend);
// 3. A tagged pend on a row whose lead is another verb leaves that lead alone.
row = hamStubRow(K); ({ head, b } = hamStubLead('decide:merge-now')); row.appendChild(head); hamPanel.children.length = 0; hamPanel.appendChild(row);
pend = stageTrackerSetPending([K], stageTrackerActingCaption('decide:delegate:preview'), now, 'decide:delegate');
assert.strictEqual(b.getAttribute('aria-busy'), null, 'Merge now does not read Asking the hive…');
stageTrackerClearPending(pend);
// 4. A lead the row rendered disabled (aria-disabled + why) is disabled again after the pend clears — not clickable.
row = hamStubRow(K); ({ head, b } = hamStubLead('decide:delegate', 'Needs write access')); row.appendChild(head); hamPanel.children.length = 0; hamPanel.appendChild(row);
pend = stageTrackerSetPending([K], stageTrackerActingCaption('decide:delegate:preview'), now, 'decide:delegate');
assert.strictEqual(b.getAttribute('aria-busy'), 'true');
stageTrackerClearPending(pend);
assert.strictEqual(b.getAttribute('aria-busy'), null);
assert.strictEqual(b.getAttribute('aria-disabled'), 'true', 'a disabled lead stays disabled after the pend clears');
assert.strictEqual(b.getAttribute('title'), 'Needs write access', 'with its reason');
assert.strictEqual(b.textContent, 'Let hive decide');
`)
}

// A covered issue whose PR is listed as its own tracker row renders a
// passive chip, named after whatever the lead is, not a second button.
func TestStageTrackerCoveredIssueDropsPRChipNode(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+`
const NOW = Date.parse('2026-10-30T18:00:00Z');
const ctx = { user: true, canHold: true, canHuman: true, login: 'octo', nowMs: NOW, governor: {} };
// The decision buttons; the passive chip is a button too, but only to scroll.
const buttons = h => (h.match(/<button type="button" class="ham-lead-act/g) || []).length;
const asking = { number: 15647, url: 'https://github.com/o/r/pull/15647', title: 'Fix it', author: 'hive', author_kind: 'hive', stage: 85, modifiers: ['needs-decision'],
  decision_needed: { question: 'Approve or send back?', options: [{ action: 'decide:delegate', label: 'Let hive decide', enabled: true }, { action: 'decide:approve', label: 'Approve', enabled: true }] } };
const issue = { repo: 'o/r', kind: 'issue', number: 15320, title: 'Tracker lead chips', url: 'https://github.com/o/r/issues/15320', stage: 40, actor: 'maintainer', modifiers: [], linked_prs: [asking] };
const prRow = Object.assign({ repo: 'o/r', kind: 'pr' }, asking);
// Rows are what the render draws (ctx.rows, the page from
// tracker-filter-render.js); before any render nothing is listed, so no chip.
_stageTracker = { items: [issue, prRow] };
assert.ok(!stageTrackerLeadActionOnRow(issue, ctx).passive, 'no render yet: no chip');
// The helper is pure: it reads only the rows it is given.
assert.strictEqual(stageTrackerPRListedOwnRow(issue, { kind: 'pr', number: 15647 }), false);
assert.strictEqual(stageTrackerPRListedOwnRow(issue, { kind: 'pr', number: 15647 }, [issue, prRow]), true);
// The PR not listed as a row: the issue keeps its button (no sub-rows drawn).
ctx.rows = [issue];
let a = stageTrackerLeadActionOnRow(issue, ctx);
assert.strictEqual(a.id, 'decide:delegate'); assert.ok(!a.passive);
assert.strictEqual(buttons(stageTrackerLeadActionHtml(issue, ctx)), 1);
// Listed as its own row: passive, naming the PR; the chip carries no decision button.
ctx.rows = [issue, prRow];
a = stageTrackerLeadActionOnRow(issue, ctx);
assert.strictEqual(a.passive, 15647, JSON.stringify(a));
let h = stageTrackerLeadActionHtml(issue, ctx);
assert.strictEqual(buttons(h), 0, 'no second button: ' + h);
assert.ok(h.includes('decision pending on #15647'), h);
assert.ok(h.includes('Let hive decide lives on its row'), h);
// Clicking the chip goes to the PR's row (hivecommons/hive#15773).
assert.ok(h.startsWith('<button type="button" class="ham-badge ham-decision-pending" data-ham-scroll-to="o/r/pr#15647" title="'), h);
assert.ok(!h.includes('data-ham-act') && !h.includes('data-ham-scroll-row'), 'the chip runs no action: ' + h);
// The PR's own row still carries the button.
assert.strictEqual(buttons(stageTrackerLeadActionHtml(prRow, ctx)), 1);
// Nested under the issue (stage_tracker_links.go sets nested_under): the PR
// is in items but stageTrackerVisible folds it into the issue's sub-row and
// leaves it out of rows, so no chip — the issue keeps its button when no
// sub-row is drawn (one covering PR) and yields it to the sub-row when several are.
const nested = Object.assign({}, prRow, { nested_under: 15320 });
_stageTracker = { items: [issue, nested] }; ctx.rows = [issue];
a = stageTrackerLeadActionOnRow(issue, ctx);
assert.ok(a && !a.passive, 'a nested PR is not its own row: ' + JSON.stringify(a));
assert.strictEqual(buttons(stageTrackerLeadActionHtml(issue, ctx)), 1, 'the issue row keeps its only decision button');
assert.strictEqual(stageTrackerLeadActionOnRow(issue, ctx, [asking]), null, 'drawn as a sub-row, the sub-row carries it');
// A render records the drawn rows; the chip follows them, not the items:
// the PR hidden by the filter or on another page leaves the button on the
// issue, the PR drawn as a row turns it into the chip.
_stageTracker = { items: [issue, prRow] }; ctx.rows = [issue];
assert.ok(!stageTrackerLeadActionOnRow(issue, ctx).passive, 'the PR not drawn: no chip');
_stageTracker = { items: [issue, nested] }; ctx.rows = [issue, nested];
assert.strictEqual(stageTrackerLeadActionOnRow(issue, ctx).passive, 15647, 'the PR drawn (Pull requests view aside, a row is a row): chip');
// A server-marked lead that is not Let hive decide is named as itself.
const merging = Object.assign({}, asking, { decision_needed: { question: 'Merge?', options: [{ action: 'decide:merge-now', label: 'Merge now', enabled: true, lead: true }, { action: 'decide:delegate', label: 'Let hive decide', enabled: true }] } });
const issue2 = Object.assign({}, issue, { number: 15321, linked_prs: [merging] });
const mergingRow = Object.assign({ repo: 'o/r', kind: 'pr' }, merging);
_stageTracker = { items: [issue2, mergingRow] }; ctx.rows = [issue2, mergingRow];
a = stageTrackerLeadActionOnRow(issue2, ctx);
assert.strictEqual(a.id, 'decide:merge-now', JSON.stringify(a)); assert.strictEqual(a.passive, 15647);
h = stageTrackerLeadActionHtml(issue2, ctx);
assert.ok(h.includes('Merge now lives on its row') && !h.includes('Let hive decide lives'), 'the chip names the lead it stands in for: ' + h);
// hivecommons/hive#15503 under #15502, both covered by PR #15759: the
// server nests the PR under the lowest issue (drawn_under: 15502 on both
// issues' linked_prs), so the PR is never its own row and #15503 draws no
// sub-row — the button is #15502's, #15503 chips to that row.
const shared = Object.assign({}, asking, { number: 15759, url: 'https://github.com/o/r/pull/15759', drawn_under: 15502 });
const primary = Object.assign({}, issue, { number: 15502, linked_prs: [shared] });
const secondary = Object.assign({}, issue, { number: 15503, linked_prs: [shared] });
const sharedRow = Object.assign({ repo: 'o/r', kind: 'pr', nested_under: 15502 }, shared);
_stageTracker = { items: [primary, secondary, sharedRow] }; ctx.rows = [primary, secondary];
assert.strictEqual(stageTrackerPRDrawnUnderOther(secondary, { kind: 'pr', number: 15759 }, ctx.rows), 15502);
assert.strictEqual(stageTrackerPRDrawnUnderOther(primary, { kind: 'pr', number: 15759 }, ctx.rows), 0, 'the drawing issue defers to nobody');
assert.strictEqual(stageTrackerPRDrawnUnderOther(secondary, { kind: 'pr', number: 15759 }, [secondary]), 0, '#15502 not drawn: #15503 keeps the button');
a = stageTrackerLeadActionOnRow(primary, ctx);
assert.strictEqual(a.id, 'decide:delegate'); assert.ok(!a.passive, '#15502 draws the PR and carries the one button: ' + JSON.stringify(a));
assert.strictEqual(buttons(stageTrackerLeadActionHtml(primary, ctx)), 1);
a = stageTrackerLeadActionOnRow(secondary, ctx);
assert.strictEqual(a.passive, 15759, JSON.stringify(a)); assert.strictEqual(a.passiveUnder, 15502);
h = stageTrackerLeadActionHtml(secondary, ctx);
assert.strictEqual(buttons(h), 0, 'no second Let hive decide on #15503: ' + h);
assert.ok(h.includes('decision pending on #15759') && h.includes('Let hive decide lives on the row of #15502, which draws the PR'), h);
assert.ok(h.includes('data-ham-scroll-to="o/r/pr#15759" data-ham-scroll-row="o/r/issue#15502"'), 'the chip goes to #15502, which draws the PR: ' + h);
// #15502 filtered off the page: #15503 is the only row that can carry it.
ctx.rows = [secondary];
a = stageTrackerLeadActionOnRow(secondary, ctx);
assert.ok(a && !a.passive, JSON.stringify(a));
assert.strictEqual(buttons(stageTrackerLeadActionHtml(secondary, ctx)), 1);
_stageTracker = null;
`)
}

func TestStageTrackerHumanChevronSaysWhyJS(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, "function stageTrackerHumanWhy(item, kind)") || !strings.Contains(html, "sa.why || humanWhy(item, sa.kind)") {
		t.Error("the human next-move tooltip must fall back to the row's reason")
	}
	// The render hands the drawn page to the lead picker on ctx; the
	// payload object carries nothing the client derived.
	for _, want := range []string{
		"_stageTrackerRows = page.rows.map(e => grouping ? e.item : e).filter(Boolean);",
		"ctx.rows = typeof _stageTrackerRows !== 'undefined' ? _stageTrackerRows : null;",
		"stageTrackerPRListedOwnRow(item, a.target, rows)",
		"stageTrackerPRDrawnUnderOther(item, a.target, rows)",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	if strings.Contains(html, "_stageTracker.rows") {
		t.Error("the drawn rows must not be stashed on the server payload")
	}
}

// The row hover says the reason once: the 035 caption ("A human decision
// is owed — workflow path") and the next-move why (the same clause, lower
// case) both land in stageTrackerTooltip, so the next-move line keeps only
// who moves when its why is the caption again. The chevron hover (no
// caption line) still carries the full why.
func TestStageTrackerTooltipSaysDecisionReasonOnceJS(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowsPreludeJS(t, html)+"\n"+stageTrackerActorIconJS(t, html)+stageTrackerRowsFixtureJS+`
const since = new Date(now - 3600000).toISOString();
const pr = { repo: 'o/r', kind: 'pr', number: 15759, title: 'Fix it', url: 'https://x/15759', author: 'hive', author_kind: 'hive', stage: 35, stage_since: since, reason: 'contributor-lane:workflow-path', modifiers: ['needs-decision'],
  caption: 'A human decision is owed — workflow path', stage_actor: { kind: 'human', login: '', since, why: 'a human decision is owed — workflow path' } };
let tip = stageTrackerTooltip(pr, now).split('\n');
assert.strictEqual(tip.filter(l => /human decision is owed/i.test(l)).length, 1, 'the reason reads once: ' + tip.join(' | '));
assert.ok(tip.includes('A human decision is owed — workflow path'), tip.join(' | '));
assert.ok(tip.includes('Next move: human'), 'the next-move line keeps who moves: ' + tip.join(' | '));
// The chevron hover has no caption line, so the why stays whole there.
assert.ok(stageTrackerActorTip(pr).includes('Next move: human — a human decision is owed — workflow path'), stageTrackerActorTip(pr));
// A why that is not the caption is kept (090: the caption says the reason, the why says who moves).
const merge = Object.assign({}, pr, { stage: 90, caption: 'Ready to merge — a maintainer merges it', stage_actor: { kind: 'human', login: 'octo', since, why: 'a maintainer merges it' } });
tip = stageTrackerTooltip(merge, now).split('\n');
assert.ok(tip.includes('Next move: human @octo — a maintainer merges it'), tip.join(' | '));
`)
}

// The server's stage_actor.why on a 085 row says why the person is wanted
// — the tooltip shows sa.why whenever it is set, so a bare "a human
// decision is owed" would leave the 👤 marker unexplained on every row.
func TestStageTrackerDecisionActorWhySaysWhy(t *testing.T) {
	now := time.Date(2026, 10, 30, 18, 0, 0, 0, time.UTC)
	actor := func(it StageTrackerItem) *StageTrackerActor {
		it.Kind = "pr"
		return stageTrackerStageActor(it, nil, now)
	}
	a := actor(StageTrackerItem{Stage: stagelabels.NeedsDecision.Number(), Reason: "contributor-lane:workflow-path"})
	if a.Kind != StageActorHuman || a.Why != "a human decision is owed — workflow path" {
		t.Errorf("reason-backed why = %q (kind %q)", a.Why, a.Kind)
	}
	a = actor(StageTrackerItem{Stage: stagelabels.IssueNeedsDecision.Number(), DecisionNeeded: &StageTrackerDecision{Question: "Keep #1 and close #2, or keep #2 and close #1?"}})
	if a.Kind != StageActorHuman || a.Why != "a human decision is owed — Keep #1 and close #2, or keep #2 and close #1?" {
		t.Errorf("question-backed why = %q (kind %q)", a.Why, a.Kind)
	}
	a = actor(StageTrackerItem{Stage: stagelabels.NeedsDecision.Number()})
	if a.Kind != StageActorHuman || a.Why != "a human decision is owed" {
		t.Errorf("bare why = %q (kind %q)", a.Why, a.Kind)
	}
	// The unauthorized step keeps its own, more specific, why.
	a = actor(StageTrackerItem{Stage: stagelabels.NeedsDecision.Number(), Reason: stageengineprstage.StageReasonUnauthorizedPrefix + stageengineprstage.UnauthorizedStepApproveWorkflowRuns})
	if !strings.Contains(a.Why, "not permitted to") {
		t.Errorf("unauthorized why = %q", a.Why)
	}
}
