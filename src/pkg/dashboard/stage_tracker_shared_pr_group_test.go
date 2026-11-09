package dashboard

import (
	"strings"
	"testing"
)

// TestStageTrackerSharedPRGroupJS: the rows of issues one PR closes together
// are one unit of the list — the secondary sits right after its primary
// whatever the lead sort put between them, draws as its child, is built from
// the primary's own PR snapshot, and folds under the primary's "also closes"
// pill (remembered in localStorage). A secondary whose primary is not a row
// of this list stays standalone, as before.
func TestStageTrackerSharedPRGroupJS(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowsPreludeJS(t, html)+"\n"+strings.Join([]string{
		jsFunc(t, html, "stageTrackerLinkedSupersededText"),
		jsFunc(t, html, "stageTrackerSharedPRParent"),
		jsFunc(t, html, "stageTrackerSharedPRGroups"),
		jsFunc(t, html, "stageTrackerSharedPRRebind"),
		jsFunc(t, html, "stageTrackerSharedPRContiguous"),
		jsConstLine(t, html, "STAGE_TRACKER_SHARED_COLLAPSED_KEY"),
		jsFunc(t, html, "stageTrackerLoadSharedCollapsed"),
		jsFunc(t, html, "stageTrackerSaveSharedCollapsed"),
		jsFunc(t, html, "stageTrackerToggleCollapsed"),
	}, "\n")+"\n"+stageTrackerActorIconJS(t, html)+stageTrackerRowsFixtureJS+`
const since = new Date(now - 3600000).toISOString();
const lit = (html) => (/<li class="ham-seg active[^"]*"[^>]*data-ham-stage="(\d+)"/.exec(strip(html)) || [])[1] || (/<li class="ham-seg active[^"]*"[^>]*title="([^"]*)"/.exec(strip(html)) || [])[1] || '';
const li = (n) => ({ number: n, title: 'Issue ' + n, url: 'https://x/issues/' + n });
const pr = (n, under, extra) => Object.assign({ number: n, title: 'fix', url: 'https://x/' + n, author: 'hive-bot', author_kind: 'hive', stage: 95, stage_since: since, drawn_under: under, merge_gate: { state: 'soaking' } }, extra || {});
const issue = (n, prs, extra) => Object.assign({ repo: 'hivecommons/hive', number: n, kind: 'issue', title: 'Issue ' + n, url: 'https://x/issues/' + n, stage: 40, stage_since: since, linked_prs: prs || [] }, extra || {});
const nums = (rows) => rows.map(r => r.number);

// The live board: #17292 (primary, PR #17308 also closes #17302) at the top,
// an unrelated #17329 between, #17302 (secondary) third.
const primary = issue(17292, [pr(17308, 17292, { also_closes: [li(17302)] })]);
const between = issue(17329, [], { stage: 30 });
// The secondary's copy of the PR is a different object from an older tick.
const secondary = issue(17302, [pr(17308, 17292, { stage: 50, merge_gate: null, stage_since: new Date(now - 7200000).toISOString() })]);

// Groups: the secondary belongs to the primary, by row key.
let g = stageTrackerSharedPRGroups([primary, between, secondary], stages);
assert.deepStrictEqual(g.parentOf, { 'hivecommons/hive/issue#17302': 'hivecommons/hive/issue#17292' });
assert.deepStrictEqual(g.children, { 'hivecommons/hive/issue#17292': ['hivecommons/hive/issue#17302'] });
assert.deepStrictEqual(g.prs, { 'hivecommons/hive/issue#17292': { 17308: true } });

// Adjacency: the secondary follows its primary; the unrelated row moves down.
let rows = stageTrackerSharedPRContiguous([primary, between, secondary], stages, {});
assert.deepStrictEqual(nums(rows), [17292, 17302, 17329], 'secondary right after its primary');
// The group sorts by the primary: a secondary ahead of its primary comes down to it.
rows = stageTrackerSharedPRContiguous([secondary, between, primary], stages, {});
assert.deepStrictEqual(nums(rows), [17329, 17292, 17302], 'group takes the primary\'s place');
// Several secondaries keep their lead order among themselves.
const third = issue(17310, [pr(17308, 17292, { stage: 50 })]);
const wide = issue(17292, [pr(17308, 17292, { also_closes: [li(17302), li(17310)] })]);
rows = stageTrackerSharedPRContiguous([third, between, wide, secondary], stages, {});
assert.deepStrictEqual(nums(rows), [17329, 17292, 17310, 17302]);
// Nothing to group → the very same array.
const plain = [between, issue(1, [pr(2, 1)])];
assert.strictEqual(stageTrackerSharedPRContiguous(plain, stages, {}), plain);

// Same snapshot: the secondary's listing of the PR becomes the primary's own
// entry (drawn_under kept, also_closes dropped) — stage 95 soaking, not the
// stale 50 — so both chevron strips light the same segment.
let sec = rows[3];
assert.strictEqual(sec.number, 17302);
assert.strictEqual(sec.linked_prs[0].stage, 95, 'rebound to the primary\'s PR snapshot');
assert.deepStrictEqual(sec.linked_prs[0].merge_gate, { state: 'soaking' });
assert.strictEqual(sec.linked_prs[0].drawn_under, 17292);
assert.strictEqual(sec.linked_prs[0].also_closes, null, 'the pill stays on the primary alone');
assert.strictEqual(secondary.linked_prs[0].stage, 50, 'the server payload is not mutated');
assert.notStrictEqual(sec, secondary, 'the row is a copy');
g = stageTrackerSharedPRGroups(rows, stages);
const ctxG = Object.assign({}, ctx, { shared: Object.assign(g, { collapsed: {} }) });
const headRow = stageTrackerRowHtml(rows[1], stages, ctxG);
const childRow = stageTrackerRowHtml(sec, stages, ctxG);
assert.strictEqual(lit(childRow), lit(headRow), 'both strips light from the same PR state: ' + lit(headRow) + ' vs ' + lit(childRow));
assert.notStrictEqual(lit(childRow), lit(stageTrackerRowHtml(secondary, stages, ctx)), 'the stale copy would have lit elsewhere');

// Visual: the primary heads the group, each secondary is a child; the last wears ham-shared-last.
assert.ok(/<div class="ham-row[^"]* ham-shared-head[^"]*"/.test(headRow), headRow.slice(0, 300));
assert.ok(/<div class="ham-row[^"]* ham-shared-child ham-shared-last"/.test(childRow), childRow.slice(0, 300));
assert.ok(childRow.includes('data-ham-shared-under="17292"'), childRow.slice(0, 300));
const midRow = stageTrackerRowHtml(rows[2], stages, ctxG);
assert.ok(/ ham-shared-child"/.test(midRow) && !midRow.includes('ham-shared-last'), midRow.slice(0, 300));
assert.ok(childRow.includes('↳ #17308 · under #17292'), 'the under chip stays');
assert.ok(!childRow.includes('ham-also-closes'), 'no reciprocal pill on a child');

// The primary's pill is the fold: a caret button, expanded by default.
const pi = headRow.indexOf('<span class="ham-badge ham-also-closes ham-shared-head-pill"');
assert.ok(pi >= 0, 'the primary wears the group pill: ' + headRow);
const pill = headRow.slice(pi, headRow.indexOf('</a></span>', pi) + '</a></span>'.length);
const pillTitle = (/ title="([^"]*)"/.exec(pill) || [])[1] || '';
assert.ok(pill.includes('data-action="hamSharedToggle" data-arg0="hivecommons/hive/issue#17292" data-stop="1" aria-expanded="true"'), pill);
assert.ok(pill.includes('aria-label="Fold 2 issue rows this PR also closes"'), pill);
assert.ok(pill.includes('▾') && text(pill).includes('also closes #17302, #17310'), text(pill));
assert.ok(pillTitle.includes('2 issue rows drawn under this row'), pillTitle);
assert.ok(!pill.slice(0, pill.indexOf('</button>')).includes('<a '), 'no link inside the button');

// Folded: the secondaries leave the list, the primary shows ▸ and +N, no head rail.
const folded = { 'hivecommons/hive/issue#17292': true };
rows = stageTrackerSharedPRContiguous([third, between, wide, secondary], stages, folded);
assert.deepStrictEqual(nums(rows), [17329, 17292], 'folded secondaries are not rows');
const ctxF = Object.assign({}, ctx, { shared: Object.assign(stageTrackerSharedPRGroups([third, between, wide, secondary], stages), { collapsed: folded }) });
const foldedRow = stageTrackerRowHtml(wide, stages, ctxF);
assert.ok(foldedRow.includes('ham-shared-folded') && foldedRow.includes('aria-expanded="false"') && foldedRow.includes('▸'), foldedRow);
assert.ok(foldedRow.includes('<span class="ham-shared-count">+2</span>'), foldedRow);
assert.ok(!/<div class="ham-row[^"]*ham-shared-head[^"]*"/.test(foldedRow), 'no rail below a folded head');

// Fallback: the primary filtered out (not a row) → the secondary stands alone, as before.
rows = stageTrackerSharedPRContiguous([between, secondary], stages, {});
assert.deepStrictEqual(nums(rows), [17329, 17302]);
assert.strictEqual(rows[1], secondary, 'untouched');
g = stageTrackerSharedPRGroups([between, secondary], stages);
assert.deepStrictEqual(g.parentOf, {});
const alone = stageTrackerRowHtml(secondary, stages, Object.assign({}, ctx, { shared: Object.assign(g, { collapsed: {} }) }));
assert.ok(!alone.includes('ham-shared-child') && alone.includes('↳ #17308 · under #17292'), alone.slice(0, 300));
// A primary in another repo with the same number is not this one's.
const other = issue(17292, [pr(17308, 17292, { also_closes: [li(17302)] })], { repo: 'hivecommons/pluk' });
assert.deepStrictEqual(stageTrackerSharedPRGroups([other, secondary], stages).parentOf, {});

// Without a group the pill's markup is unchanged.
assert.strictEqual(stageTrackerAlsoClosesChipHtml(pr(17308, 17292, { also_closes: [li(17302)] })), '<span class="ham-badge ham-also-closes" title="this PR also resolves #17302 (Issue 17302)">also closes <a href="https://x/issues/17302" target="_blank" rel="noopener">#17302</a></span>');
assert.strictEqual(stageTrackerAlsoClosesChipHtml(pr(17308, 17292, { also_closes: [li(17302)] }), { key: 'k', count: 0 }), stageTrackerAlsoClosesChipHtml(pr(17308, 17292, { also_closes: [li(17302)] })), 'an empty group is no group');
const one = stageTrackerAlsoClosesChipHtml(pr(17308, 17292, { also_closes: [li(17302)] }), { key: 'k', count: 1, collapsed: true });
assert.ok(one.includes('aria-label="Show 1 issue row this PR also closes"'), one);

// The fold persists under hive.tracker.shared.collapsed, like the repo groups.
const store = {};
global.localStorage = { getItem: k => (k in store ? store[k] : null), setItem(k, v) { store[k] = String(v); } };
assert.deepStrictEqual(stageTrackerLoadSharedCollapsed(), {});
let c = stageTrackerToggleCollapsed({}, 'hivecommons/hive/issue#17292');
stageTrackerSaveSharedCollapsed(c);
assert.strictEqual(store[STAGE_TRACKER_SHARED_COLLAPSED_KEY], '{"hivecommons/hive/issue#17292":true}');
assert.strictEqual(STAGE_TRACKER_SHARED_COLLAPSED_KEY, 'hive.tracker.shared.collapsed');
assert.deepStrictEqual(stageTrackerLoadSharedCollapsed(), { 'hivecommons/hive/issue#17292': true }, 'round-trips');
store[STAGE_TRACKER_SHARED_COLLAPSED_KEY] = '[1]';
assert.deepStrictEqual(stageTrackerLoadSharedCollapsed(), {}, 'a malformed value is empty');
`)
}

