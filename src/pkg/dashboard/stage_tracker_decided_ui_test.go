package dashboard

import (
	"strings"
	"testing"
)

// hivecommons/hive#15126 — the post-decision state machine as the Activity
// Monitor draws it, executed under node from the lifted SPA helpers:
//
//	deciding   spinner caption inside the lit pill, no ✓ badge
//	decided    ✓ badge whose tooltip leads with the decision, pill = the
//	           decision's effect + countdown, 090 striped pill gone
//	landed     adoption pair: dimmed original, chip on the copy
//	closed     a closed-superseded original is one dimmed line, never a
//	           blank numbered row with hollow chevrons
//
// plus: "Approving" never renders for a non-approve decision.
func stageTrackerDecidedUIJS(t *testing.T, html string) string {
	t.Helper()
	return strings.Join([]string{
		stageTrackerRowsPreludeJS(t, html),
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_GATE_WORD"),
		jsConstBlock(t, html, "STAGE_TRACKER_GATE_REASON_WORDS"),
		jsFunc(t, html, "stageTrackerAgoText"),
		jsFunc(t, html, "stageTrackerMergeGateStateOf"),
		jsFunc(t, html, "stageTrackerShortLeft"),
		jsFunc(t, html, "stageTrackerGateReasonText"),
		stageTrackerMergeGateInfoJS(t, html),
		jsFunc(t, html, "stageTrackerMergeGateWordHtml"),
		jsFunc(t, html, "stageTrackerGateCueHtml"),
		jsFunc(t, html, "stageTrackerDecidedOf"),
		jsConstBlock(t, html, "STAGE_TRACKER_DECIDED_ACTING"),
		jsFunc(t, html, "stageTrackerDecidedActing"),
		jsFunc(t, html, "stageTrackerDecidedLead"),
		jsFunc(t, html, "stageTrackerLinkedSupersededText"),
		jsFunc(t, html, "stageTrackerLinkedSupersededTip"),
		jsFunc(t, html, "stageTrackerSupersededOriginalClause"),
		jsFunc(t, html, "stageTrackerAdoptedFromChipHtml"),
		// Pending entries as stageTrackerRowDeciding reads them.
		"var _stageTrackerPending = [];",
		jsFunc(t, html, "stageTrackerRowDeciding"),
		jsFunc(t, html, "stageTrackerSetPending"),
		jsFunc(t, html, "stageTrackerPendingSync"),
		stageTrackerDomStubJS,
		stageTrackerRowsFixtureJS,
	}, "\n")
}

