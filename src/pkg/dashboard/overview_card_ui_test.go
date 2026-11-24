package dashboard

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The Overview card UI (design-overview-display-only §5, PR B). The server
// stamps card_band and card_group on every row and sends overview_card: the
// band specs per donut in plot order, the six groups and the six term tiles
// with their breakdowns. These tests run the page's own functions under node
// against a fixture in that exact shape.

// overviewCardUIFixture is one repo in the §12 contract shape: issues 8 open
// (7 rows and 1 the scanner did not list), PRs 7 open, including a draft the
// hive sidelines (nonActionablePrs) and a sidelined row without a card band,
// which the card does not count. Your turn is a headed group split by what
// the person has to do: a sentinel flag on an issue and a PR (one band on
// both donuts), a card-only Triage issue the inbox does not list, and an
// issue to close; the other Your turn bands are at zero.
const overviewCardUIFixture = `
const cardRow = (number, band, group, extra) => Object.assign({ number, card_band: band, card_group: group, card_reason_code: 'test', updated_at: '2026-11-23T12:00:00Z', created_at: '2026-11-20T12:00:00Z' }, extra || {});
function cardRepoA() {
  return {
    name: 'a', full: 'o/a', issues: 8, prs: 7,
    actionableIssues: [cardRow(1, 'inspect', 'your-turn', { ownership: { your_turn: true, verb: 'INSPECT' } }), cardRow(2, 'triage', 'your-turn', { ownership: { your_turn: false } }), cardRow(6, 'working', 'actionable-now')],
    heldIssues: [cardRow(5, 'human-hold', 'waiting-elsewhere')],
    nonActionableIssues: [cardRow(3, 'to-close', 'your-turn', { ownership: { your_turn: true, verb: 'CLOSE' } }), cardRow(4, 'covered-by-pr', 'waiting-elsewhere', { band: 'waiting' }), cardRow(7, 'not-work', 'outside')],
    openPrs: [cardRow(10, 'inspect', 'your-turn', { ownership: { your_turn: true, verb: 'INSPECT' } }), cardRow(12, 'author-update', 'blocked'), cardRow(13, 'bot-rebase', 'blocked'), cardRow(14, 'will-merge', 'actionable-now'), cardRow(16, 'draft', 'outside', { draft: true })],
    heldPrs: [cardRow(11, 'human-hold', 'waiting-elsewhere')],
    nonActionablePrs: [cardRow(15, 'draft', 'outside', { draft: true }), { number: 17, bucket: 'filtered' }],
  };
}
const cardSpec = (key, label, group, count, extra) => Object.assign({ key, label, rule: label + ' rule.', group, count, keep: false, hide_zero: false }, extra || {});
const cardBrow = (key, label, issues, prs, extra) => Object.assign({ key, label, rule: label + ' rule.', count: issues + prs, issues, prs, keep: false }, extra || {});
function cardFixture() {
  const waitRule = 'Another issue or PR must finish first. Check that the blocker is still real and moving.';
  return {
    issues: [
      cardSpec('inspect', 'Inspect a sentinel flag', 'your-turn', 1, { hide_zero: true }), cardSpec('decide', 'Decide', 'your-turn', 0), cardSpec('review', 'Review', 'your-turn', 0),
      cardSpec('triage', 'Triage', 'your-turn', 1, { hide_zero: true }), cardSpec('admit', 'Admit', 'your-turn', 0, { hide_zero: true }), cardSpec('your-turn-other', 'Other reason', 'your-turn', 0, { hide_zero: true }),
      cardSpec('to-close', 'Yours to close', 'your-turn', 1), cardSpec('unexplained', 'No rule matched (hive bug)', 'your-turn', 0, { keep: true }),
      cardSpec('covered-by-pr', 'Covered by an open PR', 'waiting-elsewhere', 1), cardSpec('waiting-on-item', 'Waiting on another item', 'waiting-elsewhere', 0, { rule: waitRule }),
      cardSpec('human-hold', 'Human placed on hold', 'waiting-elsewhere', 1), cardSpec('reporter-confirm', 'Reporter to confirm the fix', 'waiting-elsewhere', 0),
      cardSpec('queued', 'Queued for agents', 'actionable-now', 0, { keep: true }), cardSpec('working', 'Agents are working it', 'actionable-now', 1),
      cardSpec('held', 'Held', 'held', 0, { hide_zero: true }), cardSpec('not-work', 'Not work', 'outside', 1),
    ],
    prs: [
      cardSpec('inspect', 'Inspect a sentinel flag', 'your-turn', 1, { hide_zero: true, rule: 'PR inspect rule.' }), cardSpec('decide', 'Decide', 'your-turn', 0),
      cardSpec('approve', 'Approve', 'your-turn', 0, { hide_zero: true }), cardSpec('merge-by-hand', 'Merge by hand', 'your-turn', 0, { hide_zero: true }), cardSpec('review', 'Review', 'your-turn', 0),
      cardSpec('triage', 'Triage', 'your-turn', 0, { hide_zero: true }), cardSpec('admit', 'Admit', 'your-turn', 0, { hide_zero: true }), cardSpec('your-turn-other', 'Other reason', 'your-turn', 0, { hide_zero: true }),
      cardSpec('to-close', 'Yours to close', 'your-turn', 0), cardSpec('unexplained', 'No rule matched (hive bug)', 'your-turn', 0, { keep: true }),
      cardSpec('human-hold', 'Human placed on hold', 'waiting-elsewhere', 1),
      cardSpec('author-update', 'Author to update the PR', 'blocked', 1), cardSpec('bot-rebase', 'Dependency bot to rebase', 'blocked', 1),
      cardSpec('working', 'Agents are working it', 'actionable-now', 0), cardSpec('will-merge', 'Will merge on its own', 'actionable-now', 1),
      cardSpec('held', 'Held', 'held', 0, { hide_zero: true }), cardSpec('draft', 'Draft', 'outside', 2),
    ],
    groups: [
      { key: 'your-turn', label: 'Needs a person', rule: 'No agent will move these until a person acts.', headed: true, issues: 3, prs: 1 },
      { key: 'waiting-elsewhere', label: 'Waiting on someone else', rule: 'Someone other than you has the next move. Nothing to do until they act.', headed: true, issues: 2, prs: 1 },
      { key: 'held', label: 'Held', rule: 'Paused by a hold no earlier rule explains.', headed: false, issues: 0, prs: 0 },
      { key: 'blocked', label: 'Blocked', rule: 'Someone else must push a fix: the author or a dependency bot.', headed: true, issues: 0, prs: 2 },
      { key: 'actionable-now', label: 'Hive has it', rule: 'The hive moves these on its own. Nothing for you to do.', headed: true, issues: 1, prs: 1 },
      { key: 'outside', label: 'Outside', rule: "Not work, drafts, and items this hive's settings leave alone.", headed: false, issues: 2, prs: 2 },
    ],
    tiles: [
      { key: 'your-turn', term: 'your_turn', label: 'Needs a person', rule: 'No agent will move these until a person acts.', count: 4, issues: 3, prs: 1, breakdown: [
        cardBrow('inspect', 'Inspect a sentinel flag', 1, 1), cardBrow('decide', 'Decide', 0, 0), cardBrow('approve', 'Approve', 0, 0), cardBrow('merge-by-hand', 'Merge by hand', 0, 0),
        cardBrow('review', 'Review', 0, 0), cardBrow('triage', 'Triage', 1, 0), cardBrow('admit', 'Admit', 0, 0), cardBrow('your-turn-other', 'Other reason', 0, 0),
        cardBrow('to-close', 'Yours to close', 1, 0), cardBrow('unexplained', 'No rule matched (hive bug)', 0, 0, { keep: true })] },
      { key: 'waiting-elsewhere', term: 'waiting_elsewhere', label: 'Waiting on someone else', rule: 'Someone other than you has the next move. Nothing to do until they act.', count: 3, issues: 2, prs: 1, breakdown: [cardBrow('covered-by-pr', 'Covered by an open PR', 1, 0), cardBrow('waiting-on-item', 'Waiting on another item', 0, 0), cardBrow('human-hold', 'Human placed on hold', 1, 1), cardBrow('reporter-confirm', 'Reporter to confirm the fix', 0, 0)] },
      { key: 'held', term: 'held', label: 'Held', rule: 'Paused by a hold no earlier rule explains.', count: 0, issues: 0, prs: 0, breakdown: [cardBrow('held', 'Held', 0, 0)] },
      { key: 'blocked', term: 'blocked', label: 'Blocked', rule: 'Someone else must push a fix: the author or a dependency bot.', count: 2, issues: 0, prs: 2, breakdown: [cardBrow('author-update', 'Author to update the PR', 0, 1), cardBrow('bot-rebase', 'Dependency bot to rebase', 0, 1)] },
      { key: 'actionable-now', term: 'actionable', label: 'Hive has it', rule: 'The hive moves these on its own. Nothing for you to do.', count: 2, issues: 1, prs: 1, breakdown: [cardBrow('queued', 'Queued for agents', 0, 0, { keep: true }), cardBrow('working', 'Agents are working it', 1, 0), cardBrow('will-merge', 'Will merge on its own', 0, 1)] },
      { key: 'outside', term: 'outside', label: 'Outside', rule: "Not work, drafts, and items this hive's settings leave alone.", count: 4, issues: 2, prs: 2, breakdown: [cardBrow('not-work', 'Not work', 1, 0), cardBrow('draft', 'Draft', 0, 2)] },
    ],
    your_turn_not_in_inbox: { issues: 1, prs: 0 },
    unexplained: { issues: 0, prs: 0 },
    remainder: { issues: 1, prs: 0 },
    admission: { present: true, at: '2026-11-23T17:23:00Z', admission_seq: 382, verdicts: 3, capped: false },
  };
}
`

