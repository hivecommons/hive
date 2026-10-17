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
		jsConstLine(t, html, "STAGE_TRACKER_MOD_TEXT"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTOR"),
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL"),
		jsConstLine(t, html, "STAGE_TRACKER_TRAIL_HOPS"),
		jsConstLine(t, html, "STAGE_TRACKER_MOD_PRIORITY"),
		jsConstBlock(t, html, "STAGE_TRACKER_ICONS"),
		"var _stageTrackerFilter = { kind: 'all', repo: '' };",
		jsFunc(t, html, "stageTrackerIcon"),
		jsFunc(t, html, "stageTrackerPRRef"),
		jsFunc(t, html, "stageTrackerActorKind"),
		jsFunc(t, html, "stageTrackerHeaderHtml"),
		jsFunc(t, html, "stageTrackerStripItems"),
		jsFunc(t, html, "stageTrackerTooltip"),
		jsFunc(t, html, "stageTrackerChipsHtml"),
		jsFunc(t, html, "stageTrackerHeadColsHtml"),
		jsFunc(t, html, "stageTrackerSinceHtml"),
		jsFunc(t, html, "stageTrackerSubRowHtml"),
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
  { number: 50, label: 'Review pending', short: 'CI / Review' }, { number: 60, label: 'Reviewed', short: 'Reviewed' },
  { number: 70, label: 'Fixing', short: 'Fixing', branch_from: 50 }, { number: 72, label: 'Returned to contributor', short: 'Returned to contributor', branch_from: 50 },
  { number: 74, label: 'Adopted by hive', short: 'Adopted by hive', branch_from: 50 },
  { number: 80, label: 'Awaiting merge admission', short: 'Merge gate' },
  { number: 85, label: 'Needs human decision', short: 'Needs human decision', branch_from: 80 }, { number: 90, label: 'Ready for human merge', short: 'Ready for human merge', branch_from: 80 },
  { number: 100, label: 'Merge queue', short: 'Merge queue' }, { number: 110, label: 'Merged', short: 'Merged' },
];
// needs-rebase always lands a PR in 070/072 (automerge_stage_labels.go), so a
// fixture pairing it with 050/060 would draw a contradictory row.
const rebaseCoherent = (it) => { if ((it.modifiers || []).includes('needs-rebase')) assert.ok(![50, 60].includes(Number(it.stage)), 'fixture #' + it.number + ' pairs needs-rebase with stage ' + it.stage); (it.linked_prs || []).forEach(rebaseCoherent); };
const rawRowHtml = stageTrackerRowHtml;
stageTrackerRowHtml = (item, stages, ctx) => { rebaseCoherent(item); return rawRowHtml(item, stages, ctx); };
// A row is a title line, its strip, then zero or more nested PR sub-rows.
const strips = (row) => row.split('aria-label="Pipeline progress"').slice(1);
const strip = (row) => strips(row)[0].split('<div class="ham-sub"')[0];
const subs = (row) => row.split('<div class="ham-sub"').slice(1);
const segStates = (html) => Array.from(strip(html).matchAll(/<li class="ham-seg ([^"]*)"/g)).map(m => m[1]);
const prCol = (html) => Array.from(html.slice(0, html.indexOf('aria-label="Pipeline progress"')).matchAll(/<a class="ham-pr-ref"[^>]*>(.*?)<\/a>/g)).map(m => m[1]);
const B = 'blank';
const text = (h) => h.replace(/<[^>]+>/g, '').replace(/&#39;/g, "'").replace(/\s+/g, ' ').trim();
const tooltip = (row) => /<div class="ham-row"[^>]*title="([^"]*)"/.exec(row)[1];
const chips = (row) => (row.match(/class="ham-badge /g) || []).length;
const ctx = { nowMs: now };
const D = 'done', T = 'todo';

// Issue claimed 30 min ago: Open + Actionable done, Claimed active, five ahead, cap todo.
let row = stageTrackerRowHtml({ repo: 'acme/console', number: 7, kind: 'issue', title: 'Fix <login>', url: 'https://x/7', stage: 20, stage_since: new Date(now - 30*60000).toISOString(), caption: 'Claimed by scanner', author: 'alice', author_kind: 'human', author_kind_source: 'label' }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, 'active', T, T, T, 'todo cap'], 'an issue with no PR yet draws the whole strip ahead');
assert.ok(row.includes('aria-current="step"') && subs(row).length === 0);
assert.ok(row.includes('Fix &lt;login&gt;'), 'title escaped');
assert.strictEqual(prCol(row).length, 0, 'issue without a PR has an empty PR column');
assert.ok(tooltip(row).includes('(author/human'), 'author kind stays in the tooltip');
assert.ok(row.includes('<span class="ham-since">30m</span>'), row);
assert.ok(row.indexOf('<span class="ham-since">30m</span>') < row.indexOf('aria-label="Pipeline progress"'), 'since sits on the title line, not the strip row');
assert.ok(/aria-label="Pipeline progress">[\s\S]*?<\/ol>\s*<span class="ham-since"><\/span>/.test(row), 'the strip row keeps an empty since slot for alignment');
assert.ok(row.includes('ham-kind-ico'), 'kind icon');
assert.strictEqual((strip(row).match(/<svg class="ham-ico"/g) || []).length, 6, 'icons on every drawn non-active chevron');
assert.ok(row.includes('aria-current="step"><span class="ham-seg-t">Claimed</span></li>'), 'active chevron carries words, no icon');
assert.ok(!row.includes('ham-caption'), 'no caption line under the strip');
assert.ok(tooltip(row).includes('Claimed by scanner') && tooltip(row).includes('@alice') && tooltip(row).includes('30 min ago'), tooltip(row));
assert.ok(!row.includes('✓'), 'no text check glyphs');

// Issue whose covering PR is in the merge queue: the issue row rides the PR's stage.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 8, kind: 'issue', url: 'https://github.com/acme/console/issues/8', stage: 100, mirrored: true, linked_pr: 102, caption: 'Covered by PR #102' }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, B, B, B, 'blank cap'], 'a covered issue ends at Claimed, all passed, nothing lit');
assert.ok(!strip(row).includes('aria-current="step"') && !strip(row).includes('ham-actor-ico'), row);
assert.ok(tooltip(row).includes('Admitted — merging (its PR)') && tooltip(row).includes('Covering PR: #102'), tooltip(row));
assert.strictEqual(prCol(row).length, 0, 'the issue title line no longer lists PRs; they nest below');
assert.strictEqual(subs(row).length, 1, 'one sub-row per covering PR, even when only the number is known');
assert.ok(subs(row)[0].includes('#102') && segStates(subs(row)[0]).every(st => st.startsWith('blank') || st === 'todo' || st === 'todo cap'), 'unknown PR stage: blank lead-in, nothing lit');
assert.ok(tooltip(row).includes('acme/console'), 'repo lives in the tooltip');
// Full PR row for 100: Merge gate lit as Ready for auto merge with the hive actor icon.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 102, kind: 'pr', stage: 100, author: 'hive-bot', author_kind: 'hive' }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, 'active', 'todo cap']);
assert.ok(row.includes('aria-current="step"><span class="ham-seg-t">Ready for auto merge</span><svg class="ham-ico ham-actor-ico" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" role="img"><title>Next move: hive</title>' + STAGE_TRACKER_ICONS.hive + '</svg></li>'), '100 lights the Merge gate column as Ready for auto merge');
assert.ok(strip(row).includes('title="Merge queue — admitted, merging"'), 'chevron tooltip carries 100: ' + row);
assert.ok(text(row).includes('#102 Ready for auto merge'), text(row));

// PR returned to contributor: detour badge rides Review; chip cap = author kind + first modifier (+N).
row = stageTrackerRowHtml({ repo: 'acme/console', number: 102, kind: 'pr', title: 'Fix flaky', stage: 72, lane: 'contributor', tier: 'trusted', author: 'karibhou', author_kind: 'clanker', author_kind_source: 'inferred', modifiers: ['needs-rebase', 'needs-human'], caption: 'Returned to @karibhou', history: [{stage:50},{stage:60},{stage:72}] }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, 'active detoured', T, 'todo cap']);
assert.ok(row.includes('<span class="ham-detour">Returned to contributor</span>'));
assert.deepStrictEqual(prCol(row).length, 1, 'a PR row lists itself in the PR column');
assert.ok(prCol(row)[0].includes('ham-author-ico ham-author-clanker') && prCol(row)[0].includes('<span class="ham-pr-num">#102</span><span class="ham-pr-title">Fix flaky</span>'), 'PR row lists icon + id + full title: ' + prCol(row)[0]);
assert.ok(prCol(row)[0].includes('<title>clanker · @karibhou</title>'));
assert.strictEqual(chips(row), 1, 'author kind is an icon, not a chip');
assert.ok(row.includes('ham-mod-needs-human">needs human +1</span>'), row);
assert.ok(tooltip(row).includes('Flags: needs-rebase, needs-human') && tooltip(row).includes('Trail: 050 → 060 → 072'), tooltip(row));

