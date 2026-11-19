package dashboard

import (
	"strings"
	"testing"
)

// stageTrackerHumanGateJS lifts the human-gate pill surface
// (tracker-human-gate.js) for every node harness that renders rows: the
// reason/verb tables and the pure helpers stageTrackerRowHtml composes.
func stageTrackerHumanGateJS(t *testing.T, html string) string {
	t.Helper()
	return strings.Join([]string{
		jsConstLine(t, html, "STAGE_TRACKER_GATE_MODS"),
		jsConstLine(t, html, "STAGE_TRACKER_GATE_WORD"),
		jsConstLine(t, html, "STAGE_TRACKER_GATE_SETTLING"),
		jsConstLine(t, html, "STAGE_TRACKER_GATE_VISIBLE"),
		jsConstLine(t, html, "STAGE_TRACKER_GATE_FOLD_QUERY"),
		jsConstBlock(t, html, "STAGE_TRACKER_GATE_VERBS"),
		jsConstLine(t, html, "STAGE_TRACKER_GATE_ORDER"),
		jsConstLine(t, html, "STAGE_TRACKER_GATE_PR_ONLY"),
		jsConstBlock(t, html, "STAGE_TRACKER_GATE_LABELS"),
		jsConstLine(t, html, "STAGE_TRACKER_GATE_APPROVE_AS_IS"),
		jsConstBlock(t, html, "STAGE_TRACKER_GATE_POSTS"),
		jsFunc(t, html, "stageTrackerGateIsPR"),
		jsFunc(t, html, "stageTrackerHumanGateReasons"),
		jsFunc(t, html, "stageTrackerHumanGateVerbs"),
		jsFunc(t, html, "stageTrackerHumanGateStatusText"),
		jsFunc(t, html, "stageTrackerHumanGateTip"),
		jsFunc(t, html, "stageTrackerHumanGateVerbState"),
		jsFunc(t, html, "stageTrackerHumanGateVerbHtml"),
		jsFunc(t, html, "stageTrackerHumanGateVisible"),
		jsFunc(t, html, "stageTrackerHumanGateHtml"),
	}, "\n")
}

// TestStageTrackerHumanGateWiring pins the markup, CSS and action-path
// surface of the one human-gate control (hivecommons/hive#17470): the
// module is in the bundle, the chevron never wears a held/sentinel word,
// the CSS is token-only, and the verbs route through runRowAction.
func TestStageTrackerHumanGateWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`function stageTrackerHumanGateReasons(item)`,
		`function stageTrackerHumanGateHtml(item, shown, nowMs, subRows, opts)`,
		`function stageTrackerHumanGateSettling(button)`,
		`function stageTrackerWireHumanGate(el)`,
		`const STAGE_TRACKER_GATE_WORD = 'Needs human';`,
		`const STAGE_TRACKER_GATE_SETTLING = 'settling…';`,
		`const STAGE_TRACKER_GATE_VISIBLE = 3;`,
		`gateHere ? 'ham-human-gated' : ''`,
		`.ham-human-gate {`,
		`.ham-seg.active.ham-human-gated {`,
		`.ham-human-gate.ham-gate-settling`,
		`'gate:decide': 'Decide…'`,
		`'gate:request-changes': 'Request changes…'`,
		`'gate:confirm-sentinel': 'Confirm safe'`,
		`/sentinel-confirm`,
		`stageTrackerHumanGateSettling(b)`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing human-gate wiring %q", want)
		}
	}
	for _, gone := range []string{
		`'Held · sentinel · confirm intent'`,
		`'Held · hold'`,
		`'Sentinel alert · needs human'`,
		`heldGate ? 'ham-held' : ''`,
		`.ham-seg.active.ham-held`,
		`.ham-seg.active.ham-sentinel {`,
		`function stageTrackerHeldWord(`,
		`function stageTrackerSentinelAsSuffix(`,
	} {
		if strings.Contains(html, gone) {
			t.Errorf("index.html still carries the chevron-word human gate %q", gone)
		}
	}
	start := strings.Index(html, "/* ---- The human gate: one control per gated row")
	if start < 0 {
		t.Fatal("human-gate CSS/JS block not found")
	}
	cssStart := strings.Index(html, ".ham-human-gate {")
	cssEnd := strings.Index(html[cssStart:], ".ham-gate-menu[hidden]")
	if cssStart < 0 || cssEnd < 0 {
		t.Fatal("human-gate CSS block not found")
	}
	if css := html[cssStart : cssStart+cssEnd]; strings.Contains(css, "#") || strings.Contains(css, "rgb(") || strings.Contains(css, "rgba(") {
		t.Errorf("human-gate CSS must use tokens only, found a literal colour:\n%s", css)
	}
}