// overviewCardUIPrelude is the KPI render harness plus the chart-slice,
// legend, export and navigation helpers the card uses, and the fixture.
func overviewCardUIPrelude(t *testing.T, html string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(overviewKPIRenderPrelude(t, html))
	b.WriteString(`
const OVERVIEW_CHART_PERCENT_SCALE = 100;
const OVERVIEW_CHART_DECIMAL_PLACES = 1;
const OVERVIEW_REPOS_KEY = 'hive.overview.repos';
const OVERVIEW_CHARTS_KEY = 'hive-overview-charts';
const OVERVIEW_EXPORT_FORMATS = ['csv', 'json'];
const OVERVIEW_BAR_MIN_PERCENT = 1.5;
let applied = '';
function overviewApplyKPIProjectFilter(key) { applied = key; }
`)
	for _, name := range []string{
		"issueUpdatedAt", "prUpdatedAt", "prCreatedAt", "prReviewClassRank",
		"overviewAllRepoNames", "overviewSavedRepoNames", "overviewSelectedRepoNames",
		"overviewCardMode", "overviewSliceSpecs", "overviewCardBandSlices", "overviewHistoryCounts", "overviewHBarRow", "renderOverviewBar", "overviewIssueBandSlices", "overviewPRBandSlices",
		"overviewSliceLabel", "overviewExportURL", "overviewCardRuleTip", "overviewCardOneOwner", "overviewCardWhereLine", "overviewCardSharedLines", "overviewCardRowTip", "overviewCardLegendRows", "overviewCardLegendHTML", "overviewLegendHTML",
		"overviewCardTileForKey", "overviewNavigateBand",
	} {
		b.WriteString(jsFunc(t, html, name))
		b.WriteByte('\n')
	}
	b.WriteString(overviewCardUIFixture)
	return b.String()
}

// runOverviewCardNode runs the script from a file: the KPI prelude alone is
// close to the per-argument limit of node -e.
func runOverviewCardNode(t *testing.T, script string) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: the Overview card UI was not executed")
	}
	path := filepath.Join(t.TempDir(), "overview-card.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("node Overview card UI failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	return string(out)
}

