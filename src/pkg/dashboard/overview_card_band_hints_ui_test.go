package dashboard

import (
	"strings"
	"testing"
)

// overviewCardBandHintsFixture is a PR donut spread over seven repos of one
// owner: Decide holds 15 PRs (api 8, cli 2, docs 2 and one each in core,
// server and web), five of them red on the same smoke check; Approve holds
// 8 (core 7, cli 1), six of which already have one approval while GitHub
// still requires a review; Merge by hand holds 2 in server, both red on e2e
// desktop with two approvals; Review holds one PR in lib. hintRows reads
// every legend row's label and its hover lines (the title attribute, lines
// joined with &#10;).
const overviewCardBandHintsFixture = `
const hintRepo = (name, openPrs) => ({ name, full: 'o/' + name, issues: 0, prs: openPrs.length, actionableIssues: [], heldIssues: [], nonActionableIssues: [], openPrs, heldPrs: [], nonActionablePrs: [] });
const hintPR = (number, band, extra) => cardRow(number, band, 'your-turn', extra);
const oneApproval = { protection: { review_decision: 'REVIEW_REQUIRED', approvals_given: 1 } };
function hintRepos() {
  return [
    hintRepo('api', [1, 2, 3, 4, 5, 6, 7, 8].map(n => hintPR(n, 'decide', n <= 5 ? { failing_checks: n === 1 ? ['smoke', 'lint'] : ['smoke'] } : {}))),
    hintRepo('cli', [hintPR(1, 'decide'), hintPR(2, 'decide'), hintPR(3, 'approve', { protection: { review_decision: 'APPROVED', approvals_given: 1 } })]),
    hintRepo('docs', [hintPR(1, 'decide'), hintPR(2, 'decide')]),
    hintRepo('core', [hintPR(1, 'decide')].concat([2, 3, 4, 5, 6, 7].map(n => hintPR(n, 'approve', oneApproval)), [hintPR(8, 'approve', { protection: { review_decision: 'REVIEW_REQUIRED' } })])),
    hintRepo('server', [hintPR(1, 'decide'), hintPR(2, 'merge-by-hand', { failing_checks: ['e2e desktop'], protection: { review_decision: 'REVIEW_REQUIRED', approvals_given: 2 } }), hintPR(3, 'merge-by-hand', { failing_checks: ['e2e desktop'], protection: { review_decision: 'REVIEW_REQUIRED', approvals_given: 2 } })]),
    hintRepo('web', [hintPR(1, 'decide')]),
    hintRepo('lib', [hintPR(1, 'review', { failing_checks: ['contract'], card_reason: 'fails contract' })]),
  ];
}
function hintCard() {
  const card = cardFixture();
  card.prs.find(s => s.key === 'decide').rule = 'The hive stopped and is waiting for your call.';
  card.prs.find(s => s.key === 'approve').rule = "GitHub won't merge this without a person's approval.";
  card.prs.find(s => s.key === 'review').rule = 'The hive wants a person to look at it before it goes on.';
  return card;
}
const hintRows = html => [...html.matchAll(/<div class="[^"]*"[^>]* title="([^"]*)">(?:<span class="overview-chart-swatch[^"]*"(?: aria-hidden="true")?><\/span>)<span class="overview-chart-legend-label">([^<]*)(?:<span class="overview-chart-legend-sr">,<\/span> <span class="overview-chart-legend-note">[^<]*<\/span>)?<\/span>/g)]
  .map(m => ({ label: m[2], lines: m[1].split('&#10;') }));
const hintOf = (rows, label) => {
  const row = rows.find(r => r.label === label);
  assert.ok(row, label + ' row');
  return row.lines;
};
`