// TestStageTrackerHumanGateRows runs the row builders under node against the
// maintainer's screenshots (hivecommons/hive#17470, 2026-11-10): every gated
// row carries exactly ONE human-gate pill whose verbs follow its reasons,
// "Needs human" / "Held" never ride a chevron, the lit chevron is the real
// stage with the amber outline, and a secondary row of a shared PR renders
// no gate of its own — only the link to the row that does.
func TestStageTrackerHumanGateRows(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowsPreludeJS(t, html)+"\n"+
		jsFunc(t, html, "stageTrackerHumanAttentionOf")+"\n"+
		jsFunc(t, html, "stageTrackerNeedsHumanAsk")+"\n"+
		jsFunc(t, html, "stageTrackerHoldPill")+"\n"+
		jsFunc(t, html, "stageTrackerHoldTip")+"\n"+
		stageTrackerRowsFixtureJS+`
const seen = new Date(now - 60000).toISOString();
const visible = (html) => strip(html).split('<p class="ham-current-stage">')[0].replace(/<span [^>]*class="config-tooltip[^"]*"[^>]*>[\s\S]*?<\/span>/g, '').replace(/<[^>]*>/g, ' ');
const gates = (html) => (html.match(/class="ham-human-gate"/g) || []).length;
const reasons = (html) => (/data-ham-gate-reasons="([^"]*)"/.exec(html) || [])[1] || '';
const verbs = (html) => Array.from(html.matchAll(/data-ham-act="([^"]*)"[^>]*>([^<]*)<\/button>/g)).map(m => m[2]).filter((v, i, a) => a.indexOf(v) === i);
const gateWord = (html) => (/<span class="ham-gate-status[^"]*"[^>]*>[\s\S]*?<span class="ham-mod-word">([^<]*)<\/span>/.exec(html) || [])[1] || '';
const gateTip = (html) => (/<span class="ham-gate-status[^"]*" title="([^"]*)"/.exec(html) || [])[1] || '';

// (a) hive#17470 / PR #17481: a requires_human review verdict + a sentinel
// alert with CI still running. One lit chevron (CI, amber-outlined), no
// "Needs human" in any chevron, one gate with both reasons and the union
// of their verbs (3 visible + the rest behind "…").
const head = '4f1c2e9';
const question = 'This PR edits the release workflow. Should a maintainer sign off on the permissions change?';
const row17481 = { repo: 'hivecommons/hive', number: 17481, kind: 'pr', stage: 85, head, author: 'hive[bot]', author_kind: 'hive', modifiers: ['needs-decision'],
  caption: 'Needs human decision', decision_needed: { question, asked_by: 'reviewer', since: seen, options: [{ action: 'decide:approve', label: 'Approve', enabled: true }, { action: 'decide:delegate', label: 'Let hive decide', enabled: true }] },
  sentinel_alert: true, sentinel_reason: 'touches .github/workflows (+1 more)',
  ci_activity: { head, state: 'running', active: true, caption: 'CI 61/73 · 12 running', checks_passed: 61, checks_total: 73, source: 'checks', observed_at: seen },
  review_activity: { head, state: 'human_decision', caption: 'Review 5/5 · needs human decision', blocker: question, perspectives_done: 5, perspectives_total: 5, source: 'verified-review-bundle', observed_at: seen, since: seen } };
let row = stageTrackerRowHtml(row17481, stages, ctx);
assert.strictEqual(litChevrons(strip(row)), 1, 'one lit chevron: ' + strip(row));
assert.ok(/<li class="ham-seg ham-activity-track active ham-human-gated ham-current" data-track="ci"/.test(row), 'CI is the lit chevron, amber-marked: ' + strip(row));
assert.ok(!/Needs human|Held ·|Sentinel alert/.test(visible(row)), 'no gate word on any chevron: ' + visible(row));
assert.strictEqual(gates(row), 1, 'exactly one human-gate control: ' + row);
assert.strictEqual(reasons(row), 'review sentinel', 'review verdict + sentinel alert stack (the 085 park is the review\'s own question, not a second reason): ' + reasons(row));
assert.strictEqual(gateWord(row), 'Needs human · review, sentinel');
assert.ok(gateTip(row).includes(question.replace(/\?/g, '?')) && gateTip(row).includes('touches .github/workflows'), 'the status hover is the question and the rule: ' + gateTip(row));
assert.deepStrictEqual(verbs(row), ['Approve', 'Request changes…', 'Let hive decide', 'Confirm safe'], 'union of the review and sentinel verb sets, de-duplicated');
assert.ok(row.includes('data-ham-gate-more') && /<template class="ham-gate-menu-tpl"><div class="ham-menu ham-gate-menu"[^>]*>[\s\S]*?Confirm safe/.test(row), 'the fourth verb waits in the … menu template: ' + row);
assert.ok(!/ham-gate-menu-tpl[\s\S]*Let hive decide/.test(row), 'the menu holds only the folded verbs, not copies of the inline ones');
assert.ok(!row.includes('ham-badge-sentinel') && !row.includes('ham-mod-needs-decision') && !row.includes('ham-lead-pill'), 'no sentinel chip, needs-decision pill or loose lead buttons beside the gate: ' + row);
assert.ok(row.includes('<p class="ham-current-stage">') && row.includes('Needs human decision'), 'the expander line still names the decision');

// (b) crux#139 / PR #140: a green, reviewed PR at 090 with a sentinel
// alert and CI re-running. The lit chevron is CI; "Held · sentinel" is
// gone from the Merge queue chevron and is the gate's sentinel reason,
// which finally has its verb: Confirm safe.
const head140 = 'c0ffee1';
const row140 = { repo: 'hivecommons/crux', number: 140, kind: 'pr', stage: 90, head: head140, author: 'hive[bot]', author_kind: 'hive', reason: 'a sentinel alert is open on this PR',
  sentinel_alert: true, sentinel_reason: 'touches release signing (+2 more)',
  ci_activity: { head: head140, state: 'running', active: true, caption: 'CI 18/19 · tide running', checks_passed: 18, checks_total: 19, source: 'checks', observed_at: seen },
  review_activity: { head: head140, state: 'approved', caption: 'Review approved', perspectives_done: 5, perspectives_total: 5, source: 'verified-review-bundle', observed_at: seen, since: seen } };
row = stageTrackerRowHtml(row140, stages, ctx);
assert.strictEqual(litChevrons(strip(row)), 1, 'one lit chevron: ' + strip(row));
assert.ok(/<li class="ham-seg ham-activity-track active ham-human-gated[^"]*" data-track="ci"/.test(row), 'CI is the lit chevron: ' + strip(row));
assert.ok(!/Held|Sentinel alert|Needs human/.test(visible(row)), 'no held word on the Merge queue chevron: ' + visible(row));
assert.strictEqual(gates(row), 1, row);
assert.strictEqual(reasons(row), 'sentinel');
assert.strictEqual(gateWord(row), 'Needs human · sentinel');
assert.ok(gateTip(row).includes('touches release signing'), 'the hover is the sentinel rule: ' + gateTip(row));
assert.deepStrictEqual(verbs(row), ['Confirm safe', 'Let hive decide']);
assert.ok(!row.includes('ham-badge-sentinel'), 'the sentinel chip is folded into the gate');
// The same PR with CI green: the Merge queue chevron is lit, amber, and still reads its own column word.
const settled = stageTrackerRowHtml(Object.assign({}, row140, { ci_activity: { head: head140, state: 'passing', caption: 'CI green', checks_passed: 19, checks_total: 19, source: 'checks', observed_at: seen } }), stages, ctx);
assert.ok(/<li class="ham-seg active[^"]*ham-human-gated"/.test(settled), 'the stage chevron wears the outline: ' + strip(settled));
assert.ok(!/Held|Sentinel/.test(visible(settled)), visible(settled));
assert.strictEqual(gates(settled), 1);

// (c) hive#17451 / PR #17480 "needs decision": the gate offers Decide…
// (the question pre-filled), Approve as-is and Let hive decide.
const q17480 = 'Two fixes are possible — pin the runner image or drop the step. Which?';
const row17480 = { repo: 'hivecommons/hive', number: 17480, kind: 'pr', stage: 85, head: 'd3c1de0', author: 'hive[bot]', author_kind: 'hive', modifiers: ['needs-decision'], caption: 'Needs human decision',
  decision_needed: { question: q17480, asked_by: 'the hive', since: seen, options: [{ action: 'decide:approve', label: 'Approve', enabled: true }, { action: 'decide:delegate', label: 'Let hive decide', enabled: true }] } };
row = stageTrackerRowHtml(row17480, stages, ctx);
assert.strictEqual(gates(row), 1, row);
assert.strictEqual(reasons(row), 'decision');
assert.deepStrictEqual(verbs(row), ['Decide…', 'Approve as-is', 'Let hive decide']);
assert.ok(gateTip(row).includes('Two fixes are possible'), gateTip(row));
assert.ok(!row.includes('data-ham-gate-more'), 'three verbs need no … on the strip tier');
assert.strictEqual((row.match(/class="ham-lead-act ham-gate-act[^"]*"/g) || []).length, 3, 'all three verbs are inline');
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, 'active detoured ham-human-gated', 'todo cap'], strip(row));
assert.ok(row.includes('<span class="ham-detour">Merge queue</span>'), 'the chevron reads its column: ' + strip(row));

// hold + decision stack: reasons in canonical order, verbs unioned.
const held = Object.assign({}, row17480, { number: 17490, modifiers: ['needs-decision', 'hold'], labels: ['do-not-merge/hold'], hold: { by: 'clubanderson', reason: 'wait for the release', since: seen } });
row = stageTrackerRowHtml(held, stages, ctx);
assert.strictEqual(gates(row), 1, row);
assert.strictEqual(reasons(row), 'decision hold');
assert.strictEqual(gateWord(row), 'Needs human · decision, hold');
assert.deepStrictEqual(verbs(row), ['Decide…', 'Approve', 'Let hive decide', 'Release hold'], '"Approve as-is" only when the decision stands alone');
assert.ok(gateTip(row).includes('Held by @clubanderson'), 'the hover names the holder: ' + gateTip(row));
assert.ok(!row.includes('ham-mod-hold">hold</span>'), 'the hold chip is folded into the gate');

// Verb sets per reason are fixed (the maintainer's "consistent way").
const ids = (keys, item) => stageTrackerHumanGateVerbs(keys.map(key => ({ key, tip: '' })), item).map(v => v.label);
assert.deepStrictEqual(ids(['review']), ['Approve', 'Request changes…', 'Let hive decide']);
assert.deepStrictEqual(ids(['decision']), ['Decide…', 'Approve as-is', 'Let hive decide']);
assert.deepStrictEqual(ids(['sentinel']), ['Confirm safe', 'Let hive decide']);
assert.deepStrictEqual(ids(['hold']), ['Release hold', 'Let hive decide']);
assert.deepStrictEqual(ids(['review', 'decision', 'sentinel', 'hold']), ['Approve', 'Request changes…', 'Let hive decide', 'Decide…', 'Confirm safe', 'Release hold']);
// An issue with no PR has no head to approve or send back.
assert.deepStrictEqual(ids(['decision'], { kind: 'issue', stage: 45 }), ['Decide…', 'Let hive decide']);
assert.deepStrictEqual(ids(['hold'], { kind: 'issue', stage: 10 }), ['Release hold', 'Let hive decide']);

// What is not a gate: a draft, a merged head, an informational notice.
assert.deepStrictEqual(stageTrackerHumanGateReasons(Object.assign({}, row140, { sentinel_notice: true })), []);
assert.deepStrictEqual(stageTrackerHumanGateReasons(Object.assign({}, row17480, { modifiers: ['needs-decision', 'draft'] })), []);
assert.deepStrictEqual(stageTrackerHumanGateReasons({ kind: 'pr', stage: 110, modifiers: ['merged', 'hold'] }), []);
// hivecommons/hive#17789: a 071 row flagged needs_human on a DCO sign-off
// ask is the author's move, not a decision; another 071 reason still is.
const row17780 = { repo: 'hivecommons/hive', number: 17780, kind: 'pr', stage: 71, head: '0e5ccbe', author: 'clubanderson', author_kind: 'human', needs_human: true, reason: 'hive-signoff-attribution' };
assert.deepStrictEqual(stageTrackerHumanGateReasons(row17780), []);
assert.deepStrictEqual(stageTrackerHumanGateReasons(Object.assign({}, row17780, { reason: 'dco-missing' })), []);
assert.deepStrictEqual(stageTrackerHumanGateReasons(Object.assign({}, row17780, { reason: 'label:needs-rebase' })).map(r => r.key), ['decision']);
assert.deepStrictEqual(stageTrackerHumanGateReasons(Object.assign({}, row17780, { modifiers: ['needs-decision'] })).map(r => r.key), ['decision'], 'an explicit needs-decision still gates');
assert.ok(!stageTrackerRowHtml({ repo: 'acme/console', number: 1, kind: 'pr', stage: 50 }, stages, ctx).includes('ham-human-gate'), 'a clean row has no gate');

// (d) secondary rows #17463 / #17476 of the shared PR #17480 (primary row
// #17451): no gate, no lit needs-human chevron of their own; the chip is
// the link to the primary row.
const prShared = Object.assign({}, row17480, { drawn_under: 17451, title: 'fix', url: 'https://x/17480' });
const primary = { repo: 'hivecommons/hive', number: 17451, kind: 'issue', stage: 40, title: 'Issue 17451', linked_prs: [Object.assign({}, prShared, { also_closes: [{ number: 17463 }, { number: 17476 }] })] };
const secondary = { repo: 'hivecommons/hive', number: 17463, kind: 'issue', stage: 40, title: 'Issue 17463', linked_prs: [prShared] };
const shared = { parentOf: { 'hivecommons/hive/issue#17463': 'hivecommons/hive/issue#17451' }, children: { 'hivecommons/hive/issue#17451': ['hivecommons/hive/issue#17463'] }, prs: {}, collapsed: {} };
const ctxS = Object.assign({}, ctx, { shared });
const headRow = stageTrackerRowHtml(primary, stages, ctxS);
const childRow = stageTrackerRowHtml(secondary, stages, ctxS);
assert.strictEqual(gates(headRow), 1, 'the primary carries the gate: ' + headRow);
assert.strictEqual(gates(childRow), 0, 'the secondary carries none: ' + childRow);
assert.ok(!/Needs human/.test(visible(childRow)), 'no needs-human chevron on the secondary: ' + visible(childRow));
assert.ok(!/ham-human-gated/.test(strip(childRow)), 'no amber outline on the secondary either: ' + strip(childRow));
const link = /<button type="button" class="ham-badge ham-decision-pending" data-ham-scroll-to="([^"]*)" data-ham-scroll-row="([^"]*)"[^>]*>([^<]*)<\/button>/.exec(childRow);
assert.ok(link, 'the chip is a link: ' + childRow);
assert.strictEqual(link[1], 'hivecommons/hive/pr#17480');
assert.strictEqual(link[2], 'hivecommons/hive/issue#17451');
assert.strictEqual(link[3], 'decision pending on #17480');
`)
}

