package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
)

// overviewCardTodoStatusParse splits a card legend into its items (every
// top-level element of the legend is a div; rows hold only spans and a
// button) and reads each one: a section heading or a row with its classes,
// key, label, count, swatch and export link.
const overviewCardTodoStatusParse = `
const legendItems = html => html.split(/(?=<div class=")/).filter(Boolean).map(chunk => {
  const cls = (chunk.match(/^<div class="([^"]*)"/) || [])[1].split(' ');
  if (cls.includes('overview-chart-legend-section')) {
    const m = chunk.match(/^<div class="overview-chart-legend-section" role="heading" aria-level="4">([^<]*)(?:<span class="overview-chart-legend-sr">,<\/span> <span class="overview-chart-legend-section-note">([^<]*)<\/span>)?<\/div>$/);
    assert.ok(m, 'a section heading is role=heading level 4: ' + chunk);
    return { section: true, label: m[1].trim(), note: m[2] || '' };
  }
  const named = chunk.match(/<span class="overview-chart-legend-label">([^<]*)(?:<span class="overview-chart-legend-sr">,<\/span> <span class="overview-chart-legend-note">([^<]*)<\/span>)?<\/span>/) || [];
  return {
    section: false, cls, label: named[1], note: named[2] || '', chunk,
    key: (chunk.match(/^<div [^>]*data-arg1="([^"]*)"/) || [])[1] || '',
    action: /^<div [^>]*data-action="overviewNavigateBand"/.test(chunk),
    count: (chunk.match(/<span class="overview-chart-legend-count">([^<]*)<\/span>/) || [])[1],
    swatch: (chunk.match(/<span class="overview-chart-swatch ([^"]*)"/) || [])[1],
    spoken: (chunk.match(/<span class="overview-chart-legend-sr">([^<]*)<\/span>/) || [])[1] || '',
    url: (chunk.match(/<button[^>]*data-arg0="([^"]*)"/) || [])[1] || '',
  };
});
const sectionOf = (items, label) => {
  const at = items.findIndex(i => i.section && i.label === label);
  assert.ok(at >= 0, label + ' heading');
  const next = items.findIndex((i, n) => n > at && i.section);
  return items.slice(at + 1, next < 0 ? items.length : next);
};
const sumOf = list => list.reduce((n, s) => n + Number(s.count || 0), 0);
const pct = (n, total) => (n / total * 100).toFixed(1) + '%';
`