func TestStageTrackerDecidedStatesNode(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		// The in-flight caption is hosted by the lit chevron, never a
		// sibling of the ✓ badge.
		`strip.querySelector(':scope > .ham-strip > .ham-seg.active:not(.cap)') || strip`,
		`'decide:delegate': 'Hive deciding…'`,
		// Preview before acting: the plan route and the three buttons.
		`fetch(req.url + '/plan'`,
		`const STAGE_TRACKER_PLAN_BUTTON_DELEGATE = 'Let hive do that';`,
		`const STAGE_TRACKER_PLAN_BUTTON_MERGE = 'Approve and let hive merge';`,
		`class="hv-btn btn-secondary hive-dialog-btn cancel">Cancel</button>`,
		`if (payload.chosen) body.chosen = payload.chosen;`,
		`.ham-sub-superseded { opacity: .55; }`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html lacks %q", want)
		}
	}
	runStageTrackerRowActionsScript(t, stageTrackerDecidedUIJS(t, html)+`
window._hiveUser = 'clubanderson';
const at = new Date(now - 3 * 60000).toISOString();
const nextAt = new Date(now + 7 * 60000).toISOString();
const acted = { action: 'let-hive-decide', actor: 'clubanderson', at, state: 'waiting', verdict: 'adopt-supersede', rule: 'contributor-offline', outcome: 'applied', note: 'Let hive do that · previewed adopt-supersede' };
const decidedPending = { action: 'let-hive-decide', verdict: 'adopt-supersede', rule: 'contributor-offline', actor: 'clubanderson', at, effect: 'pending', stage_before: 90,
  word: 'Adopted', text: 'Adopted · hive rebases & merges next pass', acting: 'Adopting · opening hive copy…',
  title: 'Hive decided 11:57 UTC — adopted (supersede) (the author\'s clanker hasn\'t been connected since the nudge) · you chose Let hive decide; hive chose adopt-supersede; next: adoption pass in 7m', next_at: nextAt, effect_timeout_s: 900 };
const original = (over) => Object.assign({ number: 15029, title: 'fix: tracker pill', url: 'u29', stage: 90, author: 'Karibhou', author_kind: 'clanker', modifiers: ['untrusted-author'], human_merge_reason_text: 'untrusted author', manual_merge: true }, over || {});
const issueOf = (prs, over) => Object.assign({ repo: 'hivecommons/hive', number: 15023, kind: 'issue', title: 'Pill contradicts itself', url: 'u23', stage: prs[0].stage, linked_prs: prs }, over || {});
const subHtml = (row, n) => subs(row).find(s => s.includes('pr#' + n + '"')) || '';
const pill = (html) => text((/<li class="ham-seg active[^"]*"[\s\S]*?<\/li>/.exec(html) || ['', ''])[0]).replace(/&amp;/g, '&');

// ── 1. deciding: the 090 row with the job in flight ─────────────────────
// Before the verdict there is no decided projection: the row keeps its
// stage, the badge is the dotted pending history mark (never a decided
// one), and the pending caption is "Hive deciding…" hosted inside the
// pill (stageTrackerPendingSync picks the lit chevron).
{
  const pr = original({ last_operator_action: Object.assign({}, acted, { state: 'pending' }) });
  const row = stageTrackerRowHtml(issueOf([pr]), stages, ctx);
  assert.ok(row.includes('ham-acted-pending'), 'pending history badge: ' + row.match(/ham-acted[^"]*/g));
  assert.ok(!row.includes('ham-acted-decided'), 'no decided badge while the hive is still deciding');
  const p = stageTrackerSetPending(['hivecommons/hive/pr#15029'], 'Hive deciding…', now);
  assert.ok(stageTrackerRowDeciding('hivecommons/hive/pr#15029'), 'a pending entry without a verdict is "deciding"');
  // The badge stays hidden while deciding even if the server already
  // lists a waiting action for the row.
  const row2 = stageTrackerRowHtml(issueOf([original({ last_operator_action: acted })]), stages, ctx);
  assert.ok(!row2.includes('ham-acted'), 'the ✓ waits for the verdict');
  _stageTrackerPending.length = 0;
  // The caption lands inside the lit chevron, not beside the badge.
  const r = hamStubEl('div'); r.setAttribute('data-ham-sub-key', 'hivecommons/hive/pr#15029');
  const stripRow = hamStubEl('div'); stripRow.classList.add('ham-strip-row'); r.appendChild(stripRow);
  const ol = hamStubEl('ol'); ol.classList.add('ham-strip'); stripRow.appendChild(ol);
  const seg = hamStubEl('li'); seg.classList.add('ham-seg'); seg.classList.add('active'); ol.appendChild(seg);
  stripRow.querySelector = (sel) => sel === ':scope > .ham-strip > .ham-seg.active:not(.cap)' ? seg : (sel === ':scope > .cue-acting-cap' ? stripRow.children.find(c => c.classList.contains('cue-acting-cap')) || null : null);
  hamPanel.children.length = 0; hamPanel.appendChild(r);
  stageTrackerSetPending(['hivecommons/hive/pr#15029'], 'Hive deciding…', now); stageTrackerPendingSync(hamPanel);
  assert.strictEqual(seg.children.length, 1, 'the caption is a child of the lit chevron');
  assert.strictEqual(seg.children[0].className, 'cue-acting-cap'); assert.strictEqual(seg.children[0].lastChild.nodeValue, 'Hive deciding…');
  assert.ok(!stripRow.children.some(c => c.classList.contains('cue-acting-cap')), 'never a sibling of the strip');
  stageTrackerPendingSync(hamPanel);
  assert.strictEqual(seg.children.length, 1, 'a re-sync does not duplicate the caption');
  _stageTrackerPending.length = 0; stageTrackerPendingSync(hamPanel);
  assert.strictEqual(seg.children.length, 0, 'cleared once the pending entry goes');
}

// ── 2. decided, effect pending ──────────────────────────────────────────
// The server projects the row onto the decided stage (060): the 090
// striped pill is gone, the pill reads the decision's effect with the
// countdown to the next adoption pass, and the ✓ badge's tooltip leads
// with who pressed what and what hive chose. No spinner anywhere. With
// one covering PR the issue row is the PR's row.
{
  const pr = original({ stage: 60, modifiers: [], manual_merge: false, human_merge_reason_text: '', decided: decidedPending, last_operator_action: acted, operator_action_count: 1 });
  const issue = issueOf([pr], { decided: decidedPending });
  const row = stageTrackerRowHtml(issue, stages, ctx);
  assert.ok(!/Ready for human merge/.test(text(row)), 'the 090 pill never remains once a decision exists: ' + text(row));
  assert.ok(!/untrusted author/.test(pill(strip(row))), pill(strip(row)));
  assert.ok(pill(strip(row)).includes('Adopted · hive rebases & merges next pass'), 'pill = the decision effect: ' + pill(strip(row)));
  assert.ok(pill(strip(row)).includes('in 7m'), 'countdown to the next pass: ' + pill(strip(row)));
  assert.ok(row.includes('data-ham-until-kind="decided"'), 'the countdown ticks live');
  assert.ok(row.includes('ham-acted ham-acted-waiting ham-acted-decided'), 'the ✓ is the decision badge, not a merge celebration: ' + (row.match(/ham-acted[^"]*/g) || []).join(','));
  assert.ok(!row.includes('cue-merged') && !row.includes('ham-token-fly'), 'no celebration cues');
  assert.ok(!row.includes('cue-acting') && !row.includes('toast-spinner'), 'no spinner coexists with the decision badge');
  const tip = text((/<span id="ham-acted-tip-[^"]*" class="config-tooltip ham-acted-tip"[^>]*>([\s\S]*?)<\/span>/.exec(row) || ['', ''])[1]);
  assert.ok(/^you · Let hive decide · hive chose adopt-supersede, 11:57( AM)? UTC/.test(tip), 'the badge names the decision and the trigger: ' + tip);
  assert.ok(tip.includes('Your note: Let hive do that · previewed adopt-supersede'), 'the pressed button is on the trail: ' + tip);
  assert.ok(tip.includes('Hive decided 11:57 UTC — adopted (supersede)') && tip.includes('next: adoption pass in 7m'), tip);
  assert.ok(!/Approving/.test(row), '"Approving" never renders for an adopt decision');
  // Two covering PRs: the decided one is a sub-row with the same pill.
  const other = { number: 15030, title: 'docs', url: 'u30', stage: 50, author: 'bob', author_kind: 'human', modifiers: [] };
  const row2 = stageTrackerRowHtml(issueOf([pr, other], { stage: 60 }), stages, ctx);
  const sub = subHtml(row2, 15029);
  assert.ok(sub, 'the PR sub-row renders');
  assert.ok(pill(sub).includes('Adopted · hive rebases & merges next pass') && pill(sub).includes('in 7m'), 'sub-row pill = the decision effect: ' + pill(sub));
  assert.ok(sub.includes('ham-acted ham-acted-waiting ham-acted-decided'), 'the sub-row carries the decision badge');
  assert.ok(!/Ready for human merge|untrusted author|Approving/.test(text(row2)), text(row2));
}

// ── 2b. the in-flight caption names the recorded decision ───────────────
assert.strictEqual(stageTrackerDecidedActing({ action: 'adopted' }, null), 'Adopting · opening hive copy…');
assert.strictEqual(stageTrackerDecidedActing({ action: 'approved' }, null), 'Approving…');
assert.strictEqual(stageTrackerDecidedActing({ action: 'directed' }, null), 'Sending back…');
assert.strictEqual(stageTrackerDecidedActing({ action: 'closed' }, null), 'Closing…');
assert.strictEqual(stageTrackerDecidedActing({ action: 'adopted' }, { decided: decidedPending }), 'Adopting · opening hive copy…', 'the server projection wins');
for (const a of ['adopted', 'directed', 'closed', 'merged', 'queued-for-merge', 'recommended', 'held', 'weird']) {
  assert.ok(!/Approving/.test(stageTrackerDecidedActing({ action: a }, null)), 'Approving never for ' + a);
}

// ── 2c. timed out: no effect past the window → amber, options via badge ─
{
  const timedOut = Object.assign({}, decidedPending, { effect: 'timed-out', next_at: undefined, text: 'Adopted · no effect yet' });
  const pr = original({ decided: timedOut, last_operator_action: Object.assign({}, acted, { state: 'stuck' }), operator_action_count: 1 });
  const row = stageTrackerRowHtml(issueOf([pr]), stages, ctx);
  assert.ok(row.includes('ham-acted ham-acted-stuck ham-acted-decided'), 'amber badge once the effect timed out: ' + (row.match(/ham-acted[^"]*/g) || []).join(','));
  assert.ok(!row.includes('toast-spinner') && !row.includes('cue-acting'), 'no spinner on a timed-out decision');
  assert.ok(!/Approving/.test(row));
}

// ── 3. landed: the adoption pair ────────────────────────────────────────
// The superseded original stays listed under the issue as one muted,
// chevron-less sub-row (owner likes the look, hivecommons/hive#15511) whose
// trailing note is short and never wraps: "superseded · hive adopted →
// #15123" — the "re-authored with sign-off" part moved into the hover. The
// copy carries the single active chevron and its "↳ adopted from #15029 ·
// you let hive decide 11:57 UTC" chip; the issue row's pill follows the copy.
{
  const landed = Object.assign({}, decidedPending, { effect: 'landed', superseded_by: 15123, next_at: undefined });
  const orig = original({ stage: 74, modifiers: [], manual_merge: false, human_merge_reason_text: '', adopted_by: 15123, adopted_by_state: 'open', superseded_by: 15123, decided: landed, last_operator_action: Object.assign({}, acted, { state: 'addressed' }), operator_action_count: 1 });
  const copy = { number: 15123, title: 'fix: tracker pill', url: 'u123', stage: 50, author: 'hivecommons-hive[bot]', author_kind: 'hive', modifiers: [], adopted_from: { number: 15029, url: 'u29', author: 'Karibhou', state: 'open', decided_by: 'clubanderson', decided_at: at, decided_action: 'let-hive-decide', verdict: 'adopt-supersede', text: '↳ adopted from #15029 · you let hive decide 11:57 UTC' } };
  const issue = issueOf([orig, copy], { stage: 50 });
  const row = stageTrackerRowHtml(issue, stages, ctx);
  // subs() counts full sub-rows only; the muted line is a "ham-sub ham-sub-superseded".
  const parts = (r) => r.split('<div class="ham-sub').slice(1);
  const part = (r, n) => parts(r).find(s => s.includes('pr#' + n + '"')) || '';
  assert.deepStrictEqual(parts(row).map(s => (/pr#(\d+)"/.exec(s) || [])[1]), ['15029', '15123'], 'two sub-rows, the original muted: ' + row);
  assert.strictEqual(subs(row).length, 1, 'one full sub-row: the copy');
  const dim = part(row, 15029);
  assert.ok(dim.startsWith(' ham-sub-superseded"'), 'the original is the muted line: ' + dim);
  assert.ok(dim.includes('data-ham-superseded-by="15123"'));
  const note = (/<span class="ham-superseded-note"[^>]*>([^<]*)<\/span>/.exec(dim) || [])[1] || '';
  assert.strictEqual(note, 'superseded · hive adopted → #15123', 'the note is short, one line, no parenthetical: ' + note);
  assert.ok(!note.includes('re-authored') && !note.includes('(') && !text(dim).includes('(hive re-authored'), 'no parenthetical in the row: ' + text(dim));
  const noteTip = (/<span class="ham-superseded-note" title="([^"]*)"/.exec(dim) || [])[1] || '';
  assert.strictEqual(noteTip, 'Hive re-authored this PR as #15123 with the same commits and sign-off. This PR closes as superseded when #15123 merges.', noteTip);
  assert.ok(!dim.includes('aria-label="Pipeline progress"') && !dim.includes('aria-current="step"'), 'no chevron track on the muted line');
  assert.ok(!dim.includes('ham-kebab') && !dim.includes('ham-sel'), 'no row tools on the muted line');
  assert.ok(text(dim).includes('#15029') && text(dim).includes('fix: tracker pill'), 'greyed title stays: ' + text(dim));
  const cp = part(row, 15123);
  assert.ok(cp.includes('aria-current="step"'), 'the copy carries the active chevron');
  assert.ok(cp.includes('class="ham-badge ham-adopted-from"') && /↳ adopted from #15029 · you let hive decide 11:57( AM)? UTC/.test(text(cp)), 'the adopted-from chip on the copy: ' + text(cp));
  const chipTip = (/class="ham-badge ham-adopted-from"[^>]*title="([^"]*)"/.exec(cp) || [])[1] || '';
  assert.ok(chipTip.includes('#15029 is superseded and still open; it closes when this merges.'), 'the chip tooltip names the superseded state: ' + chipTip);
  assert.ok(!text(cp).includes('untrusted author'), 'the copy is hive-authored');
  assert.strictEqual((row.match(/aria-current="step"/g) || []).length, 1, 'exactly one active chevron: the copy');
  assert.ok(pill(row).includes('CI / Review'), 'the active pill follows the copy: ' + pill(row));
  assert.ok(!/Approving/.test(row));
  // The same muted line without superseded_by (an older payload): adopted_by
  // with a live copy in the list is enough.
  const legacy = stageTrackerRowHtml(issueOf([original({ stage: 74, modifiers: [], manual_merge: false, human_merge_reason_text: '', adopted_by: 15123, adopted_by_state: 'open' }), copy], { stage: 50 }), stages, ctx);
  assert.ok(part(legacy, 15029).startsWith(' ham-sub-superseded"') && text(part(legacy, 15029)).includes('superseded · hive adopted → #15123'), 'legacy payload: ' + text(legacy));
  // A copy that was closed (recovering): the original is a live PR again
  // — a full sub-row with a chevron, no muted line.
  const recovering = issueOf([original({ stage: 85, modifiers: [], adopted_by: 15123, adopted_by_state: 'closed', superseded_by: 0 }), Object.assign({}, copy, { state: 'closed' })], { stage: 85 });
  assert.strictEqual(stageTrackerLinkedSuperseder(recovering.linked_prs[0], recovering), 0, 'a closed copy never supersedes its original');
  const rrow = stageTrackerRowHtml(recovering, stages, ctx);
  assert.ok(!rrow.includes('ham-sub-superseded') && rrow.includes('aria-label="Pipeline progress"'), 'recovering: the original is a live row: ' + rrow);
}

// ── 4. closed original ──────────────────────────────────────────────────
// Once the close-first close landed the original is still listed, in the
// same muted form, while its superseder is open: the note becomes
// "superseded · closed · hive adopted → #15123". With the copy gone from
// the list (merged, closed) the closed original goes with it.
{
  const closed = { number: 15029, title: 'fix: tracker pill', url: 'u29', stage: 74, state: 'closed', superseded_by: 15123, adopted_by: 15123, adopted_by_state: 'open', caption: 'superseded · closed · hive adopted → #15123', modifiers: [] };
  const copy = { number: 15123, title: 'fix: tracker pill', url: 'u123', stage: 50, author: 'hivecommons-hive[bot]', author_kind: 'hive', modifiers: [], adopted_from: { number: 15029, url: 'u29', author: 'Karibhou', state: 'closed', text: '↳ adopted from #15029' } };
  const row = stageTrackerRowHtml(issueOf([closed, copy], { stage: 50 }), stages, ctx);
  const parts = (r) => r.split('<div class="ham-sub').slice(1);
  const part = (r, n) => parts(r).find(s => s.includes('pr#' + n + '"')) || '';
  const dim = part(row, 15029);
  assert.ok(dim.startsWith(' ham-sub-superseded"'), 'the closed original renders as a muted line: ' + row);
  assert.strictEqual(subs(row).length, 1, 'one full sub-row: the copy');
  const note = (/<span class="ham-superseded-note"[^>]*>([^<]*)<\/span>/.exec(dim) || [])[1] || '';
  assert.strictEqual(note, 'superseded · closed · hive adopted → #15123', note);
  assert.ok(text(dim).includes('#15029') && text(dim).includes('fix: tracker pill'), 'cached title, no live fetch: ' + text(dim));
  assert.ok(!dim.includes('aria-label="Pipeline progress"'), 'no hollow chevrons');
  const noteTip = (/<span class="ham-superseded-note" title="([^"]*)"/.exec(dim) || [])[1] || '';
  assert.ok(noteTip.includes('This PR is closed as superseded.'), noteTip);
  const chipTip = (/class="ham-badge ham-adopted-from"[^>]*title="([^"]*)"/.exec(part(row, 15123)) || [])[1] || '';
  assert.ok(chipTip.includes('#15029 is closed as superseded.'), 'the chip tooltip says closed: ' + chipTip);
  assert.ok(!stageTrackerLinkedPROpen(stageTrackerLinkedPRItem(issueOf([closed]), closed)), 'a closed PR is not an open linked PR');
  // The issue row never shows a blank numbered sub-row.
  for (const s of row.split('<div class="ham-sub').slice(1)) assert.ok(text(s).replace(/^#\d+\s*$/, '') !== '', 'never a blank numbered row: ' + text(s));
  // Copy not listed (merged / closed): the closed original is not drawn.
  assert.ok(stageTrackerLinkedSupersededGone(closed, issueOf([closed])), 'no superseder listed: gone');
  assert.ok(stageTrackerLinkedSupersededGone(closed, issueOf([closed, Object.assign({}, copy, { state: 'closed' })])), 'closed superseder: gone');
  assert.deepStrictEqual(stageTrackerLinkedSorted(issueOf([closed, copy], { stage: 50 }), stages).map(lp => lp.number), [15029, 15123]);
  assert.deepStrictEqual(stageTrackerLinkedSorted(issueOf([closed], { stage: 74 }), stages).map(lp => lp.number), []);
  const alone = stageTrackerRowHtml(issueOf([closed], { stage: 74 }), stages, ctx);
  assert.ok(!alone.includes('<span class="ham-pr-num">#15029</span>'), 'the orphaned closed original is not drawn: ' + text(alone));
}

// ── 5. superseded text is pure and conservative ─────────────────────────
assert.strictEqual(stageTrackerLinkedSupersededText({ state: 'closed', superseded_by: 9 }, { linked_prs: [{ number: 9 }] }), 'superseded · closed · hive adopted → #9');
assert.strictEqual(stageTrackerLinkedSupersededText({ state: 'closed', superseded_by: 9 }, null), '', 'a closed original without its copy listed says nothing');
assert.strictEqual(stageTrackerLinkedSupersededText({ adopted_by: 9, adopted_by_state: 'open' }, { linked_prs: [{ number: 9 }] }), 'superseded · hive adopted → #9');
assert.strictEqual(stageTrackerLinkedSupersededText({ superseded_by: 9 }, { linked_prs: [{ number: 9 }] }), 'superseded · hive adopted → #9');
assert.strictEqual(stageTrackerLinkedSupersededText({ adopted_by: 9, adopted_by_state: 'open' }, { linked_prs: [{ number: 8 }] }), '');
assert.strictEqual(stageTrackerLinkedSupersededText({ adopted_by: 9, adopted_by_state: 'closed' }, { linked_prs: [{ number: 9 }] }), '', 'a closed copy keeps the original live (recovering)');
assert.strictEqual(stageTrackerLinkedSupersededText({ adopted_by: 9, adopted_by_state: 'open' }, { linked_prs: [{ number: 9, state: 'closed' }] }), '', 'a copy listed closed does not supersede');
assert.strictEqual(stageTrackerLinkedSupersededText({}, null), '');
assert.strictEqual(stageTrackerLinkedSupersededTip({ superseded_by: 9 }), 'Hive re-authored this PR as #9 with the same commits and sign-off. This PR closes as superseded when #9 merges.');
assert.strictEqual(stageTrackerLinkedSupersededTip({ adopted_by: 9, state: 'closed' }), 'Hive re-authored this PR as #9 with the same commits and sign-off. This PR is closed as superseded.');
assert.strictEqual(stageTrackerSupersededOriginalClause(9, 'open'), ' #9 is superseded and still open; it closes when this merges.');
assert.strictEqual(stageTrackerSupersededOriginalClause(9, 'closed'), ' #9 is closed as superseded.');
assert.strictEqual(stageTrackerSupersededOriginalClause(9, ''), '');
assert.strictEqual(stageTrackerSupersededOriginalClause(0, 'open'), '');
`)
}