// TestStageTrackerHumanGateFoldsOnStackedHead pins the render-time fold from
// #17515: at the stacked-head breakpoint (STAGE_TRACKER_GATE_FOLD_QUERY) the
// pill keeps ONE verb inline and parks the rest in the … template, while
// the strip tier still shows STAGE_TRACKER_GATE_VISIBLE. The node harness
// has no matchMedia, so without a stub this arm never ran.
func TestStageTrackerHumanGateFoldsOnStackedHead(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowsPreludeJS(t, html)+"\n"+
		jsFunc(t, html, "stageTrackerHumanAttentionOf")+"\n"+
		jsFunc(t, html, "stageTrackerNeedsHumanAsk")+"\n"+
		jsFunc(t, html, "stageTrackerHoldPill")+"\n"+
		jsFunc(t, html, "stageTrackerHoldTip")+"\n"+
		stageTrackerRowsFixtureJS+`
const seen = new Date(now - 60000).toISOString();
const inlineVerbs = (html) => Array.from(html.split('<template')[0].matchAll(/<button type="button" class="ham-lead-act ham-gate-act" [^>]*>([^<]*)<\/button>/g)).map(m => m[1]);
const menuVerbs = (html) => { const tpl = /<template class="ham-gate-menu-tpl">([\s\S]*?)<\/template>/.exec(html); return tpl ? Array.from(tpl[1].matchAll(/role="menuitem"[^>]*>([^<]*)<\/button>/g)).map(m => m[1]) : []; };
const seenQueries = [];
let stacked = true;
window.matchMedia = (q) => { seenQueries.push(q); return { matches: stacked && q === STAGE_TRACKER_GATE_FOLD_QUERY, addEventListener() {} }; };

const question = 'This PR edits the release workflow. Should a maintainer sign off on the permissions change?';
const four = { repo: 'hivecommons/hive', number: 17481, kind: 'pr', stage: 85, head: '4f1c2e9', author: 'hive[bot]', author_kind: 'hive', modifiers: ['needs-decision'],
  caption: 'Needs human decision', decision_needed: { question, asked_by: 'reviewer', since: seen, options: [{ action: 'decide:approve', label: 'Approve', enabled: true }, { action: 'decide:delegate', label: 'Let hive decide', enabled: true }] },
  sentinel_alert: true, sentinel_reason: 'touches .github/workflows (+1 more)',
  ci_activity: { head: '4f1c2e9', state: 'running', active: true, caption: 'CI 61/73 · 12 running', checks_passed: 61, checks_total: 73, source: 'checks', observed_at: seen },
  review_activity: { head: '4f1c2e9', state: 'human_decision', caption: 'Review 5/5 · needs human decision', blocker: question, perspectives_done: 5, perspectives_total: 5, source: 'verified-review-bundle', observed_at: seen, since: seen } };
const three = { repo: 'hivecommons/hive', number: 17480, kind: 'pr', stage: 85, head: 'd3c1de0', author: 'hive[bot]', author_kind: 'hive', modifiers: ['needs-decision'], caption: 'Needs human decision',
  decision_needed: { question: 'Which fix?', asked_by: 'the hive', since: seen, options: [{ action: 'decide:approve', label: 'Approve', enabled: true }, { action: 'decide:delegate', label: 'Let hive decide', enabled: true }] } };
const two = { repo: 'hivecommons/crux', number: 140, kind: 'pr', stage: 90, head: 'c0ffee1', author: 'hive[bot]', author_kind: 'hive', reason: 'a sentinel alert is open on this PR', sentinel_alert: true, sentinel_reason: 'touches release signing (+2 more)',
  ci_activity: { head: 'c0ffee1', state: 'running', active: true, caption: 'CI 18/19 · tide running', checks_passed: 18, checks_total: 19, source: 'checks', observed_at: seen },
  review_activity: { head: 'c0ffee1', state: 'approved', caption: 'Review approved', perspectives_done: 5, perspectives_total: 5, source: 'verified-review-bundle', observed_at: seen, since: seen } };

// Stacked head: one verb inline, the rest behind …, nothing duplicated.
assert.strictEqual(stageTrackerHumanGateVisible(), 1, 'the fold query matched, so one verb rides inline');
assert.ok(seenQueries.includes('(max-width: 1100px)'), 'the fold is asked of the stacked-head breakpoint: ' + seenQueries);
let row = stageTrackerRowHtml(four, stages, ctx);
assert.deepStrictEqual(inlineVerbs(row), ['Approve'], 'four verbs: only the first is inline: ' + row);
assert.deepStrictEqual(menuVerbs(row), ['Request changes…', 'Let hive decide', 'Confirm safe'], 'the other three wait in the template, in pill order');
assert.ok(row.includes('data-ham-gate-more') && row.includes('title="Request changes… · Let hive decide · Confirm safe"'), 'the … lists what it folds: ' + row);
row = stageTrackerRowHtml(three, stages, ctx);
assert.deepStrictEqual(inlineVerbs(row), ['Decide…'], 'three verbs fold too on the stacked head: ' + row);
assert.deepStrictEqual(menuVerbs(row), ['Approve as-is', 'Let hive decide']);
row = stageTrackerRowHtml(two, stages, ctx);
assert.deepStrictEqual(inlineVerbs(row), ['Confirm safe']);
assert.deepStrictEqual(menuVerbs(row), ['Let hive decide'], 'even a two-verb pill keeps one line');
assert.strictEqual((row.match(/ham-gate-more-wrap/g) || []).length, 1, 'exactly one … per pill');

// Strip tier (query does not match): back to three inline; the … only
// appears once a fourth verb exists.
stacked = false;
assert.strictEqual(stageTrackerHumanGateVisible(), STAGE_TRACKER_GATE_VISIBLE);
row = stageTrackerRowHtml(three, stages, ctx);
assert.deepStrictEqual(inlineVerbs(row), ['Decide…', 'Approve as-is', 'Let hive decide']);
assert.ok(!row.includes('data-ham-gate-more') && !row.includes('<template'), 'no … and no template when nothing folds: ' + row);
row = stageTrackerRowHtml(four, stages, ctx);
assert.deepStrictEqual(inlineVerbs(row), ['Approve', 'Request changes…', 'Let hive decide']);
assert.deepStrictEqual(menuVerbs(row), ['Confirm safe']);

// No matchMedia at all (the harness default, and any non-browser caller):
// the strip tier count, never a throw.
delete window.matchMedia;
assert.strictEqual(stageTrackerHumanGateVisible(), STAGE_TRACKER_GATE_VISIBLE);
`)
}