// The card legend splits into Needs a person and Status: a Needs a person
// heading (the Your turn group's label), the group's bands as plain rows in
// plot order (no group row), then a Needs a person subtotal equal to their
// sum, then the unexplained band (No rule matched, a hive bug), which the
// subtotal counts, with its "should be 0" note (a warning only above 0); a Status heading with its note, every other group exactly as before,
// then a Status subtotal. Both subtotals use the legend's own rounding, read
// "<label> total" to a screen reader, and never dim; the Status subtotal
// opens nothing and exports every other group.
func TestOverviewCardLegendToDoAndStatus(t *testing.T) {
	html := indexHTML(t)
	script := overviewCardUIPrelude(t, html) + overviewCardTodoStatusParse + `
const card = cardFixture();
window._lastStatus = { hiveId: 'h', overview_card: card };
const repoA = cardRepoA();
_overviewLastRepos = [repoA];
const visible = s => s.count > 0 || !s.hideZero;
for (const [kind, slices] of [['issues', overviewIssueBandSlices([repoA])], ['prs', overviewPRBandSlices([repoA])]]) {
  const total = sumOf(slices);
  const legend = overviewLegendHTML(slices, kind, { exportFormat: 'csv' });
  const items = legendItems(legend);
  // The two headings, in order, Needs a person first.
  assert.deepEqual(items.filter(i => i.section).map(i => [i.label, i.note]), [['Needs a person', ''], ['Status', 'nothing for a person to do']], kind);
  assert.ok(items[0].section && items[0].label === 'Needs a person', kind + ': the legend opens with Needs a person');
  // Needs a person: exactly the Your turn bands (hide-at-zero ones hidden),
  // plain rows in plot order, then the subtotal, then the unexplained
  // warning row.
  const todo = sectionOf(items, 'Needs a person');
  const yourTurn = slices.filter(s => s.group === 'your-turn');
  const at = todo.findIndex(r => r.cls.includes('overview-chart-legend-subtotal'));
  const todoRows = todo.slice(0, at);
  const todoTotal = todo[at];
  const afterTotal = todo.slice(at + 1);
  assert.deepEqual(todoRows.map(r => r.key), yourTurn.filter(visible).filter(s => s.key !== 'unexplained').map(s => s.key), kind + ' Needs a person rows');
  for (const r of todoRows) assert.ok(!r.cls.includes('overview-chart-legend-part') && !r.cls.includes('overview-chart-legend-group') && !r.cls.includes('overview-chart-legend-warning'), kind + ': ' + r.label + ' is a plain row');
  assert.deepEqual(afterTotal.map(r => r.key), ['unexplained'], kind + ': the unexplained row follows the subtotal');
  assert.ok(!afterTotal[0].cls.includes('overview-chart-legend-warning') && !afterTotal[0].cls.includes('overview-chart-legend-zero'), kind + ': at 0 it is not a warning and never dims');
  assert.equal(afterTotal[0].note, 'should be 0', kind);
  assert.equal(todoTotal.label, 'Needs a person');
  assert.ok(todoTotal.cls.includes('overview-chart-legend-subtotal') && !todoTotal.cls.includes('overview-chart-legend-zero'), kind + ' Needs a person subtotal');
  assert.equal(todoTotal.swatch, 'overview-card-group-your-turn', kind + ': the Your turn dot');
  assert.equal(todoTotal.spoken, ' total');
  assert.ok(todoTotal.action && todoTotal.key === 'your-turn', kind + ': Needs a person opens the Your turn tile');
  const todoCount = sumOf(yourTurn);
  assert.equal(todoCount, sumOf(slices.filter(s => s.group === 'your-turn' && visible(s))), 'hidden rows are zero');
  assert.equal(todoTotal.count, todoCount + ' · ' + pct(todoCount, total), kind + ' Needs a person subtotal is the sum');
  assert.equal(new URL(todoTotal.url, 'https://hive.test').searchParams.get('card_group'), 'your-turn', kind + ': Needs a person exports the Your turn group');
  // The old Your turn group row is gone from the legend.
  assert.ok(!items.some(i => i.label === 'Your turn'), kind + ': no Your turn row');
  assert.ok(!items.some(i => !i.section && i.cls.includes('overview-chart-legend-group') && i.key === 'your-turn'), kind);
  // Status: every other group, as before, then the subtotal.
  const status = sectionOf(items, 'Status');
  const statusRows = status.slice(0, -1);
  const statusTotal = status[status.length - 1];
  const others = slices.filter(s => s.group !== 'your-turn');
  const bandKeys = statusRows.filter(r => !r.cls.includes('overview-chart-legend-group')).map(r => r.key);
  assert.deepEqual(bandKeys, others.filter(visible).map(s => s.key), kind + ' Status bands in plot order');
  for (const g of card.groups.filter(g => g.key !== 'your-turn' && g.headed && others.some(s => s.group === g.key))) {
    const head = statusRows.find(r => r.cls.includes('overview-chart-legend-group') && r.key === g.key);
    assert.ok(head, kind + ': ' + g.key + ' group row in Status');
    const parts = others.filter(s => s.group === g.key);
    assert.equal(head.count, sumOf(parts) + ' · ' + pct(sumOf(parts), total), kind + ' ' + g.key);
    for (const p of parts.filter(visible)) assert.ok(statusRows.find(r => r.key === p.key).cls.includes('overview-chart-legend-part'), kind + ': ' + p.key + ' indented');
  }
  assert.equal(statusTotal.label, 'Status');
  assert.ok(statusTotal.cls.includes('overview-chart-legend-subtotal') && statusTotal.cls.includes('overview-chart-legend-static'), kind + ' Status subtotal');
  assert.ok(!statusTotal.action, kind + ': no tile counts the Status rows, so the subtotal opens nothing');
  assert.match(statusTotal.chunk, /<span class="overview-chart-swatch overview-chart-swatch-none" aria-hidden="true"><\/span>/, kind + ': no dot');
  assert.equal(statusTotal.spoken, ' total');
  const statusCount = sumOf(others);
  assert.equal(statusTotal.count, statusCount + ' · ' + pct(statusCount, total), kind + ' Status subtotal is the sum');
  assert.equal(todoCount + statusCount, total, kind + ': the two sections cover the donut');
  const groupsOut = [...new Set(others.map(s => s.group))];
  assert.deepEqual(new URL(statusTotal.url, 'https://hive.test').searchParams.get('card_group').split(','), groupsOut, kind + ': Status exports every other group');
  assert.ok(!groupsOut.includes('your-turn'));
  assert.doesNotMatch(legend, /onclick=|[–—]|@/);
}
// The fixture's numbers: issues 3 to do of 7, PRs 1 of 7.
const issueItems = legendItems(overviewLegendHTML(overviewIssueBandSlices([repoA]), 'issues', {}));
assert.equal(issueItems.find(i => !i.section && i.label === 'Needs a person').count, '3 · 42.9%');
assert.equal(issueItems.find(i => !i.section && i.label === 'Status').count, '4 · 57.1%');
const prItems = legendItems(overviewLegendHTML(overviewPRBandSlices([repoA]), 'prs', {}));
assert.equal(prItems.find(i => !i.section && i.label === 'Needs a person').count, '1 · 14.3%');
assert.equal(prItems.find(i => !i.section && i.label === 'Status').count, '6 · 85.7%');
// The percent-only label mode applies to the subtotals too.
assert.equal(legendItems(overviewLegendHTML(overviewIssueBandSlices([repoA]), 'issues', { labelMode: 'percent' })).find(i => !i.section && i.label === 'Needs a person').count, '42.9%');
// Nothing for you to do: Needs a person still shows, at 0, and never dims.
const idle = cardRepoA();
for (const list of ['actionableIssues', 'nonActionableIssues']) idle[list] = idle[list].filter(r => r.card_group !== 'your-turn');
const idleItems = legendItems(overviewLegendHTML(overviewIssueBandSlices([idle]), 'issues', {}));
const idleTodo = idleItems.find(i => !i.section && i.label === 'Needs a person');
assert.equal(idleTodo.count, '0 · 0.0%');
assert.ok(!idleTodo.cls.includes('overview-chart-legend-zero'));
// Without a kind (the legend drawn inside the donut) no row has a download.
assert.doesNotMatch(overviewCardLegendHTML(overviewIssueBandSlices([repoA]), '', {}), /overview-band-download/);
// Without the card the fallback legend is not split.
delete window._lastStatus.overview_card;
window._lastStatus.overview_slices = { issues: [{ key: 'your-turn', label: 'Your turn', rule: 'r' }, { key: 'ready', label: 'Ready', rule: 'r' }], prs: [] };
const old = cardRepoA();
old.actionableIssues.forEach((r, i) => { r.chart_slice = i ? 'ready' : 'your-turn'; });
assert.doesNotMatch(overviewLegendHTML(overviewIssueBandSlices([old]), 'issues', {}), /overview-chart-legend-section|overview-chart-legend-subtotal/);
`
	runOverviewCardNode(t, script)
}