// The legend: Needs a person lists the Your turn bands as plain rows under a
// Needs a person subtotal; in Status, headed groups are bold subtotal rows
// with their bands indented under them. Group and subtotal rows never dim,
// zero bands dim except the keep bands, Held is hidden at zero, and every
// band, group and subtotal row downloads its own card_band= or card_group=
// export. No inline handlers (CSP).
func TestOverviewCardLegendGroupsAndDownloads(t *testing.T) {
	html := indexHTML(t)
	script := overviewCardUIPrelude(t, html) + `
window._lastStatus = { hiveId: 'h', overview_card: cardFixture() };
const repoA = cardRepoA();
_overviewLastRepos = [repoA];
const legendRows = out => [...out.matchAll(/<div class="([^"]*)"(?: data-action="overviewNavigateBand" data-arg0="([^"]*)" data-arg1="([^"]+)" data-stop="1")? title="([^"]*)"><span class="overview-chart-swatch ([^"]*)"(?: aria-hidden="true")?><\/span><span class="overview-chart-legend-label">([^<]*)(?:<span class="overview-chart-legend-sr">,<\/span> <span class="overview-chart-legend-note">([^<]*)<\/span>)?<\/span>(?:<span class="overview-chart-legend-sr">[^<]*<\/span>)?<span class="overview-chart-legend-count">([^<]*)<\/span>(?:<button[^>]*data-arg0="([^"]*)")?/g)]
  .map(m => ({ cls: m[1].split(' '), kind: m[2] || '', key: m[3] || '', title: m[4], swatch: m[5], label: m[6], note: m[7] || '', count: m[8], url: m[9] || '' }));
const issueSlices = overviewIssueBandSlices([repoA]);
assert.ok(issueSlices.every(s => s.card), 'the card bands replace the chart slices');
assert.deepEqual(issueSlices.map(s => [s.key, s.count]), [['inspect', 1], ['decide', 0], ['review', 0], ['triage', 1], ['admit', 0], ['your-turn-other', 0], ['to-close', 1], ['unexplained', 0], ['covered-by-pr', 1], ['waiting-on-item', 0], ['human-hold', 1], ['reporter-confirm', 0], ['queued', 0], ['working', 1], ['held', 0], ['not-work', 1]]);
const issueHTML = overviewLegendHTML(issueSlices, 'issues', { exportFormat: 'csv' });
const issues = legendRows(issueHTML);
assert.deepEqual(issues.map(r => r.label), ['Inspect a sentinel flag', 'Decide', 'Review', 'Triage', 'Yours to close', 'Needs a person', 'No rule matched (hive bug)', 'Waiting on someone else', 'Covered by an open PR', 'Waiting on another item', 'Human placed on hold', 'Reporter to confirm the fix', 'Hive has it', 'Queued for agents', 'Agents are working it', 'Not work', 'Status']);
const row = label => issues.find(r => r.label === label);
// headed groups: a bold subtotal row and indented parts
assert.ok(row('Waiting on someone else').cls.includes('overview-chart-legend-group'));
assert.match(row('Waiting on someone else').count, /^2 · /);
assert.equal(row('Waiting on someone else').key, 'waiting-elsewhere');
for (const label of ['Covered by an open PR', 'Waiting on another item', 'Human placed on hold', 'Reporter to confirm the fix', 'Queued for agents', 'Agents are working it']) assert.ok(row(label).cls.includes('overview-chart-legend-part'), label + ' is indented');
// Your turn is the Needs a person section: its what-to-do rows and Yours to
// close are plain rows in plot order, then the Needs a person subtotal in the
// group's colour, which opens the Your turn tile, then No rule matched (the
// unexplained band, a hive bug) with a "should be 0" note, a warning only
// above 0 (here it is 0).
// No Your turn group row.
assert.ok(!issues.some(r => r.label === 'Your turn'), 'the Your turn group row is gone');
assert.ok(row('Needs a person').cls.includes('overview-chart-legend-subtotal'));
assert.ok(!row('Needs a person').cls.includes('overview-chart-legend-group'));
assert.equal(row('Needs a person').key, 'your-turn');
assert.match(row('Needs a person').count, /^3 · /);
assert.equal(row('Needs a person').swatch, 'overview-card-group-your-turn');
for (const label of ['Inspect a sentinel flag', 'Decide', 'Review', 'Triage', 'Yours to close', 'No rule matched (hive bug)']) assert.ok(!row(label).cls.includes('overview-chart-legend-part'), label + ' is not indented in Needs a person');
assert.ok(!row('Not work').cls.includes('overview-chart-legend-part') && !row('Not work').cls.includes('overview-chart-legend-group'), 'Not work is a plain row');
// Your turn rows at zero: hide-zero ones are hidden, Decide and Review dim,
// No rule matched (a keep band) stays at full strength.
for (const key of ['admit', 'your-turn-other']) assert.ok(!issues.some(r => r.key === key), key + ' is hidden at zero');
for (const label of ['Decide', 'Review']) assert.ok(row(label).cls.includes('overview-chart-legend-zero'), label + ' dims at zero');
for (const label of ['Inspect a sentinel flag', 'Triage', 'Yours to close']) assert.ok(!row(label).cls.includes('overview-chart-legend-zero'), label);
// zero bands dim; keep bands and group rows never do
assert.ok(row('Waiting on another item').cls.includes('overview-chart-legend-zero'));
assert.ok(row('Reporter to confirm the fix').cls.includes('overview-chart-legend-zero'));
assert.ok(!row('Queued for agents').cls.includes('overview-chart-legend-zero'), 'Queued for agents is a keep band');
assert.ok(!row('No rule matched (hive bug)').cls.includes('overview-chart-legend-zero'), 'No rule matched is a keep band');
assert.ok(!row('No rule matched (hive bug)').cls.includes('overview-chart-legend-warning'), 'at 0 it is not marked as a warning');
assert.equal(row('No rule matched (hive bug)').note, 'should be 0', 'the note shows at 0 too');
assert.ok(!row('Covered by an open PR').cls.includes('overview-chart-legend-zero'));
const idle = cardRepoA();
idle.actionableIssues = idle.actionableIssues.filter(r => r.card_band !== 'working');
const idleRows = legendRows(overviewLegendHTML(overviewIssueBandSlices([idle]), 'issues', {}));
const idleHead = idleRows.find(r => r.label === 'Hive has it');
assert.match(idleHead.count, /^0 · /);
assert.ok(!idleHead.cls.includes('overview-chart-legend-zero'), 'a group row never dims, even at 0');
// Held is hidden at zero and listed once it has a row
assert.ok(!issues.some(r => r.key === 'held'));
const heldRepo = cardRepoA();
heldRepo.heldIssues.push(cardRow(8, 'held', 'held'));
assert.ok(legendRows(overviewLegendHTML(overviewIssueBandSlices([heldRepo]), 'issues', {})).some(r => r.key === 'held' && r.cls.length === 1));
// every band and group row downloads its own rows
for (const r of issues) {
  const url = new URL(r.url, 'https://hive.test');
  assert.equal(url.pathname, '/api/overview/issues.csv', r.label);
  if (r.label === 'Status') assert.equal(url.searchParams.get('card_group'), 'waiting-elsewhere,actionable-now,held,outside', r.label);
  else if (r.cls.includes('overview-chart-legend-group') || r.cls.includes('overview-chart-legend-subtotal')) assert.equal(url.searchParams.get('card_group'), r.key, r.label);
  else assert.equal(url.searchParams.get('card_band'), r.key, r.label);
}
// colours and plain-words tooltips
assert.equal(row('Covered by an open PR').swatch, 'overview-card-issue-covered-by-pr');
assert.equal(row('Waiting on someone else').swatch, 'overview-card-group-waiting-elsewhere');
assert.equal(row('Waiting on another item').title, 'Waiting on another item: another issue or PR must finish first. Check that the blocker is still real and moving.');
assert.equal(row('Waiting on someone else').title, 'Waiting on someone else: someone other than you has the next move. Nothing to do until they act.&#10;Where: a 2');
assert.doesNotMatch(issueHTML, /onclick=|[\u2013\u2014]/);

// The PR donut also plots the drafts the hive sidelines.
const prSlices = overviewPRBandSlices([repoA]);
assert.deepEqual(prSlices.find(s => s.key === 'draft').items.map(e => e.number).sort(), [15, 16]);
localStorage.setItem(OVERVIEW_CHARTS_KEY, JSON.stringify({ exportFormat: 'json' }));
const prHTML = overviewLegendHTML(prSlices, 'prs', { exportFormat: 'json' });
localStorage.removeItem(OVERVIEW_CHARTS_KEY);
const prs = legendRows(prHTML);
assert.deepEqual(prs.map(r => r.label), ['Inspect a sentinel flag', 'Decide', 'Review', 'Yours to close', 'Needs a person', 'No rule matched (hive bug)', 'Waiting on someone else', 'Human placed on hold', 'Blocked', 'Author to update the PR', 'Dependency bot to rebase', 'Hive has it', 'Agents are working it', 'Will merge on its own', 'Draft', 'Status']);
for (const key of ['approve', 'merge-by-hand', 'triage', 'admit', 'your-turn-other']) assert.ok(!prs.some(r => r.key === key), 'PR ' + key + ' is hidden at zero');
assert.match(prs.find(r => r.label === 'Needs a person').count, /^1 · /);
assert.equal(prs.indexOf(prs.find(r => r.label === 'No rule matched (hive bug)')), prs.indexOf(prs.find(r => r.label === 'Needs a person')) + 1, 'No rule matched follows the Needs a person subtotal');
assert.ok(prs.find(r => r.label === 'Blocked').cls.includes('overview-chart-legend-group'));
assert.equal(new URL(prs.find(r => r.label === 'Blocked').url, 'https://hive.test').searchParams.get('card_group'), 'blocked');
assert.equal(new URL(prs.find(r => r.label === 'Draft').url, 'https://hive.test').pathname, '/api/overview/prs.json');
assert.doesNotMatch(prHTML, /onclick=|[\u2013\u2014]/);
assert.equal(overviewExportURL('issues', 'queued', 'card_band'), '/api/overview/issues.csv?card_band=queued');
// The bar chart hides Held at zero, as the legend does, and joins label and
// rule with a colon; the line chart samples card bands under their own keys.
const bar = renderOverviewBar('Issues by band', '', issueSlices);
assert.doesNotMatch(bar, />Held</);
assert.match(bar, /title="Covered by an open PR \(1\): covered by an open PR rule\."/);
assert.doesNotMatch(bar, /[\u2013\u2014]/);
assert.deepEqual(Object.keys(overviewHistoryCounts(issueSlices)).slice(0, 2), ['card:inspect', 'card:decide']);
// A rule's first word is lower-cased only when it is an ordinary word.
assert.equal(overviewCardRuleTip('Covered', 'A PR is doing the work.'), 'Covered: a PR is doing the work.');
assert.equal(overviewCardRuleTip('Waiting', 'GitHub lists 2 open blockers.'), 'Waiting: GitHub lists 2 open blockers.');
assert.equal(overviewCardRuleTip('Draft', 'PR authors mark it ready.'), 'Draft: PR authors mark it ready.');
assert.equal(overviewExportURL('prs', 'blocked', 'card_group'), '/api/overview/prs.csv?card_group=blocked');
`
	runOverviewCardNode(t, script)
}