// TestStageTrackerSharedPRGroupWiring: the render applies the grouping after
// the lead sort, hands the groups to the rows, counts folded rows in the
// total, the caret's handler and CSS are in the page, and the CSS is tokens only.
func TestStageTrackerSharedPRGroupWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`ctx.shared = sharing ? Object.assign(stageTrackerSharedPRGroups(prepared, stages), { collapsed: _stageTrackerSharedCollapsed }) : null;`,
		`const ordered = sharing ? stageTrackerSharedPRContiguous(prepared, stages, _stageTrackerSharedCollapsed) : prepared;`,
		`if (foldedShared > 0) page.total += foldedShared;`,
		`if (typeof stageTrackerLoadSharedCollapsed === 'function') _stageTrackerSharedCollapsed = stageTrackerLoadSharedCollapsed();`,
		`function hamSharedToggle(key) {`,
		`.ham-row.ham-shared-child {`, `.ham-row.ham-shared-head::before {`, `.ham-shared-toggle {`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html lacks %q", want)
		}
	}
	start := strings.Index(html, ".ham-row.ham-shared-child {")
	end := strings.Index(html[start:], ".ham-shared-count {")
	if start < 0 || end < 0 {
		t.Fatal("shared-PR group CSS block not found")
	}
	block := html[start : start+end]
	for _, bad := range []string{"#", "rgb(", "hsl("} {
		if strings.Contains(block, bad) {
			t.Errorf("shared-PR group CSS uses a literal colour (%q); tokens only", bad)
		}
	}
}