// The unexplained band (No rule matched, a hive bug) is drawn after the
// Needs a person subtotal with a muted "should be 0" note, marked as a
// warning above 0.
// It still counts in that subtotal and exports its own card_band; the donut
// keeps its slice in plot order, so only the legend row moves.
func TestOverviewCardLegendNoRuleMatchedAfterSubtotal(t *testing.T) {
	html := indexHTML(t)
	script := overviewCardUIPrelude(t, html) + overviewCardTodoStatusParse + `
window._lastStatus = { hiveId: 'h', overview_card: cardFixture() };
const repoA = cardRepoA();
repoA.openPrs.push(cardRow(18, 'unexplained', 'your-turn', { ownership: { your_turn: false } }));
_overviewLastRepos = [repoA];
const slices = overviewPRBandSlices([repoA]);
const keys = slices.map(s => s.key);
assert.equal(keys.indexOf('unexplained'), keys.indexOf('to-close') + 1, 'the donut keeps unexplained in plot order');
const total = sumOf(slices);
const items = legendItems(overviewLegendHTML(slices, 'prs', { exportFormat: 'csv' }));
const todo = sectionOf(items, 'Needs a person');
const at = todo.findIndex(r => r.label === 'Needs a person');
assert.ok(at > 0, 'the Needs a person subtotal');
assert.equal(todo[at].count, '2 · ' + pct(2, total), 'the subtotal still counts the unexplained PR');
assert.ok(!todo.slice(0, at).some(r => r.key === 'unexplained'), 'not among the to-do rows');
const bug = todo[at + 1];
assert.ok(bug, 'a row after the subtotal');
assert.equal(bug.key, 'unexplained');
assert.equal(bug.label, 'No rule matched (hive bug)');
assert.equal(bug.note, 'should be 0');
assert.match(bug.chunk, /<span class="overview-chart-legend-sr">,<\/span> <span class="overview-chart-legend-note">should be 0<\/span><\/span>/, 'the note sits inside the label, after a spoken comma');
assert.ok(bug.cls.includes('overview-chart-legend-warning'), 'marked as a warning above 0');
assert.ok(!bug.cls.includes('overview-chart-legend-part') && !bug.cls.includes('overview-chart-legend-subtotal'));
assert.equal(bug.count, '1 · ' + pct(1, total));
assert.equal(bug.swatch, 'overview-card-pr-unexplained');
assert.equal(new URL(bug.url, 'https://hive.test').searchParams.get('card_band'), 'unexplained', 'it exports its own band');
assert.equal(todo.length, at + 2, 'nothing else follows it in Needs a person');
assert.doesNotMatch(overviewLegendHTML(slices, 'prs', {}), /[\u2013\u2014@]/);
`
	runOverviewCardNode(t, script)
}