// A card legend row's hover text is its rule, then where its items are
// (largest repos first, at most three, then the rest as one count), then
// what two or more of them share, read from failing_checks and GitHub's
// review facts. A line with nothing to say is left out, so a one-repo band
// with nothing shared reads as two lines and a zero band as its rule alone.
// Group and subtotal rows read the items of their bands.
func TestOverviewCardBandHoverHints(t *testing.T) {
	html := indexHTML(t)
	script := overviewCardUIPrelude(t, html) + overviewCardBandHintsFixture + `
window._lastStatus = { hiveId: 'h', overview_card: hintCard() };
const repos = hintRepos();
_overviewLastRepos = repos;
const rows = hintRows(overviewLegendHTML(overviewPRBandSlices(repos), 'prs', {}));
assert.deepEqual(hintOf(rows, 'Decide'), [
  'Decide: the hive stopped and is waiting for your call.',
  'Where: api 8, cli 2, docs 2, 3 other repos 3',
  '5 fail the same check: smoke',
]);
// Only REVIEW_REQUIRED rows count; an approved PR and one with no approval
// do not.
assert.deepEqual(hintOf(rows, 'Approve'), [
  'Approve: GitHub won&#39;t merge this without a person&#39;s approval.',
  'Where: core 7, cli 1',
  '6 have 1 approval; GitHub still requires a review',
]);
// Both kinds, check first. The approval line never says how many more
// approvals are needed: the missing review can be a code owner's.
assert.deepEqual(hintOf(rows, 'Merge by hand'), [
  'Merge by hand: merge by hand rule.',
  'Where: server 2',
  '2 fail the same check: e2e desktop',
  '2 have 2 approvals; GitHub still requires a review',
]);
// One repo, one PR: no shared line, no empty line. Reason text is never
// read: card_reason names the check, failing_checks has it once.
assert.deepEqual(hintOf(rows, 'Review'), [
  'Review: the hive wants a person to look at it before it goes on.',
  'Where: lib 1',
]);
// A zero band keeps its rule alone.
assert.deepEqual(hintOf(rows, 'No rule matched (hive bug)'), ['No rule matched (hive bug): no rule matched (hive bug) rule.']);
// The Needs a person subtotal reads every band's items in its section.
assert.deepEqual(hintOf(rows, 'Needs a person'), [
  'Needs a person: no agent will move these until a person acts.',
  'Where: api 8, core 8, cli 3, 4 other repos 7',
  '5 fail the same check: smoke',
  '6 have 1 approval; GitHub still requires a review',
]);
// Status has no items here: its rule alone.
assert.equal(hintOf(rows, 'Status').length, 1);
// Issue rows carry neither fact, so an issue row gets the where line only;
// a group row reads its bands' items.
const repoA = cardRepoA();
window._lastStatus = { hiveId: 'h', overview_card: cardFixture() };
const issueRows = hintRows(overviewLegendHTML(overviewIssueBandSlices([repoA]), 'issues', {}));
assert.deepEqual(hintOf(issueRows, 'Needs a person'), ['Needs a person: no agent will move these until a person acts.', 'Where: a 3']);
assert.deepEqual(hintOf(issueRows, 'Waiting on someone else'), ['Waiting on someone else: someone other than you has the next move. Nothing to do until they act.', 'Where: a 2']);
for (const r of rows.concat(issueRows)) {
  assert.ok(r.lines.every(Boolean), r.label + ': no empty line');
  assert.doesNotMatch(r.lines.join('\n'), /[\u2013\u2014]|@/, r.label);
}
// The owner test is made once for the whole donut: with a second owner on
// the chart, a one-owner row keeps full names too, so a repo reads the same
// in every row.
window._lastStatus = { hiveId: 'h', overview_card: hintCard() };
const mixed = [hintRepo('api', [hintPR(1, 'decide')]), Object.assign(hintRepo('web', [hintPR(1, 'review')]), { full: 'p/web' })];
const mixedRows = hintRows(overviewLegendHTML(overviewPRBandSlices(mixed), 'prs', {}));
assert.equal(hintOf(mixedRows, 'Decide')[1], 'Where: o/api 1');
assert.equal(hintOf(mixedRows, 'Review')[1], 'Where: p/web 1');
assert.equal(hintOf(mixedRows, 'Needs a person')[1], 'Where: o/api 1, p/web 1');
`
	runOverviewCardNode(t, script)
}

