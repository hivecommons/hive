package dashboard

import (
	"strings"
	"testing"
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
		"stageTrackerActingCaption('decide:delegate'), undefined, 'decide:delegate');",
		"try { plans = await Promise.all(items.map(stageTrackerFetchDelegatePlan)); } finally { stageTrackerClearPending(pend); }",
		"stageTrackerLeadBusySync(n.querySelector(':scope > .ham-row-head .ham-lead-act[data-ham-act]'), busy[key] || '');",
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
// 1. The preview's tagged pend: busy + spinner + "Asking the hive…", not clickable; restore puts the label and hint back.
let row = hamStubRow(K); let { head, b } = hamStubLead('decide:delegate'); row.appendChild(head); hamPanel.children.length = 0; hamPanel.appendChild(row);
let pend = stageTrackerSetPending([K], stageTrackerActingCaption('decide:delegate'), now, 'decide:delegate');
assert.strictEqual(b.getAttribute('aria-busy'), 'true', 'tagged pend busies the button');
assert.strictEqual(b.getAttribute('aria-disabled'), 'true', 'busy is not clickable (the click guard reads aria-disabled)');
assert.strictEqual(b.getAttribute('title'), null, 'the hint does not show while busy');
assert.ok(b.children.some(c => c.classList && c.classList.contains('cue-acting-spin')), 'spinner');
assert.strictEqual(b.children[b.children.length - 1].nodeValue, STAGE_TRACKER_LEAD_BUSY_TEXT);
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
pend = stageTrackerSetPending([K], stageTrackerActingCaption('decide:delegate'), now, 'decide:delegate');
assert.strictEqual(b.getAttribute('aria-busy'), null, 'Merge now does not read Asking the hive…');
stageTrackerClearPending(pend);
// 4. A lead the row rendered disabled (aria-disabled + why) is disabled again after the pend clears — not clickable.
row = hamStubRow(K); ({ head, b } = hamStubLead('decide:delegate', 'Needs write access')); row.appendChild(head); hamPanel.children.length = 0; hamPanel.appendChild(row);
pend = stageTrackerSetPending([K], stageTrackerActingCaption('decide:delegate'), now, 'decide:delegate');
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
const buttons = h => (h.match(/<button\b/g) || []).length;
const asking = { number: 15647, url: 'https://github.com/o/r/pull/15647', title: 'Fix it', author: 'hive', author_kind: 'hive', stage: 85, modifiers: ['needs-decision'],
  decision_needed: { question: 'Approve or send back?', options: [{ action: 'decide:delegate', label: 'Let hive decide', enabled: true }, { action: 'decide:approve', label: 'Approve', enabled: true }] } };
const issue = { repo: 'o/r', kind: 'issue', number: 15320, title: 'Tracker lead chips', url: 'https://github.com/o/r/issues/15320', stage: 40, actor: 'maintainer', modifiers: [], linked_prs: [asking] };
const prRow = Object.assign({ repo: 'o/r', kind: 'pr' }, asking);
// The PR not listed as a row: the issue keeps its button (no sub-rows drawn).
_stageTracker = { items: [issue] };
let a = stageTrackerLeadActionOnRow(issue, ctx);
assert.strictEqual(a.id, 'decide:delegate'); assert.ok(!a.passive);
assert.strictEqual(buttons(stageTrackerLeadActionHtml(issue, ctx)), 1);
// Listed as its own row: passive, naming the PR; the chip is a span, no button.
_stageTracker = { items: [issue, prRow] };
a = stageTrackerLeadActionOnRow(issue, ctx);
assert.strictEqual(a.passive, 15647, JSON.stringify(a));
let h = stageTrackerLeadActionHtml(issue, ctx);
assert.strictEqual(buttons(h), 0, 'no second button: ' + h);
assert.ok(h.includes('decision pending on #15647'), h);
assert.ok(h.includes('Let hive decide lives on its row'), h);
// The PR's own row still carries the button.
assert.strictEqual(buttons(stageTrackerLeadActionHtml(prRow, ctx)), 1);
// A server-marked lead that is not Let hive decide is named as itself.
const merging = Object.assign({}, asking, { decision_needed: { question: 'Merge?', options: [{ action: 'decide:merge-now', label: 'Merge now', enabled: true, lead: true }, { action: 'decide:delegate', label: 'Let hive decide', enabled: true }] } });
const issue2 = Object.assign({}, issue, { number: 15321, linked_prs: [merging] });
_stageTracker = { items: [issue2, Object.assign({ repo: 'o/r', kind: 'pr' }, merging)] };
a = stageTrackerLeadActionOnRow(issue2, ctx);
assert.strictEqual(a.id, 'decide:merge-now', JSON.stringify(a)); assert.strictEqual(a.passive, 15647);
h = stageTrackerLeadActionHtml(issue2, ctx);
assert.ok(h.includes('Merge now lives on its row') && !h.includes('Let hive decide lives'), 'the chip names the lead it stands in for: ' + h);
_stageTracker = null;
`)
}

func TestStageTrackerHumanChevronSaysWhyJS(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, "function stageTrackerHumanWhy(item, kind)") || !strings.Contains(html, "sa.why || humanWhy(item, sa.kind)") {
		t.Error("the human next-move tooltip must fall back to the row's reason")
	}
}
