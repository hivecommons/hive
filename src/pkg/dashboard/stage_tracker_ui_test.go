package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestStageTrackerSectionWiring pins the Hive activity monitor into the shared
// section machinery (nav, layout template, collapse, help, poll) and the
// stylesheet rules the strip depends on (pulse + reduced-motion).
func TestStageTrackerSectionWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`id="activity-monitor-section" data-dashboard-section="activity-monitor-section"`,
		`data-section="activity-monitor-section" data-action="ocNavigate"`,
		`📍 Hive activity monitor`,
		`id="activity-monitor-panel"`,
		`<label class="ham-repo-filter"><span>Repo</span><select id="activity-monitor-repo" class="ham-select"`,
		`.ham-select {`,
		`.ham-repo-filter {`,
		`id="activity-monitor-sidebar-badge"`,
		`'activity-monitor-section': { title: '📍 Hive activity monitor'`,
		`'activity-monitor-section': { heading: 'Hive activity monitor'`,
		`'activity-monitor-section', 'agents-section', 'agent-activity-section', 'faq-section'`,
		`fetch('/api/stage-tracker', _stageTrackerEtag ? { headers: { 'If-None-Match': _stageTrackerEtag } } : undefined)`,
		// A 304 carries no body: generated_at/source come from the headers so the
		// "updated" chip stays truthful on a quiet hive.
		`if (r.status === 304) {`,
		`const at = r.headers.get('X-Hive-Stage-Tracker-Generated-At');`,
		`if (at) _stageTracker.generated_at = at;`,
		`stageTrackerTickClock();`,
		`dashboardSetInterval('stage-tracker', fetchStageTracker, STAGE_TRACKER_POLL_MS)`,
		`dashboardSetInterval('stage-tracker-clock', stageTrackerTickClock, STAGE_TRACKER_CLOCK_MS)`,
		`id="activity-monitor-q" class="ham-input" type="search"`,
		`data-input-action="hamFilter" data-arg0="q" data-arg-types="s,w"`,
		`id="activity-monitor-age"`, `id="activity-monitor-author"`, `id="activity-monitor-sort"`, `id="activity-monitor-size"`,
		`id="activity-monitor-attention" data-action="hamFilter" data-arg0="attention" data-arg1="toggle"`,
		`id="activity-monitor-dir" data-action="hamFilter" data-arg0="dir" data-arg1="toggle"`,
		`id="activity-monitor-reset" data-action="hamFilter" data-arg0="reset" hidden`,
		`<div class="ham-labels" id="activity-monitor-labels"`,
		`<div class="ham-meta" id="activity-monitor-meta" aria-live="polite">`,
		`localStorage.setItem(STAGE_TRACKER_FILTER_KEY, JSON.stringify(f))`,
		`function stageTrackerRowHtml(item, stages, ctx)`,
		`function stageTrackerHeaderHtml(stages)`,
		`const STAGE_TRACKER_ICONS = {`,
		`@keyframes ham-pulse`,
		// The bare lit rule carries no animation: Open, Open · hold and Claimed are steady.
		`.ham-seg.active { background: var(--amber); color: var(--surface-0); }`,
		`.ham-seg.active[data-motion="active"]::before { background: white; animation: ham-pulse 1.6s ease-in-out infinite;`,
		`.ham-seg.active[data-motion="needs-human"]::before { background: var(--red); animation: ham-breathe-human`,
		// Compositor-safe (#13764): the infinite pulses animate only opacity on an overlay.
		`@keyframes ham-pulse { 0%, 100% { opacity: 0; } 50% { opacity: 0.3; } }`,
		`@keyframes ham-breathe-human { 0%, 100% { opacity: 0; } 50% { opacity: 0.55; } }`,
		`@keyframes ham-breathe-human`,
		`function stageTrackerMotion(item)`,
		`.ham-seg.active.ham-open-hold`,
		`.ham-seg.active.ham-open-blocked { background: var(--yellow); color: var(--surface-0); }`,
		`.ham-seg.active.ham-open-blocked .ham-seg-t::before { content: '⛔';`,
		`@media (prefers-reduced-motion: reduce) { .ham-seg.active { animation: none;`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing Hive activity monitor wiring %q", want)
		}
	}
	if strings.Contains(jsConstBlock(t, html, "DASHBOARD_DEFAULT_COLLAPSED_SECTIONS"), "'activity-monitor-section'") {
		t.Fatalf("activity-monitor-section must be expanded for a new browser: it answers 'is the hive moving?'")
	}
	// The repo filter must not inherit the button chrome (#12599 follow-up):
	// hv-btn brings control-height padding and a heavy border that dwarfs the chips.
	if strings.Contains(html, `id="activity-monitor-repo" class="hv-btn`) {
		t.Fatalf("activity monitor repo select must use .ham-select, not .hv-btn")
	}
}