// The where line and the shared-blocker lines on their own: owners are kept
// when the repos have more than one, one remaining repo reads "1 other
// repo", ties go by name, and nothing is shared below two items.
func TestOverviewCardBandHoverHintLines(t *testing.T) {
	html := indexHTML(t)
	script := overviewCardUIPrelude(t, html) + `
const at = repo => ({ repo });
assert.equal(overviewCardWhereLine([]), '');
assert.equal(overviewCardWhereLine(undefined), '');
assert.equal(overviewCardWhereLine([at('o/x')]), 'Where: x 1');
assert.equal(overviewCardWhereLine([at('a/x'), at('b/x'), at('a/x')]), 'Where: a/x 2, b/x 1');
// Exactly one repo past the top three is named; two or more are counted.
assert.equal(overviewCardWhereLine([at('o/d'), at('o/c'), at('o/b'), at('o/a'), at('o/a')]), 'Where: a 2, b 1, c 1, d 1');
assert.equal(overviewCardWhereLine([at('o/e'), at('o/d'), at('o/c'), at('o/b'), at('o/a'), at('o/a')]), 'Where: a 2, b 1, c 1, 2 other repos 2');
// The caller's owner test wins over the row's own.
assert.equal(overviewCardWhereLine([at('o/x')], false), 'Where: o/x 1');
assert.equal(overviewCardWhereLine([at('a/x'), at('b/y')], true), 'Where: x 1, y 1');
assert.equal(overviewCardOneOwner([at('o/x'), at('o/y')]), true);
assert.equal(overviewCardOneOwner([at('o/x'), at('p/y')]), false);
const pr = (checks, protection) => ({ item: { failing_checks: checks, protection } });
assert.deepEqual(overviewCardSharedLines([]), []);
assert.deepEqual(overviewCardSharedLines([pr(['build'])]), [], 'one item shares nothing');
assert.deepEqual(overviewCardSharedLines([pr(['build', 'build']), pr([])]), [], 'a check listed twice on one PR counts once');
assert.deepEqual(overviewCardSharedLines([pr(['zeta', 'alpha']), pr(['alpha', 'zeta'])]), ['2 fail the same check: alpha'], 'ties go by name');
assert.deepEqual(overviewCardSharedLines([pr([], { review_decision: 'REVIEW_REQUIRED', approvals_given: 1 }), pr([], { review_decision: 'APPROVED', approvals_given: 1 })]), [], 'an approved PR needs nothing');
assert.deepEqual(overviewCardSharedLines([pr([], { review_decision: 'REVIEW_REQUIRED', approvals_given: 1 }), pr([], { review_decision: 'review_required', approvals_given: 1 })]), ['2 have 1 approval; GitHub still requires a review']);
assert.deepEqual(overviewCardSharedLines([{ card_reason: 'fails smoke' }, { card_reason: 'fails smoke' }]), [], 'reason text is never read');
assert.deepEqual(overviewCardRowTip('Decide', 'The hive waits.', []), ['Decide: the hive waits.']);
`
	runOverviewCardNode(t, script)
}

// The hint helpers read structured facts only (no reason field), and their
// strings have no em or en dashes or "@".
func TestOverviewCardBandHoverHintsStatic(t *testing.T) {
	html := indexHTML(t)
	for _, name := range []string{"overviewCardOneOwner", "overviewCardWhereLine", "overviewCardSharedLines", "overviewCardRowTip"} {
		src := jsFunc(t, html, name)
		if strings.ContainsAny(src, "–—@") {
			t.Errorf("%s() contains an em or en dash or an @", name)
		}
		if strings.Contains(src, "reason") {
			t.Errorf("%s() reads a reason field; shared blockers come from structured facts only", name)
		}
	}
	legend := jsFunc(t, html, "overviewCardLegendHTML")
	if !strings.Contains(legend, "const tip = overviewCardRowTip(s.label, s.rule, s.items, oneOwner).map(esc).join('&#10;');") || !strings.Contains(legend, `title="${tip}"`) {
		t.Error("the legend rows do not build their hover text with overviewCardRowTip outside the template")
	}
	if !strings.Contains(legend, "const oneOwner = overviewCardOneOwner(") {
		t.Error("the legend does not make one owner test for the whole donut")
	}
}