// PR reviewed (060): Review done, Merge gate lit, no detour.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 107, kind: 'pr', stage: 60 }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, 'active', 'todo cap']);
assert.ok(row.includes('aria-current="step"><span class="ham-seg-t">Ready for auto merge</span><svg class="ham-ico ham-actor-ico" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" role="img"><title>Next move: hive</title>' + STAGE_TRACKER_ICONS.hive + '</svg></li>'), '060/080/100 all light Merge gate as Ready for auto merge');
row = stageTrackerRowHtml({ repo: 'acme/console', number: 109, kind: 'pr', stage: 80, caption: 'soak 20m left' }, stages, ctx);
assert.ok(row.includes('<span class="ham-seg-t">Ready for auto merge</span>') && strip(row).includes('title="Awaiting merge admission — not yet admitted — soak 20m left"'), 'chevron tooltip carries 080: ' + row);

// PR regressed from the queue back to review-pending: Review lit, "Sent back" in the tooltip.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 103, kind: 'pr', stage: 70, modifiers: ['regressed', 'needs-rebase'], history: [{stage:80},{stage:100},{stage:70}] }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, 'active detoured', T, 'todo cap']);
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
// 085 has its own word; needs-decision as a modifier elsewhere reads "needs human decision".
row = stageTrackerRowHtml({ repo: 'acme/console', number: 106, kind: 'pr', stage: 85, modifiers: ['blocked'], caption: 'DCO failing — fix, waive or close' }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, 'active detoured', 'todo cap']);
assert.ok(row.includes('<span class="ham-detour">Needs human decision</span>') && !row.includes('ham-mod-needs-decision'), row);
assert.ok(tooltip(row).includes('Needs a human decision'), row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 107, kind: 'pr', stage: 50, modifiers: ['needs-decision'] }, stages, ctx);
assert.ok(row.includes('ham-mod-needs-decision">needs human decision</span>') && tooltip(row).includes('Flags: needs human decision'), row);
// Whose move it is, as an ink-coloured icon after the word: Review = hive,
// PR in progress = the PR author, Claimed = the claimant; Open/Actionable carry none.
assert.ok(row.includes('<svg class="ham-ico ham-actor-ico" viewBox="0 0 16 16"') && row.includes('<title>Next move: hive</title>') && row.includes('<span class="ham-seg-t">CI / Review</span><svg class="ham-ico ham-actor-ico"'), row);
assert.ok(!row.includes('ham-author-ico ham-actor') && !row.includes('ham-actor-ico ham-author-'), 'actor icon must not carry the author colour classes: ' + row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 108, kind: 'pr', stage: 72, author_kind: 'clanker' }, stages, ctx);
assert.ok(row.includes('<title>Next move: clanker</title>') && row.includes('<span class="ham-detour">Returned to contributor</span><svg class="ham-ico ham-actor-ico"'), row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 7, kind: 'issue', stage: 40, linked_prs: [{ number: 108, author_kind: 'clanker', stage: 30 }] }, stages, ctx);
assert.ok(!strip(row).includes('ham-actor-ico') && subs(row)[0].includes('<title>Next move: clanker</title>') && subs(row)[0].includes('<span class="ham-seg-t">PR in progress</span><svg class="ham-ico ham-actor-ico"'), 'the PR sub-row, not the issue, lights PR in progress: ' + row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 8, kind: 'issue', stage: 20, claimant_kind: 'hive' }, stages, ctx);
assert.ok(row.includes('<title>Next move: hive</title>') && row.includes('<span class="ham-seg-t">Claimed</span><svg class="ham-ico ham-actor-ico"'), row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 8, kind: 'issue', stage: 20 }, stages, ctx);
assert.ok(!row.includes('ham-actor-ico') && row.includes('<span class="ham-seg-t">Claimed</span>'), 'no claimant known, no icon: ' + row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 9, kind: 'issue', stage: 10 }, stages, ctx);
assert.ok(!row.includes('ham-actor-ico') && !tooltip(row).includes('Next move'), row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 105, kind: 'pr', stage: 90 }, stages, ctx);
assert.ok(row.includes('<title>Next move: human</title>') && row.includes('<span class="ham-detour">Ready for human merge</span><svg class="ham-ico ham-actor-ico"'), row);