// The tiles: values, splits and sublines from overview_card.tiles; an ⓘ on
// every term tile listing its bands (zero bands hidden except keep, a band on
// both donuts as one combined row, Your turn's not-in-the-inbox sentence);
// the KPI aria label and the sidebar pill title from the card tiles; the
// card's own sparkline fields; and the same numbers counted locally when only
// some repos are selected.
func TestOverviewCardTilesInfoAndSublines(t *testing.T) {
	html := indexHTML(t)
	script := overviewCardUIPrelude(t, html) + `
const state = { showKPIs: true };
// Record what each sparkline is drawn from (this declaration replaces the
// prelude's stub for the whole script).
const sparkCalls = [];
function renderSparkline(_, values, opts) { sparkCalls.push([(opts && opts.id) || '', values.slice(), (opts && opts.color) || '']); return '<svg data-id="' + ((opts && opts.id) || '') + '"></svg>'; }
const repoA = cardRepoA();
const repoB = { name: 'b', full: 'o/b', issues: 1, prs: 0, actionableIssues: [cardRow(20, 'decide', 'your-turn', { ownership: { your_turn: true } })] };
_overviewLastRepos = [repoA];
window._lastStatus = { hiveId: 'h', repos: [repoA], overviewTotals: { issues: { forge: 8 }, prs: { forge: 7 } }, overview_card: cardFixture(),
  actionableNow: { equation: { terms: [{ key: 'outside', label: 'outside', count: 4, breakdown: [{ key: 'exempt-labels', label: 'exempt', count: 1, settingsTab: 'Labels', docsHref: '/docs/dashboard.md#outside-buckets' }] }] } } };
const tiles = out => Object.fromEntries([...out.matchAll(/data-overview-kpi-key="([^"]+)" data-overview-kpi-value="([^"]+)"[^>]*>[^<]*<\/span><span class="overview-kpi-label">([^<]*)/g)].map(m => [m[1], { value: Number(m[2]), label: m[3] }]));
const tip = (out, key) => { const m = out.match(new RegExp('<span id="overview-kpi-' + key + '-tip"[^>]*>(.*?)</span></span>')); return m ? m[1] : ''; };
const sublines = out => [...out.matchAll(/<span class="overview-kpi-subline">([^<]*)<\/span>/g)].map(m => m[1]);
let out = renderOverviewKPIs([repoA], [], [], state);
let kpi = tiles(out);
const values = Object.fromEntries(Object.entries(kpi).map(([k, v]) => [k, v.value]));
assert.deepEqual(values, { 'open-issues': 8, 'open-prs': 7, 'actionable-now': 2, 'your-turn': 4, 'waiting-elsewhere': 3, held: 0, blocked: 2, outside: 4 });
assert.equal(kpi['actionable-now'].label, 'Hive has it');
// The your-turn tile reads "Needs a person"; its key stays your-turn.
assert.equal(kpi['your-turn'].label, 'Needs a person');
assert.match(tip(out, 'your-turn'), /overview-partition-title">Needs a person · 4</);
assert.equal(values['open-issues'] + values['open-prs'], values['actionable-now'] + values['your-turn'] + values['waiting-elsewhere'] + values.held + values.blocked + values.outside, 'the six tiles sum to the totals');
// sublines: the totals in the mock-up's order, the splits in today's form
const subs = sublines(out);
assert.equal(subs[0], '3 needs a person · 2 waiting · 1 hive has it · 2 outside');
assert.equal(subs[1], '1 needs a person · 1 waiting · 2 blocked · 1 hive has it · 2 outside');
assert.equal(subs[2], '2 = 1 issues + 1 PRs');
assert.equal(subs[3], '4 = 3 issues + 1 PRs');
// an ⓘ on all six term tiles
for (const key of ['your-turn', 'waiting-elsewhere', 'held', 'blocked', 'actionable-now', 'outside']) assert.ok(tip(out, key), key + ' has an ⓘ');
assert.match(tip(out, 'waiting-elsewhere'), /overview-partition-title">Waiting on someone else · 3</);
assert.match(tip(out, 'waiting-elsewhere'), /overview-partition-summary">Someone other than you has the next move\. Nothing to do until they act\.</);
assert.match(tip(out, 'waiting-elsewhere'), /title="Human placed on hold rule\."><span class="overview-partition-label"><span class="overview-chart-swatch overview-card-issue-human-hold"><\/span>Human placed on hold \(1 issue, 1 PR\)<\/span><span class="overview-partition-count">2</);
assert.match(tip(out, 'waiting-elsewhere'), /Covered by an open PR<\/span><span class="overview-partition-count">1</);
assert.doesNotMatch(tip(out, 'waiting-elsewhere'), /Waiting on another item|Reporter to confirm/, 'zero bands are left out');
assert.match(tip(out, 'actionable-now'), /<div class="overview-partition-row" title="Queued for agents rule\."><span class="overview-partition-label"><span class="overview-chart-swatch overview-card-issue-queued"><\/span>Queued for agents<\/span><span class="overview-partition-count">0</, 'keep bands stay at zero, not dimmed, with their swatch');
assert.match(tip(out, 'your-turn'), /title="Issues: Inspect a sentinel flag rule\. PRs: PR inspect rule\."><span class="overview-partition-label"><span class="overview-chart-swatch overview-card-issue-inspect"><\/span>Inspect a sentinel flag \(1 issue, 1 PR\)/, 'a combined row names both rules');
assert.match(tip(out, 'your-turn'), /Triage \(1 issue, 0 PRs\)<\/span><span class="overview-partition-count">1</);
assert.doesNotMatch(tip(out, 'your-turn'), />Decide<|>Review<|>Approve</, 'zero bands are left out');
assert.doesNotMatch(tip(out, 'your-turn'), /No rule matched/, 'No rule matched is listed only above zero');
assert.equal(sparkCalls.filter(c => c[0] === 'overview-kpi:actionable-now').pop()[2], 'var(--band-unclaimed)', 'Hive has it is the mock-up blue');
assert.match(tip(out, 'your-turn'), /Includes 1 the hive holds for a person that is not in the inbox\./);
assert.match(tip(out, 'outside'), /Open but not listed \(1 issue, 0 PRs\)<\/span><span class="overview-partition-count">1</);
for (const key of ['your-turn', 'waiting-elsewhere', 'held', 'blocked', 'actionable-now', 'outside']) assert.doesNotMatch(tip(out, key), /[\u2013\u2014]|fixed buckets/, key);
// Outside keeps the server equation's Settings and Learn more links.
assert.match(tip(out, 'outside'), /Settings → Labels/);
assert.match(tip(out, 'outside'), /Learn more ↗/);
// the KPI strip's aria label and the sidebar pill title use the card tiles
const label = '8 total open issues + 7 total open PRs = 2 hive has it + 4 needs a person + 3 waiting on someone else + 0 held + 2 blocked + 4 outside';
assert.ok(out.includes('<div class="overview-kpis" aria-label="' + label + '">'));
assert.ok(document.getElementById('overview-sidebar-badge').title.endsWith(' · ' + label));
// the card tiles sample their own history fields, never today's
let stored = JSON.parse(localStorage.getItem(overviewKPILocalKey()) || '[]');
const last = stored[stored.length - 1];
assert.deepEqual([last.overviewCardYourTurn, last.overviewCardWaiting, last.overviewCardHeld, last.overviewCardBlocked, last.overviewCardActionable, last.overviewCardOutside], [4, 3, 0, 2, 2, 4]);
for (const old of ['overviewYourTurn', 'overviewWaiting', 'overviewHeld', 'overviewBlocked', 'overviewActionable', 'overviewOutside']) assert.ok(!(old in last), old + ' is not written with the card');
assert.equal(last.overviewOpenIssues, 8);
const sparkValues = key => (sparkCalls.filter(c => c[0] === 'overview-kpi:' + key).pop() || [null, []])[1];
localStorage.data = {};
_overviewKPIHistory = [{ t: Date.now() - 600000, overviewYourTurn: 9 }];
renderOverviewKPIs([repoA], [], [], state);
assert.deepEqual(sparkValues('your-turn'), [4], 'a sample of today\'s equation never draws into a card sparkline');
_overviewKPIHistory = [{ t: Date.now() - 600000, overviewCardYourTurn: 3 }];
renderOverviewKPIs([repoA], [], [], state);
assert.deepEqual(sparkValues('your-turn'), [3, 4]);
_overviewKPIHistory = [];

// No rule matched is listed in Your turn with its count.
const withBug = cardFixture();
withBug.tiles[0].count = 5; withBug.tiles[0].issues = 4;
withBug.tiles[0].breakdown[withBug.tiles[0].breakdown.findIndex(r => r.key === 'unexplained')] = cardBrow('unexplained', 'No rule matched (hive bug)', 1, 0, { keep: true });
window._lastStatus.overview_card = withBug;
out = renderOverviewKPIs([repoA], [], [], state);
assert.match(tip(out, 'your-turn'), /No rule matched \(hive bug\) \(1 issue, 0 PRs\)<\/span><span class="overview-partition-count">1</);
window._lastStatus.overview_card = cardFixture();

// Some repos selected: the same tiles, counted from card_group, card_band and
// each repo's remainder.
_overviewLastRepos = [repoA, repoB];
out = renderOverviewKPIs([repoA], [], [], state);
kpi = tiles(out);
assert.deepEqual(Object.fromEntries(Object.entries(kpi).map(([k, v]) => [k, v.value])), values);
assert.deepEqual(sublines(out).slice(0, 2), subs.slice(0, 2));
assert.match(tip(out, 'your-turn'), /Includes 1 the hive holds for a person that is not in the inbox\./);
assert.match(tip(out, 'waiting-elsewhere'), /Human placed on hold \(1 issue, 1 PR\)<\/span><span class="overview-partition-count">2</);
assert.match(tip(out, 'outside'), /Open but not listed \(1 issue, 0 PRs\)<\/span><span class="overview-partition-count">1</);
// Counted in the browser, the Your turn rows keep the server's order: a
// PR-only band (Approve) is not appended after Yours to close.
const repoC = Object.assign(cardRepoA(), { name: 'c', full: 'o/c' });
repoC.openPrs.push(cardRow(30, 'approve', 'your-turn', { ownership: { your_turn: true } }));
_overviewLastRepos = [repoC, repoB];
out = renderOverviewKPIs([repoC], [], [], state);
const ytTip = tip(out, 'your-turn');
const at = label => ytTip.indexOf('</span>' + label);
assert.ok(at('Approve') > 0, 'Approve is listed');
assert.ok(at('Approve') < at('Triage') && at('Triage') < at('Yours to close'), 'server order: ' + ytTip);
_overviewLastRepos = [repoA];
`
	runOverviewCardNode(t, script)
}