// The served status (the server's own card) splits the same way: Needs a
// person is the Your turn group's count and Status every other group's.
func TestOverviewCardLegendToDoAndStatusServed(t *testing.T) {
	served := cardPartitionServer().statusWithOverviewBands(cardPartitionStatus(), cardTestNow)
	if served.OverviewCard == nil {
		t.Fatal("the served status carries no overview_card")
	}
	payload, err := json.Marshal(served)
	if err != nil {
		t.Fatal(err)
	}
	html := indexHTML(t)
	script := overviewCardUIPrelude(t, html) + overviewCardTodoStatusParse + "\nwindow._lastStatus = " + string(payload) + ";\n" + `
const card = window._lastStatus.overview_card;
_overviewLastRepos = window._lastStatus.repos;
const yourTurn = card.groups.find(g => g.key === 'your-turn');
assert.ok(yourTurn, 'the server sends the Your turn group');
for (const [kind, field, slices] of [['issues', 'issues', overviewIssueBandSlices(_overviewLastRepos)], ['prs', 'prs', overviewPRBandSlices(_overviewLastRepos)]]) {
  const total = sumOf(slices);
  if (!total) continue;
  const items = legendItems(overviewLegendHTML(slices, kind, {}));
  const todo = items.find(i => !i.section && i.label === 'Needs a person');
  const status = items.find(i => !i.section && i.label === 'Status');
  // The groups count the rows on the donuts.
  const others = card.groups.filter(g => g.key !== 'your-turn').reduce((n, g) => n + Number(g[field] || 0), 0);
  assert.equal(yourTurn[field] + others, total, kind + ': the groups cover the donut');
  assert.equal(todo.count, yourTurn[field] + ' · ' + pct(yourTurn[field], total), kind + ' Needs a person');
  assert.equal(status.count, others + ' · ' + pct(others, total), kind + ' Status');
}
process.stdout.write('ok');
`
	if got := runOverviewCardNode(t, script); strings.TrimSpace(got) != "ok" {
		t.Fatalf("unexpected node output: %s", got)
	}
}

// The section and subtotal styles ship, the chart title is the level 3
// heading the level 4 section headings sit under, and the split code has no
// em or en dashes, "@" in its strings, or inline handlers.
func TestOverviewCardLegendToDoAndStatusStatic(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		".overview-chart-legend-section {",
		".overview-chart-legend-section-note {",
		".overview-chart-legend-subtotal {",
		".overview-chart-legend-static { cursor: default; }",
		".overview-chart-swatch-none { visibility: hidden; }",
		".overview-chart-legend-sr {",
		".overview-chart-legend-warning .overview-chart-legend-label {",
		".overview-chart-legend-note {",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	if panel := jsFunc(t, html, "renderOverviewPanel"); !strings.Contains(panel, `<div class="overview-chart-title" role="heading" aria-level="3">`) {
		t.Error("the chart title is not a level 3 heading")
	}
	for _, name := range []string{"overviewCardLegendRows", "overviewCardLegendHTML"} {
		src := jsFunc(t, html, name)
		if strings.ContainsAny(src, "–—@") {
			t.Errorf("%s() contains an em or en dash or an @", name)
		}
		if strings.Contains(src, "onclick") {
			t.Errorf("%s() uses an inline handler", name)
		}
	}
}