// Merged: everything neutral-done, the cap carries the only check.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 106, kind: 'pr', stage: 110 }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, D, 'active cap merged']);
assert.ok(/<li class="ham-seg active cap merged"[^>]*><svg class="ham-ico"/.test(row), 'merged cap is the lit segment with the check icon');

// Hive-authored rows: hive icon in the PR column, no chips.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 108, kind: 'pr', stage: 50, author: 'hive-bot', author_kind: 'hive' }, stages, ctx);
assert.strictEqual(chips(row), 0);
assert.ok(prCol(row)[0].includes('ham-author-hive'));

// 040 shares the PR in progress column with 030; the chevron tooltip names the real stage.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 9, kind: 'issue', title: 'Ordering drifts', stage: 40, linked_prs: [{ number: 103, title: 'Backport', author_kind: 'clanker', stage: 30 }, { number: 102, title: 'Fix it', url: 'u', author_kind: 'human', stage: 70, stage_since: new Date(now - 3600000).toISOString(), modifiers: ['needs-rebase'], caption: 'Rebasing onto main' }] }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, B, B, B, 'blank cap']);
assert.ok(!strip(row).includes('aria-current'), 'issue strip has no lit chevron once PRs cover it');
assert.ok(row.includes('<span class="ham-title">Ordering drifts</span>'), 'issue keeps its own title on the left');
assert.ok(tooltip(row).includes('Covering PR: #102, #103'));
// Nested sub-rows, furthest along first: "[icon] #N title" at the PR column, then the PR's strip from that column on.
let sr = subs(row);
assert.strictEqual(sr.length, 2, 'one sub-row per covering PR');
assert.ok(prCol(sr[0])[0].includes('ham-author-human') && prCol(sr[0])[0].includes('#102') && prCol(sr[0])[0].includes('<span class="ham-pr-title">Fix it</span>'), prCol(sr[0])[0]);
assert.deepStrictEqual(segStates(sr[0]), [B, B, B, D, 'active detoured', T, 'todo cap'], 'PR strip starts at PR in progress');
assert.ok(sr[0].includes('<span class="ham-detour">Fixing</span><svg class="ham-ico ham-actor-ico"') && sr[0].includes('<title>Next move: human</title>') && sr[0].indexOf('<span class="ham-since">1h</span>') < sr[0].indexOf('aria-label="Pipeline progress"') && sr[0].includes('ham-mod-needs-rebase">needs rebase</span>'), 'Fixing is the author\'s move; the chip says what is being fixed: ' + sr[0]);
assert.ok(/^ data-ham-kind="pr" data-ham-stage="70" title="Fixing review findings Rebasing onto main/.test(sr[0]), 'sub-row carries its own tooltip: ' + sr[0].slice(0, 200));
assert.ok(prCol(sr[1])[0].includes('ham-author-clanker') && prCol(sr[1])[0].includes('#103'));
assert.deepStrictEqual(segStates(sr[1]), [B, B, B, 'active', T, T, 'todo cap'], 'a PR at 030 lights PR in progress on its own sub-row');
assert.ok(!row.slice(0, row.indexOf('<div class="ham-sub"')).includes('ham-pr-ref'), 'the issue title line itself lists no PRs');
row = stageTrackerRowHtml({ repo: 'acme/console', number: 10, kind: 'issue', stage: 30 }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, 'active', T, T, 'todo cap'], 'kick running, no PR yet: the issue lights PR in progress and draws the steps ahead');
assert.ok(row.includes('aria-current="step"><span class="ham-seg-t">PR in progress</span></li>') && strip(row).includes('title="PR in progress'), row);
assert.strictEqual(subs(row).length, 0, 'no PR yet');
// A merged covering PR nests with the green cap lit.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 11, kind: 'issue', stage: 40, linked_prs: [{ number: 120, title: 'Done', author_kind: 'hive', stage: 110 }] }, stages, ctx);
assert.deepStrictEqual(segStates(subs(row)[0]), [B, B, B, D, D, D, 'active cap merged']);