// Navigation and Projects filters: a band row opens its group's tile, a group
// row its own; each tile's Projects filter counts exactly the rows its tile
// counts, matched on card_group; and with no card (an older server) the
// chart slices, the old map and today's sparkline fields come back.
func TestOverviewCardNavigationFiltersAndFallback(t *testing.T) {
	html := indexHTML(t)
	script := overviewCardUIPrelude(t, html) + `
const card = cardFixture();
window._lastStatus = { hiveId: 'h', overview_card: card };
const repoA = cardRepoA();
_overviewLastRepos = [repoA];
for (const spec of card.issues.concat(card.prs)) {
  overviewNavigateBand('issues', spec.key);
  assert.equal(applied, spec.group, spec.key);
}
for (const group of card.groups) {
  overviewNavigateBand('prs', group.key);
  assert.equal(applied, group.key, group.key);
}
for (const tile of card.tiles) assert.equal(overviewProjectFilterSummary(tile.key, [repoA]).count, tile.count, tile.key);
const matched = key => {
  const r = overviewProjectFilterRepoMatches(repoA, Object.assign({ key }, OVERVIEW_PROJECT_FILTERS[key]));
  return r.issues.concat(r.prs).map(e => e.item.number).sort((a, b) => a - b);
};
assert.deepEqual(matched('your-turn'), [1, 2, 10]);
assert.deepEqual(matched('waiting-elsewhere'), [4, 5, 11]);
assert.deepEqual(matched('blocked'), [12, 13]);
assert.deepEqual(matched('outside'), [16]);
const outside = overviewProjectFilterRepoMatches(repoA, Object.assign({ key: 'outside' }, OVERVIEW_PROJECT_FILTERS.outside));
assert.deepEqual(outside.buckets.map(b => [b.kind, b.count]), [['issue', 1], ['pr', 1], ['issue', 1]], 'Not work and the sidelined draft are counted, then the remainder');

// No card: the chart slices, today's map and today's history fields.
delete window._lastStatus.overview_card;
window._lastStatus.overview_slices = { issues: [{ key: 'your-turn', label: 'Your turn', rule: 'r' }, { key: 'ready', label: 'Ready', rule: 'r' }], prs: [{ key: 'eligible', label: 'Eligible', rule: 'r' }] };
const old = cardRepoA();
old.actionableIssues.forEach((r, i) => { r.chart_slice = i ? 'ready' : 'your-turn'; });
old.openPrs.forEach(r => { r.chart_slice = 'eligible'; });
const issueSlices = overviewIssueBandSlices([old]);
assert.deepEqual(issueSlices.map(s => [s.key, s.count, !!s.card]), [['your-turn', 1, false], ['ready', 2, false]]);
assert.deepEqual(overviewPRBandSlices([old]).map(s => [s.key, s.count]), [['eligible', 5]]);
const legend = overviewLegendHTML(issueSlices, 'issues', {});
assert.match(legend, /issues\.csv\?slice=your-turn/);
assert.doesNotMatch(legend, /overview-chart-legend-group|card_band/);
overviewNavigateBand('issues', 'ready');
assert.equal(applied, 'actionable-now');
overviewNavigateBand('issues', 'draft');
assert.equal(applied, 'outside');
localStorage.data = {};
const rows = { overview_term: 'your_turn' };
const fallbackRepo = { name: 'a', full: 'o/a', issues: 2, prs: 0, actionableIssues: [rows] };
_overviewLastRepos = [fallbackRepo];
renderOverviewKPIs([fallbackRepo], [], [], { showKPIs: true });
const sample = JSON.parse(localStorage.getItem(overviewKPILocalKey()) || '[]').pop();
assert.equal(sample.overviewYourTurn, 1);
assert.ok(!('overviewCardYourTurn' in sample), 'without the card the tiles keep today\'s fields');
`
	runOverviewCardNode(t, script)
}

