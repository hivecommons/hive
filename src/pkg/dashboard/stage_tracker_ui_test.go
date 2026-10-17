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
		`id="activity-monitor-repo"`,
		`id="activity-monitor-sidebar-badge"`,
		`'activity-monitor-section': { title: '📍 Hive activity monitor'`,
		`'activity-monitor-section': { heading: 'Hive activity monitor'`,
		`'activity-monitor-section', 'agents-section', 'agent-activity-section', 'faq-section'`,
		`fetch('/api/stage-tracker')`,
		`dashboardSetInterval('stage-tracker', fetchStageTracker, STAGE_TRACKER_POLL_MS)`,
		`function stageTrackerRowHtml(item, stages, ctx)`,
		`function stageTrackerHeaderHtml(stages)`,
		`const STAGE_TRACKER_ICONS = {`,
		`@keyframes ham-pulse`,
		`animation: ham-pulse 1.6s ease-in-out infinite;`,
		`@media (prefers-reduced-motion: reduce) { .ham-seg.active { animation: none;`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing Hive activity monitor wiring %q", want)
		}
	}
	if strings.Contains(jsConstBlock(t, html, "DASHBOARD_DEFAULT_COLLAPSED_SECTIONS"), "'activity-monitor-section'") {
		t.Fatalf("activity-monitor-section must be expanded for a new browser: it answers 'is the hive moving?'")
	}
}