// Header: names once, aligned with the chevrons, plus the since column.
const head = stageTrackerHeaderHtml(stages);
assert.deepStrictEqual(Array.from(head.matchAll(/<span class="ham-seg-t">([^<]*)<\/span>/g)).map(m => m[1]), ['Open', 'Actionable', 'Claimed', 'PR in progress', 'CI / Review', 'Merge gate']);
assert.ok(head.includes('<span class="ham-since">since</span>'));

// Unknown / zero stage lights "Open".
row = stageTrackerRowHtml({ repo: 'acme/console', number: 1, kind: 'issue', stage: 0 }, stages, ctx);
assert.deepStrictEqual(segStates(row), ['active', T, T, T, T, T, 'todo cap']);

// Every PR stage has an active word and an actor on its segment; the word
// is what the chevron says, the actor is whose move it is.
const prWords = {};
for (const [st, word, actor] of [[50, 'CI / Review', 'hive'], [60, 'Ready for auto merge', 'hive'], [70, 'Fixing', 'author'], [72, 'Returned to contributor', 'author'], [74, 'Adopted by hive', 'hive'], [80, 'Ready for auto merge', 'hive'], [85, 'Needs human decision', 'human'], [90, 'Ready for human merge', 'human'], [100, 'Ready for auto merge', 'hive']]) {
  const r = stageTrackerRowHtml({ repo: 'acme/console', number: 500 + st, kind: 'pr', stage: st, author_kind: 'clanker' }, stages, ctx);
  const m = /aria-current="step"><span class="ham-(?:seg-t|detour)">([^<]*)<\/span><svg class="ham-ico ham-actor-ico"[^>]*><title>Next move: ([a-z]+)<\/title>/.exec(r);
  assert.ok(m, 'stage ' + st + ' has no active word + actor: ' + r);
  assert.deepStrictEqual([m[1], m[2]], [word, actor === 'author' ? 'clanker' : actor], 'stage ' + st);
  prWords[st] = m[1];
}
assert.strictEqual(STAGE_TRACKER_ACTOR[70], 'author', '070 fixing is the author\'s move');
// Filters.
const items = [
  { kind: 'pr', repo: 'a/b', stage: 50 }, { kind: 'issue', repo: 'a/c', number: 1, stage: 10 },
  { kind: 'pr', repo: 'a/b', stage: 72 }, { kind: 'pr', repo: 'a/c', number: 9, stage: 80, modifiers: ['stalled'], nested_under: 1 },
  { kind: 'pr', repo: 'a/c', number: 10, stage: 80, nested_under: 2 },
];
// All / Issues nest a PR under its listed issue; Pull requests flattens; a PR whose issue is not listed stands alone.
assert.deepStrictEqual(stageTrackerVisible(items, { kind: 'all', repo: '' }).map(it => it.number), [undefined, 1, undefined, 10]);
assert.deepStrictEqual(stageTrackerVisible(items, { kind: 'issue', repo: '' }).map(it => it.number), [1]);
assert.strictEqual(stageTrackerVisible(items, { kind: 'pr', repo: '' }).length, 4);
assert.strictEqual(stageTrackerVisible(items).length, 4, 'defaults to the live filter');
_stageTrackerFilter.kind = 'pr'; assert.strictEqual(stageTrackerVisible(items).length, 4);
_stageTrackerFilter.repo = 'a/c'; assert.strictEqual(stageTrackerVisible(items).length, 2);
assert.strictEqual(items.filter(stageTrackerNeedsAttention).length, 2);
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node Hive activity monitor fixture failed: %v\n%s", err, out)
	}
}