// TestStageTrackerFilterSortPage executes the pure filter / sort / paging
// helpers under node: free text, label families (OR within, AND across),
// age thresholds incl. "stale" (past the stage dwell), author kind, needs
// attention, the four sort orders with direction, page clamping and the
// "showing X–Y of N (M filtered out)" text.
func TestStageTrackerFilterSortPage(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — Hive activity monitor filters were NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"const window = {};",
		jsConstLine(t, html, "STAGE_TRACKER_PAGE_SIZES"),
		jsConstLine(t, html, "STAGE_TRACKER_DEFAULT_PAGE_SIZE"),
		jsConstLine(t, html, "STAGE_TRACKER_LABEL_CHIP_LIMIT"),
		jsConstLine(t, html, "STAGE_TRACKER_AGE_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_ATTENTION_STAGES"),
		jsConstLine(t, html, "STAGE_TRACKER_LABEL_FAMILIES"),
		jsConstLine(t, html, "STAGE_TRACKER_SORTS"),
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL"),
		stageTrackerSentinelJS(t, html),
		jsConstLine(t, html, "STAGE_TRACKER_PATH"),
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_DECISION"),
		stageTrackerManualMergeJS(t, html),
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_NEEDS_DECISION"),
		jsConstLine(t, html, "STAGE_TRACKER_CLOSED_ISSUE_TITLE"),
		jsConstLine(t, html, "STAGE_TRACKER_UNTITLED_ISSUE"),
		jsConstLine(t, html, "STAGE_TRACKER_SOFT_LINK_VIAS"),
		jsConstLine(t, html, "STAGE_TRACKER_SOFT_LINK_TIP"),
		jsConstLine(t, html, "STAGE_TRACKER_TRIAGE_NEEDED"),
		jsConstLine(t, html, "STAGE_TRACKER_POSSIBLY_EXISTS"),
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_DECISION_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_PRE_MERGE_SOAK"),
		jsFunc(t, html, "stageTrackerDefaultFilter"),
		"var _stageTrackerFilter = stageTrackerDefaultFilter();",
		jsFunc(t, html, "stageTrackerAnchor"),
		jsFunc(t, html, "stageTrackerLinkedDrawnHere"),
		jsFunc(t, html, "stageTrackerLinkedSuperseder"),
		jsFunc(t, html, "stageTrackerLinkedSupersededGone"),
		jsFunc(t, html, "stageTrackerLinkedSupersededTip"),
		jsFunc(t, html, "stageTrackerLinkedSorted"),
		stageTrackerTierJS(t, html),
		jsFunc(t, html, "stageTrackerSortKey"),
		jsConstLine(t, html, "STAGE_TRACKER_HUMAN_MODS"),
		jsConstLine(t, html, "STAGE_TRACKER_HANDLED_TEXT"),
		jsConstLine(t, html, "STAGE_TRACKER_HANDLED_NOTE"),
		jsFunc(t, html, "stageTrackerRegressionHandled"),
		jsFunc(t, html, "stageTrackerNeedsAttention"),
		jsFunc(t, html, "stageTrackerWaitSince"),
		jsFunc(t, html, "stageTrackerLabelFamily"),
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_HUMAN_STAGES"),
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_HUMAN_AUTHOR_STAGES"),
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_HUMAN_MODS"),
		jsFunc(t, html, "stageTrackerItemOnHold"),
		jsFunc(t, html, "stageTrackerItemNeedsHuman"),
		jsFunc(t, html, "stageTrackerLabelCompare"),
		jsFunc(t, html, "stageTrackerLabelDialogData"),
		jsConstBlock(t, html, "STAGE_TRACKER_ICONS"),
		stageTrackerActedJS(t, html),
		jsFunc(t, html, "stageTrackerNormalizeFilter"),
		jsFunc(t, html, "stageTrackerFilterActive"),
		jsFunc(t, html, "stageTrackerMatches"),
		jsFunc(t, html, "stageTrackerVisible"),
		jsFunc(t, html, "stageTrackerRowCount"),
		jsFunc(t, html, "stageTrackerSort"),
		jsFunc(t, html, "stageTrackerPage"),
		jsFunc(t, html, "stageTrackerLabelFacets"),
		jsFunc(t, html, "stageTrackerCountsText"),
		jsFunc(t, html, "stageTrackerRowBreakdown"),
	}, "\n") + `
const assert = require('assert');
const now = Date.parse('2026-01-01T12:00:00Z');
const H = 3600000, D = 24 * H;
const ago = (ms) => new Date(now - ms).toISOString();
const stages = [
  { number: 10, label: 'Actionable', dwell_seconds: 7 * 86400 }, { number: 20, label: 'Claimed', dwell_seconds: 4 * 3600 },
  { number: 50, label: 'Review pending', dwell_seconds: 86400 }, { number: 80, label: 'Merge gate' }, { number: 110, label: 'Merged' },
];
const items = [
  // #1: unlabelled human issue opened 2 days ago, no step yet (the "Open" step).
  { kind: 'issue', repo: 'a/b', number: 1, title: 'Login page crashes', stage: 0, created_at: ago(2 * D), author_kind: 'human', labels: ['bug'], modifiers: [] },
  // #2: clanker issue claimed 5h ago (past the 4h dwell), stage/020 + author/clanker.
  { kind: 'issue', repo: 'a/b', number: 2, title: 'Refactor scanner', stage: 20, stage_since: ago(5 * H), created_at: ago(3 * D), author_kind: 'clanker', labels: ['stage/020-claimed', 'author/clanker', 'bug'], modifiers: [] },
  // #3: hive PR in review for 30 min, needs-rebase.
  { kind: 'pr', repo: 'a/b', number: 3, title: 'Fix login crash', stage: 50, stage_since: ago(30 * 60000), created_at: ago(H), author_kind: 'hive', labels: ['stage/050-review', 'author/hive', 'needs-rebase'], modifiers: ['needs-rebase'] },
  // #4: human PR at the merge gate for 9 days in another repo.
  { kind: 'pr', repo: 'a/c', number: 4, title: 'Docs sweep', stage: 80, stage_since: ago(9 * D), created_at: ago(10 * D), author_kind: 'human', labels: ['stage/080-merge-gate', 'author/human', 'role/docs'], modifiers: [] },
  // #5: clanker issue actionable for 2 days, under its dwell; stalled modifier.
  { kind: 'issue', repo: 'a/c', number: 5, title: 'Add paging', stage: 10, stage_since: ago(2 * D), created_at: ago(2 * D), author_kind: 'clanker', labels: ['stage/010-actionable', 'author/clanker', 'role/docs'], modifiers: ['stalled'] },
];
const ctx = { nowMs: now, stages };
const nums = (rows) => rows.map(it => it.number);
const F = (over) => Object.assign(stageTrackerDefaultFilter(), over);

// Defaults: everything, nothing active.
assert.deepStrictEqual(nums(stageTrackerVisible(items, F(), ctx)).sort(), [1, 2, 3, 4, 5]);
assert.strictEqual(stageTrackerFilterActive(F()), false);
assert.strictEqual(stageTrackerFilterActive(F({ sort: 'number', dir: 'asc', size: 25 })), false, 'sort and paging are not narrowing filters');
assert.strictEqual(stageTrackerFilterActive(F({ q: 'x' })), true);

// Free text: number (with or without #), title substring, case-insensitive, linked PR number.
assert.deepStrictEqual(nums(stageTrackerVisible(items, F({ q: '#4' }), ctx)), [4]);
assert.deepStrictEqual(nums(stageTrackerVisible(items, F({ q: '4' }), ctx)), [4]);
assert.deepStrictEqual(nums(stageTrackerVisible(items, F({ q: 'LOGIN' }), ctx)), [1, 3]);
assert.deepStrictEqual(nums(stageTrackerVisible([{ kind: 'issue', repo: 'a/b', number: 9, title: 'x', linked_prs: [{ number: 77, title: 'y' }] }], F({ q: '77' }), ctx)), [9], 'an issue matches the number of a PR it is covered by');

// Labels: OR within a family, AND across families; case-insensitive.
assert.deepStrictEqual(nums(stageTrackerVisible(items, F({ labels: ['author/clanker'] }), ctx)), [2, 5]);
assert.deepStrictEqual(nums(stageTrackerVisible(items, F({ labels: ['author/clanker', 'author/human'] }), ctx)), [1, 2, 4, 5].filter(n => n !== 1), 'two author chips OR together; #1 has no author label');
assert.deepStrictEqual(nums(stageTrackerVisible(items, F({ labels: ['author/clanker', 'role/docs'] }), ctx)), [5], 'author and role chips AND together');
assert.deepStrictEqual(nums(stageTrackerVisible(items, F({ labels: ['BUG'] }), ctx)), [1, 2]);
assert.deepStrictEqual(nums(stageTrackerVisible(items, F({ labels: ['needs-rebase'] }), ctx)), [3]);

// Age: on-step time, falling back to opened time for the "Open" step.
assert.deepStrictEqual(nums(stageTrackerVisible(items, F({ age: '1h' }), ctx)), [1, 2, 4, 5], '#3 has been on its step for 30m');
assert.deepStrictEqual(nums(stageTrackerVisible(items, F({ age: '1d' }), ctx)), [1, 4, 5]);
assert.deepStrictEqual(nums(stageTrackerVisible(items, F({ age: '1w' }), ctx)), [4]);
assert.deepStrictEqual(nums(stageTrackerVisible(items, F({ age: 'stale' }), ctx)), [2, 5], 'stale = past the stage dwell (#2) or stalled (#5); 080 has no dwell');

// Author kind, attention, repo, kind.
assert.deepStrictEqual(nums(stageTrackerVisible(items, F({ author: 'hive' }), ctx)), [3]);
assert.deepStrictEqual(nums(stageTrackerVisible(items, F({ author: 'human' }), ctx)), [1, 4]);
assert.deepStrictEqual(nums(stageTrackerVisible(items, F({ attention: true }), ctx)), [3, 5]);
assert.deepStrictEqual(nums(stageTrackerVisible(items, F({ repo: 'a/c', kind: 'pr' }), ctx)), [4]);
assert.deepStrictEqual(nums(stageTrackerVisible(items, F({ attention: true, author: 'human' }), ctx)), [], 'filters compose');

// A PR nested under a listed issue shares that issue's row; when a filter
// (label chips, the Pull requests tab, …) drops the issue the PR must still
// render as its own row — an item never disappears because its issue did.
const nestedItems = items.concat([
  { kind: 'issue', repo: 'a/b', number: 6, title: 'Covered', stage: 20, stage_since: ago(H), created_at: ago(D), author_kind: 'human', labels: ['stage/020-claimed', 'bug'], modifiers: [], linked_prs: [{ number: 7, stage: 50 }] },
  { kind: 'pr', repo: 'a/b', number: 7, title: 'Covers 6', stage: 50, stage_since: ago(H), created_at: ago(H), author_kind: 'hive', labels: ['stage/050-review', 'author/hive'], modifiers: [], nested_under: 6 },
]);
assert.deepStrictEqual(nums(stageTrackerVisible(nestedItems, F(), ctx)).sort(), [1, 2, 3, 4, 5, 6], 'issue listed → the PR rides its row, not its own');
assert.deepStrictEqual(nums(stageTrackerVisible(nestedItems, F({ labels: ['author/hive'] }), ctx)), [3, 7], 'issue filtered out → the PR is its own row');
assert.deepStrictEqual(nums(stageTrackerVisible(nestedItems, F({ kind: 'pr' }), ctx)), [3, 4, 7], 'the Pull requests view lists every PR');
assert.deepStrictEqual(nums(stageTrackerVisible(nestedItems, F({ q: '7' }), ctx)), [6], 'a text match on the PR number also matches its issue, which then carries the PR');
assert.deepStrictEqual(nums(stageTrackerVisible(nestedItems, F({ q: 'Covers' }), ctx)), [7], 'only the PR matches → own row');
// The sidebar pill and the "of N" baseline are the default view's rows:
// 7 items, 6 rows (PR #7 rides issue #6) — the same whichever tab is on.
assert.strictEqual(stageTrackerRowCount(nestedItems, ctx), 6, 'rows, not items');
assert.strictEqual(stageTrackerRowCount(items, ctx), 5);
assert.strictEqual(stageTrackerCountsText(stageTrackerPage(stageTrackerVisible(nestedItems, F({ kind: 'pr' }), ctx), F()), stageTrackerRowCount(nestedItems, ctx), 0, { issues: 3, prs: 4 }).badge, 6, 'the Pull requests tab does not move the pill');

// Normalisation tolerates junk and partial objects (older stored state).
const n = stageTrackerNormalizeFilter({ kind: 'bogus', labels: 'nope', age: '2h', sort: 'x', dir: 'sideways', size: 7, page: -3 });
assert.deepStrictEqual([n.kind, n.labels, n.age, n.sort, n.dir, n.size, n.page], ['all', [], 'any', 'furthest', 'desc', STAGE_TRACKER_DEFAULT_PAGE_SIZE, 1]);
assert.deepStrictEqual(stageTrackerNormalizeFilter({ kind: 'pr', repo: 'a/b' }).labels, [], 'the pre-filter shape still works');
assert.deepStrictEqual(stageTrackerNormalizeFilter({ labels: ['Author/Human'] }).labels, ['author/human']);

// Sorting: furthest along first (then longest on step); waiting; newest; number; direction flips.
assert.deepStrictEqual(nums(stageTrackerSort(items, stages, F({ sort: 'furthest' }))), [4, 3, 2, 5, 1]);
assert.deepStrictEqual(nums(stageTrackerSort(items, stages, F({ sort: 'furthest', dir: 'asc' }))), [1, 5, 2, 3, 4]);
assert.deepStrictEqual(nums(stageTrackerSort(items, stages, F({ sort: 'waiting' }))), [5, 3, 4, 1, 2], 'attention rows (stalled #5, needs-rebase #3) first, then longest wait; an unstaged item waits since it was opened');
assert.deepStrictEqual(nums(stageTrackerSort(items, stages, F({ sort: 'waiting', dir: 'asc' }))), [3, 5, 2, 1, 4], 'ascending flips the wait order but keeps attention on top');
assert.deepStrictEqual(nums(stageTrackerSort(items, stages, F({ sort: 'newest' }))), [3, 5, 1, 2, 4], 'newest first; equal ages fall back to the higher number');
assert.deepStrictEqual(nums(stageTrackerSort(items, stages, F({ sort: 'number' }))), [5, 4, 3, 2, 1], 'number sort descends by repo then number');
assert.deepStrictEqual(nums(stageTrackerSort(items, stages, F({ sort: 'number', dir: 'asc' }))), [1, 2, 3, 4, 5]);
assert.deepStrictEqual(nums(items), [1, 2, 3, 4, 5], 'sorting does not mutate the payload');

// Paging: sizes, clamping past the end, the counts text.
const many = Array.from({ length: 120 }, (_, i) => ({ kind: 'issue', repo: 'a/b', number: i + 1 }));
let pg = stageTrackerPage(many, F({ size: 50 }));
assert.deepStrictEqual([pg.page, pg.pages, pg.from, pg.to, pg.total, pg.rows.length], [1, 3, 1, 50, 120, 50]);
pg = stageTrackerPage(many, F({ size: 50, page: 3 }));
assert.deepStrictEqual([pg.from, pg.to, pg.rows.length, pg.rows[0].number], [101, 120, 20, 101]);
pg = stageTrackerPage(many, F({ size: 25, page: 99 }));
assert.deepStrictEqual([pg.page, pg.pages, pg.from, pg.to], [5, 5, 101, 120], 'a page past the end clamps to the last page');
pg = stageTrackerPage(many, F({ size: 100 }));
assert.strictEqual(pg.pages, 2);
pg = stageTrackerPage([], F());
assert.deepStrictEqual([pg.page, pg.pages, pg.from, pg.to, pg.total], [1, 1, 0, 0, 0]);
let c = stageTrackerCountsText(stageTrackerPage(many, F({ size: 50, page: 2 })), 150, 7);
assert.strictEqual(c.showing, 'showing 51–100 of 120 (30 filtered out)');
assert.strictEqual(c.header, '120 of 150 rows · 7 need attention');
assert.strictEqual(c.badge, 150, 'the pill is the default view\'s row count, filters notwithstanding');
assert.strictEqual(c.badgeTitle, '150 rows · 7 need attention');
// Owner: rows are canonical. Open-item totals only footnote PRs that share
// their issue's row; they never become a second total or change the pill.
c = stageTrackerCountsText(stageTrackerPage(many, F({ size: 50, page: 2 })), 150, 7, { issues: 100, prs: 52 }, 9);
assert.strictEqual(c.header, "120 of 150 rows (100 issues + 50 PRs on their own row) · 7 need attention · 2 PRs shown on their issue's row", 'header spells out how the default view\'s rows are made');
assert.strictEqual(c.badge, 150);
assert.strictEqual(c.note, "2 PRs shown on their issue's row");
assert.strictEqual(c.breakdown, '100 issues + 50 PRs on their own row');
assert.strictEqual(c.badgeTitle, "150 rows (100 issues + 50 PRs on their own row) · 9 need attention · 2 PRs shown on their issue's row", 'pill tooltip counts attention over every item, names the row make-up and footnotes shared rows');
assert.strictEqual(c.showing, 'showing 51–100 of 120 (30 filtered out)', 'the showing line never prints an items total');
c = stageTrackerCountsText(stageTrackerPage(many, F({ size: 50, page: 2 })), 150, 7, { issues: 100, prs: 50 }, 9);
assert.strictEqual(c.note, '', 'rows = items → no footnote');
assert.strictEqual(c.breakdown, '100 issues + 50 PRs on their own row');
// Owner (2026-10-29): Overview 33/13 beside activity monitor 37 — rows are
// 33 issues + 4 PRs that have no listed issue; 9 PRs sit on their issue's row.
c = stageTrackerCountsText(stageTrackerPage(many.slice(0, 37), F({ size: 50 })), 37, 3, { issues: 33, prs: 13 }, 3);
assert.strictEqual(c.header, "37 rows (33 issues + 4 PRs on their own row) · 3 need attention · 9 PRs shown on their issue's row");
assert.strictEqual(c.badgeTitle, "37 rows (33 issues + 4 PRs on their own row) · 3 need attention · 9 PRs shown on their issue's row");
assert.deepStrictEqual(stageTrackerRowBreakdown(37, { issues: 33, prs: 13 }), { known: true, rows: 37, issues: 33, prs: 13, standalonePRs: 4, shared: 9 });
assert.deepStrictEqual(stageTrackerRowBreakdown(34, { issues: 33, prs: 1 }), { known: true, rows: 34, issues: 33, prs: 1, standalonePRs: 1, shared: 0 });
assert.strictEqual(stageTrackerCountsText(stageTrackerPage(many.slice(0, 34), F({ size: 50 })), 34, 0, { issues: 33, prs: 1 }).breakdown, '33 issues + 1 PR on its own row', 'singular PR');
assert.strictEqual(stageTrackerRowBreakdown(40, { issues: 33, prs: 5 }).standalonePRs, 5, 'standalone PRs never exceed open PRs');
assert.strictEqual(stageTrackerRowBreakdown(10, null).known, false);
assert.strictEqual(stageTrackerCountsText(stageTrackerPage(many.slice(0, 10), F({ size: 50 })), 10, 0).breakdown, '', 'no totals → no breakdown, header unchanged');
c = stageTrackerCountsText({ total: 1, from: 1, to: 1 }, 1, 0, { issues: 0, prs: 1 });
assert.strictEqual(c.header, '1 row (0 issues + 1 PR on its own row) · nothing needs attention');
c = stageTrackerCountsText({ total: 0, from: 0, to: 0 }, 0, 0, { issues: 0, prs: 0 });
assert.strictEqual(c.badgeTitle, '0 rows (0 issues + 0 PRs on their own row) · nothing needs attention');
c = stageTrackerCountsText(stageTrackerPage(many.slice(0, 1), F()), 1, 1);
assert.strictEqual(c.showing, 'showing 1–1 of 1');
assert.strictEqual(c.header, '1 row · 1 needs attention');
c = stageTrackerCountsText(stageTrackerPage([], F()), 5, 0);
assert.strictEqual(c.showing, 'showing 0 of 0 (5 filtered out)');
assert.strictEqual(c.header, '0 of 5 rows · nothing needs attention');

// Label facets: stage/NNN first by number, then every other label alphabetically; every label is offered.
let fac = stageTrackerLabelFacets(items, F());
assert.deepStrictEqual(fac.chips.map(x => x.label), ['stage/010-actionable', 'stage/020-claimed', 'stage/050-review', 'stage/080-merge-gate', 'author/clanker', 'author/hive', 'author/human', 'bug', 'needs-rebase', 'role/docs']);
assert.deepStrictEqual(fac.chips.find(x => x.label === 'author/clanker').count, 2);
assert.deepStrictEqual(fac.all.map(x => x.label), fac.chips.map(x => x.label), 'under the limit the dialog list equals the row');
assert.strictEqual(fac.hidden, 0);
assert.deepStrictEqual([fac.needsHuman.count, fac.needsHuman.on, fac.onHold.count, fac.onHold.on], [0, false, 0, false], 'synthetic chips always exist; nothing here is human-gated or held');
assert.ok(stageTrackerLabelFacets(items.concat([{ kind: 'issue', repo: 'a/b', number: 7, labels: ['solo'] }]), F()).chips.some(x => x.label === 'solo'), 'single-item labels are offered too (no hidden labels)');
// Over the limit: the row shows the first N in order plus any selected label; all keeps every label and hidden the overflow.
const wide = items.concat(Array.from({ length: 30 }, (_, i) => ({ kind: 'issue', repo: 'a/b', number: 100 + i, labels: ['zz-' + String(i).padStart(2, '0')] })));
fac = stageTrackerLabelFacets(wide, F({ labels: ['zz-29'] }));
assert.strictEqual(fac.chips.length, STAGE_TRACKER_LABEL_CHIP_LIMIT + 1);
assert.strictEqual(fac.chips[fac.chips.length - 1].label, 'zz-29', 'a selected overflow label is promoted into the row, in order');
assert.ok(fac.chips[fac.chips.length - 1].on);
assert.strictEqual(fac.all.length, 40);
assert.strictEqual(fac.hidden, 40 - STAGE_TRACKER_LABEL_CHIP_LIMIT - 1);
let dlg = stageTrackerLabelDialogData(fac, 'ZZ-1');
assert.deepStrictEqual(dlg.rows.map(x => x.label), ['zz-10', 'zz-11', 'zz-12', 'zz-13', 'zz-14', 'zz-15', 'zz-16', 'zz-17', 'zz-18', 'zz-19', 'zz-29'], 'query is a case-insensitive substring; selected labels stay listed');
assert.deepStrictEqual([dlg.total, dlg.selected, dlg.query], [40, 1, 'zz-1']);

// Needs human (hivecommons/hive#15008): stage/modifier predicate shared with the count — labels alone never qualify.
const parked = items.concat([
  { kind: 'issue', repo: 'a/b', number: 20, stage: 45, modifiers: [], labels: ['stage/045-needs-decision'] },
  { kind: 'issue', repo: 'a/b', number: 21, stage: 20, modifiers: ['needs-human'], labels: ['needs-human/agent-ready'] },
  { kind: 'pr', repo: 'a/b', number: 22, stage: 71, author_kind: 'human', modifiers: [], labels: ['stage/071-needs-author', 'author/human'] },
  { kind: 'pr', repo: 'a/b', number: 23, stage: 71, author_kind: 'clanker', modifiers: [], labels: ['stage/071-needs-author'] },
  { kind: 'issue', repo: 'a/b', number: 24, stage: 40, modifiers: [], labels: ['needs-signal'] },
  { kind: 'issue', repo: 'a/b', number: 25, stage: 45, modifiers: ['hold'], labels: ['stage/045-needs-decision', 'hold'] },
  { kind: 'pr', repo: 'a/b', number: 26, stage: 80, modifiers: ['hold'], labels: ['hold'] },
  { kind: 'pr', repo: 'a/b', number: 27, stage: 60, modifiers: ['stalled', 'blocked-on-pr'], labels: [] },
  { kind: 'issue', repo: 'a/b', number: 28, stage: 15, modifiers: [], labels: [] },
  { kind: 'pr', repo: 'a/b', number: 29, stage: 90, modifiers: [], labels: [] },
  { kind: 'pr', repo: 'a/b', number: 30, stage: 50, modifiers: ['unauthorized'], labels: [] },
]);
assert.strictEqual(stageTrackerItemNeedsHuman(parked.find(x => x.number === 24)), false, '040 "PR in progress" needs no human (#14976)');
assert.strictEqual(stageTrackerItemNeedsHuman(parked.find(x => x.number === 23)), false, '071 with a clanker author is the bot\'s turn');
assert.strictEqual(stageTrackerItemNeedsHuman(parked.find(x => x.number === 25)), false, 'hold wins: a held row is paused, not waiting');
assert.strictEqual(stageTrackerItemOnHold(parked.find(x => x.number === 25)), true);
fac = stageTrackerLabelFacets(parked, F());
assert.deepStrictEqual([fac.needsHuman.count, fac.needsHuman.on, fac.onHold.count, fac.onHold.on], [6, false, 2, false]);
assert.deepStrictEqual(nums(stageTrackerVisible(parked, F({ needsHuman: true }), ctx)), [20, 21, 22, 28, 29, 30], 'filter and chip count agree');
assert.deepStrictEqual(nums(stageTrackerVisible(parked, F({ onHold: true }), ctx)), [25, 26]);
assert.deepStrictEqual(nums(stageTrackerVisible(parked, F({ needsHuman: true, labels: ['author/human'] }), ctx)), [22], 'needs-human ANDs with label chips');
assert.strictEqual(stageTrackerFilterActive(F({ needsHuman: true })), true);
assert.strictEqual(stageTrackerFilterActive(F({ onHold: true })), true);
assert.strictEqual(stageTrackerNormalizeFilter({ needsHuman: 'yes' }).needsHuman, true);
assert.strictEqual(stageTrackerNormalizeFilter({ onHold: 1 }).onHold, true);
assert.strictEqual(stageTrackerLabelDialogData(stageTrackerLabelFacets(parked, F({ needsHuman: true, onHold: true }))).selected, 2);
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node Hive activity monitor fixture failed: %v\n%s", err, out)
	}
}