// overviewCardFallbackScript renders everything the card changes, for a hive
// whose status carries no overview_card (an older server): the KPI strip, the
// sidebar pill title, both legends, the bar chart, every Projects filter and
// the legend navigation. It prints them as JSON.
const overviewCardFallbackScript = `
const fallbackRow = (number, term, slice, extra) => Object.assign({ number, overview_term: term, chart_slice: slice, updated_at: '2026-11-23T12:00:00Z', created_at: '2026-11-20T12:00:00Z' }, extra || {});
const repo = {
  name: 'a', full: 'o/a', issues: 9, prs: 5,
  actionableIssues: [fallbackRow(1, 'actionable', 'ready'), fallbackRow(2, 'your_turn', 'your-turn', { band: 'ready' })],
  heldIssues: [fallbackRow(3, 'held', 'ready')],
  nonActionableIssues: [fallbackRow(4, 'waiting_elsewhere', 'waiting-elsewhere', { band: 'waiting' }), fallbackRow(5, 'your_turn', 'to-close'), fallbackRow(6, 'outside', '')],
  openPrs: [fallbackRow(10, 'actionable', 'eligible'), fallbackRow(11, 'blocked', 'blocked'), fallbackRow(12, 'outside', 'draft')],
  heldPrs: [fallbackRow(13, 'held', 'eligible')],
};
const term = (key, label, count, extra) => Object.assign({ key, label, count }, extra || {});
const outside = term('outside', 'outside', 4, { rule: 'Kept off the queue.', breakdown: [{ key: 'exempt-labels', label: 'exempt', count: 1, rule: 'An exempt label.', settingsTab: 'Labels', docsHref: '/docs/dashboard.md#outside-buckets' }, { key: 'dependency-dashboard', label: 'dependency dashboard', count: 3, rule: 'A bot dashboard.' }] });
const issueEq = { kind: 'issues', open: 9, result: 1, terms: [term('your_turn', 'Your turn', 2, { rule: 'Stuck on a person.', breakdown: [{ key: 'parked', label: 'parked', count: 2, rule: 'Parked by a label.' }] }), term('waiting_elsewhere', 'Waiting on someone else', 1), term('held', 'held', 1), outside], text: '9 open', title: 'Actionable now: work that can move.' };
const prEq = { kind: 'PRs', open: 5, result: 1, terms: [term('held', 'held', 1), term('blocked', 'blocked', 1), term('outside', 'outside', 2)], text: '5 open' };
window._lastStatus = {
  hiveId: 'h', repos: [repo],
  overviewTotals: { issues: { forge: 9 }, prs: { forge: 5 } },
  overview_slices: { issues: [{ key: 'your-turn', label: 'Your turn', rule: 'Stuck on a person.' }, { key: 'to-close', label: 'To close', rule: 'Close it.' }, { key: 'waiting-elsewhere', label: 'Waiting on someone else', rule: 'Someone else moves.' }, { key: 'ready', label: 'Unclaimed', rule: 'Nobody has it.' }], prs: [{ key: 'eligible', label: 'Merge-eligible', rule: 'Will merge.' }, { key: 'blocked', label: 'Blocked', rule: 'Red.' }, { key: 'draft', label: 'Draft', rule: 'Not ready.' }] },
  actionableNow: { total: 2, issues: 1, prs: 1, equation: { openIssues: 9, openPrs: 5, totalOpen: 14, result: 2, terms: [term('your_turn', 'Your turn', 2), term('waiting_elsewhere', 'Waiting on someone else', 1), term('held', 'held', 2), term('blocked', 'blocked', 1), outside, term('actionable', 'actionable', 2)], text: 'equation', title: 'Actionable now: work that can move.' }, issueEquation: issueEq, prEquation: prEq },
};
_overviewLastRepos = [repo];
const keys = ['open-issues', 'open-prs', 'actionable-now', 'your-turn', 'waiting-elsewhere', 'held', 'blocked', 'outside'];
const kpis = renderOverviewKPIs([repo], [], [], { showKPIs: true });
const issueSlices = overviewIssueBandSlices([repo]);
const prSlices = overviewPRBandSlices([repo]);
const navigation = {};
for (const key of ['your-turn', 'to-close', 'waiting-elsewhere', 'ready', 'eligible', 'blocked', 'draft', 'held', 'not-work']) { overviewNavigateBand('issues', key); navigation[key] = applied; }
const sample = Object.assign({}, JSON.parse(localStorage.getItem(overviewKPILocalKey()) || '[]').pop());
delete sample.t;
process.stdout.write(JSON.stringify({
  kpis,
  pill: document.getElementById('overview-sidebar-badge').title,
  issueSlices: issueSlices.map(s => [s.key, s.label, s.count, s.className]),
  prSlices: prSlices.map(s => [s.key, s.label, s.count, s.className]),
  issueLegend: overviewLegendHTML(issueSlices, 'issues', {}),
  prLegend: overviewLegendHTML(prSlices, 'prs', {}),
  bar: renderOverviewBar('Issues by band', '', issueSlices),
  filters: keys.map(key => { const f = overviewProjectFilterRepoMatches(repo, Object.assign({ key }, overviewProjectFilterSpec(key))); return [key, overviewProjectFilterSpec(key).label, overviewProjectFilterSummary(key, [repo]).count, f.buckets]; }),
  navigation,
  sample,
}, null, 1));
`