// TestStageTrackerRows executes the pure row renderer under node and checks
// the unified strip geometry (Open → 010 → 020 → 030 → 040 → 050 → 080 → 100 →
// merged cap): which segments are done / active / detoured, the tooltip and
// the chip cap, for issues and PRs alike.
func TestStageTrackerRows(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — Hive activity monitor rows were NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"const window = {};",
		"function formatDashboardTime(d){return new Date(d).toISOString().slice(11,16)+' UTC';}",
		"function formatDashboardDate(d){return new Date(d).toISOString().slice(0,10);}",
		"function formatDashboardDateTime(d){return new Date(d).toISOString().slice(0,16);}",
		jsConstLine(t, html, "STAGE_TRACKER_PATH"),
		jsConstLine(t, html, "STAGE_TRACKER_PR_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTIVE_WORD"),
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL"),
		jsConstLine(t, html, "STAGE_TRACKER_TRAIL_HOPS"),
		jsConstLine(t, html, "STAGE_TRACKER_MOD_PRIORITY"),
		jsConstBlock(t, html, "STAGE_TRACKER_ICONS"),
		"var _stageTrackerFilter = { kind: 'all', repo: '' };",
		jsFunc(t, html, "stageTrackerIcon"),
		jsFunc(t, html, "stageTrackerPRRef"),
		jsFunc(t, html, "stageTrackerHeaderHtml"),
		jsFunc(t, html, "esc"),
		jsFunc(t, html, "agentNowAgo"),
		jsFunc(t, html, "agentNowClockTime"),
		jsFunc(t, html, "stageTrackerHeadline"),
		jsFunc(t, html, "stageTrackerAnchor"),
		jsFunc(t, html, "stageTrackerSegments"),
		jsFunc(t, html, "stageTrackerShortAgo"),
		jsFunc(t, html, "stageTrackerRowHtml"),
		jsFunc(t, html, "stageTrackerNeedsAttention"),
		jsFunc(t, html, "stageTrackerVisible"),
	}, "\n") + `
const assert = require('assert');
const now = Date.parse('2026-01-01T12:00:00Z');
const stages = [
  { number: 10, label: 'Actionable', short: 'Actionable', description: 'Ready to be picked up' },
  { number: 20, label: 'Claimed', short: 'Claimed' }, { number: 30, label: 'PR in progress', short: 'PR in progress' },
  { number: 40, label: 'Covered by PR', short: 'Covered by PR' },
  { number: 50, label: 'Review pending', short: 'Review' }, { number: 60, label: 'Reviewed', short: 'Reviewed' },
  { number: 70, label: 'Fixing', short: 'Fixing', branch_from: 50 }, { number: 72, label: 'Returned to contributor', short: 'Returned to contributor', branch_from: 50 },
  { number: 74, label: 'Adopted by hive', short: 'Adopted by hive', branch_from: 50 },
  { number: 80, label: 'Awaiting merge admission', short: 'Merge gate' },
  { number: 85, label: 'Needs decision', short: 'Needs decision', branch_from: 80 }, { number: 90, label: 'Ready for human merge', short: 'Ready for human merge', branch_from: 80 },
  { number: 100, label: 'Merge queue', short: 'Merge queue' }, { number: 110, label: 'Merged', short: 'Merged' },
];
const strip = (row) => row.slice(row.indexOf('aria-label="Pipeline progress"'));
const segStates = (row) => Array.from(strip(row).matchAll(/<li class="ham-seg ([^"]*)"/g)).map(m => m[1]);
const prCol = (row) => Array.from(row.slice(0, row.indexOf('aria-label="Pipeline progress"')).matchAll(/<a class="ham-pr-ref"[^>]*>(.*?)<\/a>/g)).map(m => m[1]);
const text = (h) => h.replace(/<[^>]+>/g, '').replace(/&#39;/g, "'").replace(/\s+/g, ' ').trim();
const tooltip = (row) => /<div class="ham-row"[^>]*title="([^"]*)"/.exec(row)[1];
const chips = (row) => (row.match(/class="ham-badge /g) || []).length;
const ctx = { nowMs: now };
const D = 'done', T = 'todo';

// Issue claimed 30 min ago: Open + Actionable done, Claimed active, five ahead, cap todo.
let row = stageTrackerRowHtml({ repo: 'acme/console', number: 7, kind: 'issue', title: 'Fix <login>', url: 'https://x/7', stage: 20, stage_since: new Date(now - 30*60000).toISOString(), caption: 'Claimed by scanner', author: 'alice', author_kind: 'human', author_kind_source: 'label' }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, 'active', T, T, T, 'todo cap']);
assert.ok(row.includes('aria-current="step"'));
assert.ok(row.includes('Fix &lt;login&gt;'), 'title escaped');
assert.strictEqual(prCol(row).length, 0, 'issue without a PR has an empty PR column');
assert.ok(tooltip(row).includes('(author/human'), 'author kind stays in the tooltip');
assert.ok(row.includes('<span class="ham-since">30m</span>'), row);
assert.ok(row.includes('ham-kind-ico'), 'kind icon');
assert.strictEqual((strip(row).match(/<svg class="ham-ico"/g) || []).length, 6, 'icons on every non-active chevron incl. cap');
assert.ok(row.includes('aria-current="step"><span class="ham-seg-t">Claimed</span></li>'), 'active chevron carries words, no icon');
assert.ok(!row.includes('ham-caption'), 'no caption line under the strip');
assert.ok(tooltip(row).includes('Claimed by scanner') && tooltip(row).includes('@alice') && tooltip(row).includes('30 min ago'), tooltip(row));
assert.ok(!row.includes('✓'), 'no text check glyphs');

// Issue whose covering PR is in the merge queue: the issue row rides the PR's stage.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 8, kind: 'issue', url: 'https://github.com/acme/console/issues/8', stage: 100, mirrored: true, linked_pr: 102, caption: 'Covered by PR #102' }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, 'active', 'todo cap']);
assert.ok(tooltip(row).includes('Admitted — merging (its PR)') && tooltip(row).includes('Covering PR: #102'), tooltip(row));
assert.ok(row.includes('aria-current="step"><span class="ham-seg-t">Auto merge queue</span></li>'), '100 lights the Merge gate column as Auto merge queue');
assert.ok(strip(row).includes('title="Merge queue — admitted, merging"'), 'chevron tooltip carries 100: ' + row);
assert.ok(text(row).startsWith('hive#102'), text(row));
assert.ok(tooltip(row).includes('acme/console'), 'repo lives in the tooltip');

// PR returned to contributor: detour badge rides Review; chip cap = author kind + first modifier (+N).
row = stageTrackerRowHtml({ repo: 'acme/console', number: 102, kind: 'pr', title: 'Fix flaky', stage: 72, lane: 'contributor', tier: 'trusted', author: 'karibhou', author_kind: 'clanker', author_kind_source: 'inferred', modifiers: ['needs-rebase', 'needs-human'], caption: 'Returned to @karibhou', history: [{stage:50},{stage:60},{stage:72}] }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, 'active detoured', T, 'todo cap']);
assert.ok(row.includes('<span class="ham-detour">Returned to contributor</span>'));
assert.deepStrictEqual(prCol(row).length, 1, 'a PR row lists itself in the PR column');
assert.ok(prCol(row)[0].includes('ham-author-ico ham-author-clanker') && prCol(row)[0].includes('<span class="ham-pr-num">#102</span><span class="ham-pr-title">Fix flaky</span>'), 'PR row lists icon + id + full title: ' + prCol(row)[0]);
assert.ok(prCol(row)[0].includes('<title>clanker · @karibhou</title>'));
assert.strictEqual(chips(row), 1, 'author kind is an icon, not a chip');
assert.ok(row.includes('ham-mod-needs-human">needs human +1</span>'), row);
assert.ok(tooltip(row).includes('Flags: needs-human, needs-rebase') && tooltip(row).includes('Trail: 050 → 060 → 072'), tooltip(row));

// PR reviewed (060): Review done, Merge gate lit, no detour.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 107, kind: 'pr', stage: 60 }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, 'active', 'todo cap']);
assert.ok(row.includes('aria-current="step"><span class="ham-seg-t">Auto merge queue</span></li>'), '060/080/100 all light Merge gate as Auto merge queue');
row = stageTrackerRowHtml({ repo: 'acme/console', number: 109, kind: 'pr', stage: 80, caption: 'soak 20m left' }, stages, ctx);
assert.ok(row.includes('<span class="ham-seg-t">Auto merge queue</span>') && strip(row).includes('title="Awaiting merge admission — not yet admitted — soak 20m left"'), 'chevron tooltip carries 080: ' + row);

// PR regressed from the queue back to review-pending: Review lit, "Sent back" in the tooltip.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 103, kind: 'pr', stage: 50, modifiers: ['regressed', 'needs-rebase'], history: [{stage:80},{stage:100},{stage:50}] }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, 'active', T, 'todo cap']);
assert.ok(tooltip(row).startsWith('Sent back'));
assert.ok(row.includes('ham-mod-regressed'), 'regressed outranks needs-rebase for the single chip');

// Stalled PR awaiting admission: since text turns red.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 104, kind: 'pr', stage: 80, stage_since: new Date(now - 3*3600000).toISOString(), modifiers: ['stalled'] }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, 'active', 'todo cap']);
assert.ok(row.includes('<span class="ham-since ham-stalled">3h</span>'), row);

// Human-merge detour off Merge gate.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 105, kind: 'pr', stage: 90 }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, 'active detoured', 'todo cap']);
assert.ok(row.includes('<span class="ham-detour">Ready for human merge</span>'));

// Merged: everything neutral-done, the cap carries the only check.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 106, kind: 'pr', stage: 110 }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, D, 'active cap merged']);
assert.ok(/<li class="ham-seg active cap merged"[^>]*><svg class="ham-ico"/.test(row), 'merged cap is the lit segment with the check icon');

// Hive-authored rows: hive icon in the PR column, no chips.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 108, kind: 'pr', stage: 50, author: 'hive-bot', author_kind: 'hive' }, stages, ctx);
assert.strictEqual(chips(row), 0);
assert.ok(prCol(row)[0].includes('ham-author-hive'));

// 040 shares the PR in progress column with 030; the chevron tooltip names the real stage.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 9, kind: 'issue', title: 'Ordering drifts', stage: 40, linked_prs: [{ number: 102, title: 'Fix it', url: 'u', author_kind: 'human', stage: 50 }, { number: 103, title: 'Backport', author_kind: 'clanker' }] }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, 'active', T, T, 'todo cap']);
assert.ok(row.includes('aria-current="step"><span class="ham-seg-t">PR in progress</span></li>'), 'column name, not a detour');
assert.ok(strip(row).includes('title="Covered by PR'), 'tooltip distinguishes 040');
assert.ok(row.includes('<span class="ham-title">Ordering drifts</span>'), 'issue keeps its own title on the left');
assert.deepStrictEqual(prCol(row).length, 2, 'issue lists every covering PR, stacked');
assert.ok(prCol(row)[0].includes('ham-author-human') && prCol(row)[0].includes('#102') && prCol(row)[0].includes('<span class="ham-pr-title">Fix it</span>'), prCol(row)[0]);
assert.ok(prCol(row)[1].includes('ham-author-clanker') && prCol(row)[1].includes('#103'));
assert.ok(tooltip(row).includes('Covering PR: #102, #103'));
row = stageTrackerRowHtml({ repo: 'acme/console', number: 10, kind: 'issue', stage: 30 }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, 'active', T, T, 'todo cap']);
assert.strictEqual(prCol(row).length, 0, 'no PR yet');

// Header: names once, aligned with the chevrons, plus the since column.
const head = stageTrackerHeaderHtml(stages);
assert.deepStrictEqual(Array.from(head.matchAll(/<span class="ham-seg-t">([^<]*)<\/span>/g)).map(m => m[1]), ['Open', 'Actionable', 'Claimed', 'PR in progress', 'Review', 'Merge gate']);
assert.ok(head.includes('<span class="ham-since">since</span>'));

// Unknown / zero stage lights "Open".
row = stageTrackerRowHtml({ repo: 'acme/console', number: 1, kind: 'issue', stage: 0 }, stages, ctx);
assert.deepStrictEqual(segStates(row), ['active', T, T, T, T, T, 'todo cap']);

// Filters.
const items = [
  { kind: 'pr', repo: 'a/b', stage: 50 }, { kind: 'issue', repo: 'a/c', stage: 10 },
  { kind: 'pr', repo: 'a/b', stage: 72 }, { kind: 'pr', repo: 'a/c', stage: 80, modifiers: ['stalled'] },
];
assert.strictEqual(stageTrackerVisible(items).length, 4);
_stageTrackerFilter.kind = 'pr'; assert.strictEqual(stageTrackerVisible(items).length, 3);
_stageTrackerFilter.repo = 'a/c'; assert.strictEqual(stageTrackerVisible(items).length, 1);
assert.strictEqual(items.filter(stageTrackerNeedsAttention).length, 2);
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node Hive activity monitor fixture failed: %v\n%s", err, out)
	}
}