// Without the card (an older server) the Overview is exactly today's: the
// golden file was rendered by origin/v6's own functions (before the card) on
// the same fixture, and the card code must not change a byte of it.
func TestOverviewCardFallbackIsTodaysCard(t *testing.T) {
	html := indexHTML(t)
	got := runOverviewCardNode(t, overviewCardUIPrelude(t, html)+overviewCardFallbackScript)
	want, err := os.ReadFile(filepath.Join("testdata", "overview_card_fallback_golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) != strings.TrimSpace(string(want)) {
		t.Fatalf("the no-card Overview differs from today's.\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// overviewCardServedScript checks the page against a status the server built
// (window._lastStatus is set before it runs): the page draws the card, every
// stamped row lands on its band exactly once, the bands, groups and tiles
// equal the server's, and the same tiles come out when they are counted in
// the browser (some repos selected).
const overviewCardServedScript = `
const served = window._lastStatus;
const card = served.overview_card;
const repos = served.repos;
assert.ok(overviewCardMode(), 'a served status with overview_card draws the card');
_overviewLastRepos = repos;
// Every stamped row is on its band once (an item listed twice counts once,
// as the server counts it), and each donut band counts what the server says.
const stamped = new Set();
for (const r of repos) {
  for (const [kind, lists] of [['issue', ['actionableIssues', 'heldIssues', 'nonActionableIssues']], ['pr', ['openPrs', 'heldPrs', 'nonActionablePrs']]]) {
    for (const list of lists) for (const row of (r[list] || [])) if (row && row.card_band) stamped.add(kind + '#' + row.number);
  }
}
const plotted = [];
const issueSlices = overviewIssueBandSlices(repos);
const prSlices = overviewPRBandSlices(repos);
for (const [kind, slices, specs] of [['issue', issueSlices, card.issues], ['pr', prSlices, card.prs]]) {
  assert.deepEqual(slices.map(s => [s.key, s.count, s.group, s.keep, s.hideZero]), specs.map(s => [s.key, s.count, s.group, s.keep, s.hide_zero]), kind + ' bands');
  for (const slice of slices) for (const entry of slice.items) {
    assert.equal(entry.info.card_band, slice.key);
    plotted.push(kind + '#' + entry.number);
  }
}
assert.equal(new Set(plotted).size, plotted.length, 'no item is plotted twice');
assert.deepEqual([...plotted].sort(), [...stamped].sort(), 'every stamped row is on a donut');
// The legend's group rows and the Needs a person subtotal (the Your turn
// group) are the server's groups.
const groupRows = html => Object.fromEntries([...html.matchAll(/<div class="overview-chart-legend-row overview-chart-legend-(?:group|subtotal)" data-action="overviewNavigateBand" data-arg0="[^"]*" data-arg1="([^"]+)"[^>]*>.*?<span class="overview-chart-legend-count">(\d+) /g)].map(m => [m[1], Number(m[2])]));
const headed = card.groups.filter(g => g.headed);
const issueGroups = groupRows(overviewLegendHTML(issueSlices, 'issues', {}));
const prGroups = groupRows(overviewLegendHTML(prSlices, 'prs', {}));
for (const g of headed) {
  if (card.issues.some(s => s.group === g.key)) assert.equal(issueGroups[g.key], g.issues, 'issue group ' + g.key);
  if (card.prs.some(s => s.group === g.key)) assert.equal(prGroups[g.key], g.prs, 'PR group ' + g.key);
}
// The tiles are the server's, sum to the totals, and come out the same when
// the browser counts them for a subset of repos.
const tiles = out => Object.fromEntries([...out.matchAll(/data-overview-kpi-key="([^"]+)" data-overview-kpi-value="([^"]+)"/g)].map(m => [m[1], Number(m[2])]));
const serverTiles = Object.fromEntries(card.tiles.map(t => [t.key, t.count]));
let out = renderOverviewKPIs(repos, issueSlices, prSlices, { showKPIs: true });
let got = tiles(out);
for (const [key, count] of Object.entries(serverTiles)) assert.equal(got[key], count, 'tile ' + key);
assert.equal(got['open-issues'] + got['open-prs'], Object.values(serverTiles).reduce((n, c) => n + c, 0), 'the six tiles sum to the totals');
const hidden = card.your_turn_not_in_inbox.issues + card.your_turn_not_in_inbox.prs;
if (hidden > 0) assert.ok(out.includes('Includes ' + hidden + ' the hive holds for a person'), 'the Your turn note');
_overviewLastRepos = repos.concat([{ name: 'other', full: 'octo/other' }]);
out = renderOverviewKPIs(repos, issueSlices, prSlices, { showKPIs: true });
got = tiles(out);
for (const [key, count] of Object.entries(serverTiles)) assert.equal(got[key], count, 'tile ' + key + ' counted in the browser');
if (hidden > 0) assert.ok(out.includes('Includes ' + hidden + ' the hive holds for a person'), 'the Your turn note counted in the browser');
// Each tile's Projects filter counts what its tile counts.
for (const tile of card.tiles) assert.equal(overviewProjectFilterSummary(tile.key, repos).count, tile.count, 'filter ' + tile.key);
process.stdout.write('ok');
`

// The page against a status PR A's own code built (statusWithOverviewBands
// over cardPartitionStatus, which has a row in every band, items listed
// twice, a remainder and Unexplained): this catches any drift in field
// names or shapes between the server's card and the page that draws it.
func TestOverviewCardRendersTheServedStatus(t *testing.T) {
	served := cardPartitionServer().statusWithOverviewBands(cardPartitionStatus(), cardTestNow)
	if served.OverviewCard == nil {
		t.Fatal("the served status carries no overview_card")
	}
	payload, err := json.Marshal(served)
	if err != nil {
		t.Fatal(err)
	}
	html := indexHTML(t)
	script := overviewCardUIPrelude(t, html) + "\nwindow._lastStatus = " + string(payload) + ";\n" + overviewCardServedScript
	if got := runOverviewCardNode(t, script); strings.TrimSpace(got) != "ok" {
		t.Fatalf("unexpected node output: %s", got)
	}
}

// The new card code is plain words with no em or en dashes, wires every
// action through data-action (no inline handlers under the CSP), and ships
// the legend and palette classes.
func TestOverviewCardUIStatic(t *testing.T) {
	html := indexHTML(t)
	for _, name := range []string{
		"overviewCard", "overviewCardSpecs", "overviewCardGroups", "overviewCardTiles", "overviewCardRows", "overviewCardBandSlices",
		"overviewCardRepoRemainder", "overviewCardFilterBuckets", "overviewCardRuleTip", "overviewCardLegendRows", "overviewCardLegendHTML",
		"overviewCardTileDefs", "overviewCardKPI", "overviewCardKinds", "overviewCardTermInfo", "overviewCardApplyTiles", "overviewCardTileForKey", "overviewCardMode",
	} {
		src := jsFunc(t, html, name)
		if strings.ContainsAny(src, "\u2013\u2014") {
			t.Errorf("%s() contains an em or en dash", name)
		}
		if strings.Contains(src, "onclick") {
			t.Errorf("%s() uses an inline handler", name)
		}
	}
	for _, want := range []string{
		".overview-chart-legend-group {",
		".overview-chart-legend-part {",
		".overview-chart-legend-zero { opacity: 0.55; }",
		".overview-card-issue-covered-by-pr, .overview-card-pr-covered-by-pr {",
		".overview-card-issue-unexplained, .overview-card-pr-unexplained {",
		".overview-card-group-actionable-now {",
		"overviewCardLegendHTML(slices, '', state)",
		// Without the card the actionable tile and its filter keep today's name.
		"'actionable-now': { label: 'Actionable now', kinds: ['issue', 'pr'], term: 'actionable' }",
		"{ key: 'actionable-now', field: 'overviewActionable', label: 'Actionable now',",
		"' · hover a slice for its rule'",
		"' · hover a band for its rule'",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}
