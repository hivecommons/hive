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
		`if (r.status === 304) { stageTrackerTickClock(); return; }`,
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
		`animation: ham-pulse 1.6s ease-in-out infinite;`,
		`.ham-seg.active.ham-open-hold`,
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
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_DECISION"),
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_NEEDS_DECISION"),
		jsConstLine(t, html, "STAGE_TRACKER_CLOSED_ISSUE_TITLE"),
		jsConstLine(t, html, "STAGE_TRACKER_UNTITLED_ISSUE"),
		jsConstLine(t, html, "STAGE_TRACKER_TRIAGE_NEEDED"),
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_DECISION_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_PRE_MERGE_SOAK"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTIVE_WORD"),
		jsConstLine(t, html, "STAGE_TRACKER_MOD_TEXT"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTOR"),
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL"),
		jsConstLine(t, html, "STAGE_TRACKER_TRAIL_HOPS"),
		jsConstLine(t, html, "STAGE_TRACKER_MOD_PRIORITY"),
		jsConstBlock(t, html, "STAGE_TRACKER_ICONS"),
		jsConstLine(t, html, "STAGE_TRACKER_PAGE_SIZES"),
		jsConstLine(t, html, "STAGE_TRACKER_DEFAULT_PAGE_SIZE"),
		jsConstLine(t, html, "STAGE_TRACKER_LABEL_CHIP_LIMIT"),
		jsConstLine(t, html, "STAGE_TRACKER_AGE_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_ATTENTION_STAGES"),
		jsConstLine(t, html, "STAGE_TRACKER_LABEL_FAMILIES"),
		jsConstLine(t, html, "STAGE_TRACKER_SORTS"),
		jsFunc(t, html, "stageTrackerDefaultFilter"),
		"var _stageTrackerFilter = stageTrackerDefaultFilter();",
		jsFunc(t, html, "stageTrackerWaitSince"),
		jsFunc(t, html, "stageTrackerLabelFamily"),
		jsFunc(t, html, "stageTrackerNormalizeFilter"),
		jsFunc(t, html, "stageTrackerFilterActive"),
		jsFunc(t, html, "stageTrackerMatches"),
		jsFunc(t, html, "stageTrackerSort"),
		jsFunc(t, html, "stageTrackerPage"),
		jsFunc(t, html, "stageTrackerLabelFacets"),
		jsFunc(t, html, "stageTrackerCountsText"),
		jsFunc(t, html, "stageTrackerIcon"),
		jsFunc(t, html, "stageTrackerPRRef"),
		jsFunc(t, html, "stageTrackerActorKind"),
		jsFunc(t, html, "stageTrackerHeaderHtml"),
		jsFunc(t, html, "stageTrackerHoldLabels"),
		jsFunc(t, html, "stageTrackerSoakLeft"),
		jsFunc(t, html, "stageTrackerStripItems"),
		jsFunc(t, html, "stageTrackerCurrentStageHtml"),
		jsFunc(t, html, "stageTrackerTooltip"),
		jsFunc(t, html, "stageTrackerChipsHtml"),
		jsFunc(t, html, "stageTrackerHeadColsHtml"),
		jsFunc(t, html, "stageTrackerSinceHtml"),
		jsFunc(t, html, "stageTrackerSubRowHtml"),
		jsFunc(t, html, "stageTrackerLinkedSorted"),
		jsFunc(t, html, "stageTrackerSortKey"),
		jsFunc(t, html, "esc"),
		jsFunc(t, html, "agentNowAgo"),
		jsFunc(t, html, "agentNowClockTime"),
		jsFunc(t, html, "stageTrackerHeadline"),
		jsFunc(t, html, "stageTrackerAnchor"),
		jsFunc(t, html, "stageTrackerSegments"),
		jsFunc(t, html, "stageTrackerShortAgo"),
		jsFunc(t, html, "stageTrackerRowHtml"),
		jsFunc(t, html, "stageTrackerPRIssueSlot"),
		jsFunc(t, html, "stageTrackerItemKey"),
		jsFunc(t, html, "stageTrackerMoveAttrs"),
		jsFunc(t, html, "stageTrackerNeedsAttention"),
		jsFunc(t, html, "stageTrackerVisible"),
	}, "\n") + `
const assert = require('assert');
const now = Date.parse('2026-01-01T12:00:00Z');
const stages = [
  { number: 10, label: 'Actionable', short: 'Actionable', description: 'Ready to be picked up' },
  { number: 15, label: 'Triage needed', short: 'Triage needed', description: 'Actionable but no maintainer has looked at it yet' },
  { number: 20, label: 'Claimed', short: 'Claimed' }, { number: 30, label: 'PR in progress', short: 'PR in progress' },
  { number: 40, label: 'Covered by PR', short: 'Covered by PR' },
  { number: 45, label: 'Needs human decision', short: 'Needs human decision' },
  { number: 50, label: 'Review pending', short: 'CI / Review' }, { number: 60, label: 'Reviewed', short: 'Reviewed' },
  { number: 70, label: 'Fixing', short: 'Fixing', branch_from: 50 }, { number: 71, label: 'Needs author', short: 'Needs author', branch_from: 50 },
  { number: 72, label: 'Returned to contributor', short: 'Returned to contributor', branch_from: 50 },
  { number: 74, label: 'Adopted by hive', short: 'Adopted by hive', branch_from: 50 },
  { number: 80, label: 'Awaiting merge admission', short: 'Merge gate' },
  { number: 85, label: 'Needs human decision', short: 'Needs human decision', branch_from: 80 }, { number: 90, label: 'Ready for human merge', short: 'Ready for human merge', branch_from: 80 },
  { number: 95, label: 'Pre-merge soak', short: 'Soaking', description: 'Lane admitted; the pre-merge soak window is running before the merge is attempted' },
  { number: 100, label: 'Merge queue', short: 'Merge queue' }, { number: 110, label: 'Merged', short: 'Merged' },
];
// needs-rebase always lands a PR in 070/071/072 (automerge_stage_labels.go), so a
// fixture pairing it with 050/060 would draw a contradictory row.
const rebaseCoherent = (it) => { if ((it.modifiers || []).includes('needs-rebase')) assert.ok(![50, 60].includes(Number(it.stage)), 'fixture #' + it.number + ' pairs needs-rebase with stage ' + it.stage); (it.linked_prs || []).forEach(rebaseCoherent); };
const rawRowHtml = stageTrackerRowHtml;
stageTrackerRowHtml = (item, stages, ctx) => { rebaseCoherent(item); return rawRowHtml(item, stages, ctx); };
// A row is a title line, its strip, then zero or more nested PR sub-rows.
const strips = (row) => row.split('aria-label="Pipeline progress"').slice(1);
const strip = (row) => strips(row)[0].split('<div class="ham-sub"')[0];
const subs = (row) => row.split('<div class="ham-sub"').slice(1);
const segStates = (html) => Array.from(strip(html).matchAll(/<li class="ham-seg ([^"]*)"/g)).map(m => m[1]);
// Visible title occurrences (element text only; title="" attrs excluded).
const titles = (html, t) => (html.match(new RegExp('>' + t.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '<', 'g')) || []).length;
const prCol = (html) => Array.from(html.slice(0, html.indexOf('aria-label="Pipeline progress"')).matchAll(/<a class="ham-pr-ref"[^>]*>(.*?)<\/a>/g)).map(m => m[1]);
const B = 'blank';
const text = (h) => h.replace(/<[^>]+>/g, '').replace(/&#39;/g, "'").replace(/\s+/g, ' ').trim();
// The row tooltip rides the issue's #N link on an issue row and the PR link
// (after "PR title · ") on a PR row; the issue slot of a PR row carries none.
const tooltip = (row) => (/<a class="ham-ref"[^>]*title="([^"]*)"/.exec(row) || /<a class="ham-pr-ref"[^>]*title="([^"]*)"/.exec(row))[1];
// Issue slot (lead cell) of a row: its element text.
const lead = (row) => text(/<li class="ham-seg ham-head-lead[^"]*">([\s\S]*?)<\/li>/.exec(row)[1]);
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
assert.ok(tooltip(row).includes('Claimed by scanner') && tooltip(row).includes('@alice') && tooltip(row).includes('30 min ago'), tooltip(row));
assert.ok(!row.includes('✓'), 'no text check glyphs');

// Open but held: the Open segment itself names the hold state and the row
// tooltip names the matching hold label, while the trailing flag pill remains.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 9, kind: 'pr', title: 'Paused', stage: 0, caption: 'No stage recorded yet', labels: ['do-not-merge/hold'], modifiers: ['hold'] }, stages, ctx);
assert.deepStrictEqual(segStates(row), ['active ham-open-hold', T, T, T, T, T, 'todo cap']);
assert.ok(row.includes('ham-mod-hold">hold</span>'), row);
assert.ok(tooltip(row).includes('Open · on hold (do-not-merge/hold) · No stage recorded yet · acme/console'), tooltip(row));

// Open and awaiting triage: the action chip reads "triage needed"; the Open
// column itself is untouched (owner: the Open column keeps its behaviour —
// "triage needed" is the 015 stage's word in the Actionable column).
row = stageTrackerRowHtml({ repo: 'acme/console', number: 11, kind: 'issue', title: 'New report', stage: 0, caption: 'No stage recorded yet', labels: ['needs-triage'], modifiers: ['needs-triage'] }, stages, ctx);
assert.deepStrictEqual(segStates(row), ['active', T, T, T, T, T, 'todo cap']);
assert.ok(row.includes('ham-mod-needs-triage">triage needed</span>'), row);
assert.ok(/aria-current="step"><svg class="ham-ico ham-active-ico"[^>]*>[\s\S]*?<\/svg><span class="ham-seg-t">Open<\/span><\/li>/.test(row), row);
assert.ok(tooltip(row).startsWith('Open No stage recorded yet') && tooltip(row).includes('Flags: triage needed'), tooltip(row));

// 015 triage-needed: the Actionable column lights (not a detour) with the
// word "Triage needed" and a human (maintainer) next move; the headline and
// chevron tooltip name the stage; the server already dropped the chip.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 13262, kind: 'issue', title: 'Review backlog (security)', stage: 15, author: 'hive-bot', author_kind: 'hive', caption: 'Actionable, but no maintainer has looked at it yet; waiting for triage' }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, 'active', T, T, T, T, 'todo cap'], '015 lights the Actionable column: ' + row);
assert.ok(/aria-current="step"><svg class="ham-ico ham-active-ico"[^>]*>[\s\S]*?<\/svg><span class="ham-seg-t">Triage needed<\/span><svg class="ham-ico ham-actor-ico"/.test(row) && row.includes('aria-label="Next move: human"'), '015 word + maintainer move: ' + row);
assert.ok(!row.includes('ham-detour'), '015 is not a detour');
assert.ok(strip(row).includes('title="Triage needed — Actionable but no maintainer has looked at it yet · Next move: human"'), 'chevron tooltip names 015: ' + row);
assert.ok(tooltip(row).startsWith('Triage needed Actionable, but no maintainer has looked at it yet; waiting for triage'), tooltip(row));
assert.ok(text(row).includes('#13262 Review backlog (security) Triage needed'), text(row));
assert.strictEqual(stageTrackerHeadline({ stage: 15, kind: 'issue' }), 'Triage needed');
assert.strictEqual(stageTrackerAnchor(15, stages, 'issue'), 10, '015 anchors on the Actionable column');
assert.ok(!stageTrackerNeedsAttention({ kind: 'issue', stage: 15 }), '015 is a tracking state, not an attention flag');
// A plain 010 row is unchanged by the new stage.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 12999, kind: 'issue', title: 'Epic', stage: 10 }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, 'active', T, T, T, T, 'todo cap']);
assert.ok(/aria-current="step"><svg class="ham-ico ham-active-ico"[^>]*>[\s\S]*?<\/svg><span class="ham-seg-t">Actionable<\/span><\/li>/.test(row), row);

// Issue with exactly one covering PR (only its number known, legacy linked_pr):
// a single entry whose strip is driven by the PR; with no PR stage it lights
// PR in progress.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 8, kind: 'issue', url: 'https://github.com/acme/console/issues/8', stage: 100, mirrored: true, linked_pr: 102, caption: 'Covered by PR #102' }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, 'active', T, T, 'todo cap'], 'one covering PR: single entry');
assert.ok(tooltip(row).includes('Covering PR: #102'), tooltip(row));
assert.deepStrictEqual(prCol(row).length, 1, 'the covering PR sits in the PR column of the title line');
assert.strictEqual(subs(row).length, 0, 'no sub-rows for a single covering PR');
// With the PR's stage known the issue strip rides it (merge queue → Merge gate lit, hive move).
row = stageTrackerRowHtml({ repo: 'acme/console', number: 8, kind: 'issue', stage: 40, linked_prs: [{ number: 102, title: 'Fix it', author_kind: 'human', author: 'bob', stage: 100, stage_since: new Date(now - 7200000).toISOString(), modifiers: ['stalled'], caption: 'Admitted, merging' }] }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, 'active', 'todo cap']);
assert.ok(row.includes('<span class="ham-seg-t">Ready for auto merge</span><svg class="ham-ico ham-actor-ico"') && row.includes('aria-label="Next move: hive"'), row);
assert.ok(row.includes('ham-mod-stalled">stalled</span>') && row.includes('<span class="ham-since ham-stalled">2h</span>'), 'chips + since come from the covering PR: ' + row);
assert.ok(tooltip(row).includes('Admitted, merging') && tooltip(row).includes('Covering PR: #102'), tooltip(row));
assert.ok(prCol(row)[0].includes('ham-author-human') && prCol(row)[0].includes('<span class="ham-pr-title">Fix it</span>'), prCol(row)[0]);
assert.strictEqual(subs(row).length, 0);
// Two covering PRs, one of unknown stage: that sub-row is a blank lead-in with nothing lit.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 8, kind: 'issue', stage: 40, linked_prs: [{ number: 102, stage: 100 }, { number: 104 }] }, stages, ctx);
assert.ok(subs(row)[1].includes('#104') && segStates(subs(row)[1]).every(st => st.startsWith('blank') || st === 'todo' || st === 'todo cap'), 'unknown PR stage: blank lead-in, nothing lit');
assert.ok(tooltip(row).includes('acme/console'), 'repo lives in the tooltip');
// Full PR row for 100: Merge gate lit as Ready for auto merge with the hive actor icon.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 102, kind: 'pr', stage: 100, author: 'hive-bot', author_kind: 'hive' }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, 'active', 'todo cap']);
assert.ok(strip(row).includes('title="Merge queue — admitted, merging · Next move: hive"'), 'chevron tooltip carries 100: ' + row);
assert.ok(text(row).includes('#102 Ready for auto merge'), text(row));
// Full PR row for 095: Merge gate lit as Soaking (not a detour), hive actor,
// chevron tooltip names the real stage and the time left; headline carries it too.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 13020, kind: 'pr', stage: 95, author: 'hive-bot', author_kind: 'hive', soak_remaining_seconds: 17 * 60 + 1 }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, 'active', 'todo cap'], '095 lights the Merge gate column, no detour');
assert.ok(strip(row).includes('title="Pre-merge soak — admitted, soaking — 18m left · Next move: hive"'), 'chevron tooltip carries 095 + time left: ' + row);
assert.ok(tooltip(row).startsWith('Pre-merge soak — 18m left'), 'headline: ' + tooltip(row));
assert.strictEqual(stageTrackerHeadline({ stage: 95 }), 'Pre-merge soak', 'no remaining known → plain headline');
assert.strictEqual(stageTrackerHeadline({ stage: 95, soak_remaining_seconds: 5400 }), 'Pre-merge soak — 1h30m left');
assert.strictEqual(stageTrackerHeadline({ stage: 95, kind: 'issue', soak_remaining_seconds: 60 }), 'Pre-merge soak — 1m left (its PR)');
assert.strictEqual(stageTrackerAnchor(95, stages, 'pr'), STAGE_TRACKER_MERGE_COLUMN, '095 anchors on the merge gate column');

// PR returned to contributor: detour badge rides Review; chip cap = author kind + first modifier (+N).
row = stageTrackerRowHtml({ repo: 'acme/console', number: 102, kind: 'pr', title: 'Fix flaky', stage: 72, lane: 'contributor', tier: 'trusted', author: 'karibhou', author_kind: 'clanker', author_kind_source: 'inferred', modifiers: ['needs-rebase', 'needs-human'], caption: 'Returned to @karibhou', history: [{stage:50},{stage:60},{stage:72}] }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, 'active detoured', T, 'todo cap']);
assert.ok(row.includes('<span class="ham-detour">Returned to contributor</span>'));
// Invariant: a PR never occupies the issue slot. A PR with no linked issue
// puts the muted "no linked issue" placeholder on the left and names itself
// — author icon, #N, title — in the PR column, where a covering PR sits on
// an issue row (owner reference: the #100 → #101 row).
assert.deepStrictEqual(prCol(row).length, 1, 'a PR row names its PR in the PR column');
assert.ok(row.includes('class="ham-seg ham-head-pr"'), 'PR cell present on a PR row');
assert.ok(!row.includes('ham-head-wide'), 'lead cell is not wide: the PR column is in use');
assert.strictEqual(lead(row), 'no linked issue', 'issue slot: placeholder');
assert.ok(/<div class="ham-head-main ham-no-issue">\s*<svg class="ham-kind-ico"[\s\S]*?<span class="ham-title ham-no-issue-t">no linked issue<\/span>/.test(row), 'muted issue icon + placeholder text: ' + row);
assert.ok(!/<a class="ham-ref"/.test(row), 'no #N link in the issue slot when there is no issue');
assert.ok(prCol(row)[0].includes('ham-author-ico ham-author-clanker') && prCol(row)[0].includes('aria-label="clanker · @karibhou"') && prCol(row)[0].includes('<span class="ham-pr-num">#102</span>') && prCol(row)[0].includes('<span class="ham-pr-title">Fix flaky</span>'), 'coloured author icon, #102, title in the PR column: ' + prCol(row)[0]);
assert.strictEqual(titles(row, 'Fix flaky'), 1, 'title appears once');
assert.strictEqual(chips(row), 1, 'author kind is an icon, not a chip');
assert.ok(row.includes('ham-mod-needs-human">needs human +1</span>'), row);
assert.ok(tooltip(row).includes('Flags: needs-rebase, needs-human') && tooltip(row).includes('Trail: 050 → 060 → 072'), tooltip(row));

// PR reviewed (060): Review done, Merge gate lit, no detour.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 107, kind: 'pr', stage: 60 }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, 'active', 'todo cap']);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 109, kind: 'pr', stage: 80, caption: 'soak 20m left' }, stages, ctx);
assert.ok(row.includes('<span class="ham-seg-t">Ready for auto merge</span>') && strip(row).includes('title="Awaiting merge admission — not yet admitted — soak 20m left · Next move: hive"'), 'chevron tooltip carries 080: ' + row);

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
// A blocked issue with a hive:recheck-after deadline names it on the chip
// (hivecommons/hive#13310); a bare blocked row keeps the plain chip.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 107, kind: 'issue', stage: 0, modifiers: ['blocked'], bucket: 'filtered', bucket_reason: 'blocked until 2020-01-02T03:04:00Z' }, stages, ctx);
// The time goes through agentNowClockTime, so only the stable parts are pinned: the chip text and the minute.
assert.ok(/>blocked · re-check at [^<]*03:04[^<]*<\/span>/.test(row), row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 108, kind: 'issue', stage: 0, modifiers: ['blocked'], bucket: 'filtered', bucket_reason: 'blocked' }, stages, ctx);
assert.ok(row.includes('>blocked</span>') && !row.includes('re-check'), row);
// An ISSUE parked on a person wears 045 (owner decision: 085 is PR-only)
// before any PR exists: it lights Actionable (the issue side) as a detour,
// not the merge gate, and its headline carries no "(its PR)" suffix. A
// pre-045 issue still wearing 085 is drawn the same way until re-staged.
for (const parked of [45, 85]) {
  row = stageTrackerRowHtml({ repo: 'acme/console', number: 355, kind: 'issue', stage: parked }, stages, ctx);
  assert.deepStrictEqual(segStates(row), [D, 'active detoured', T, T, T, T, 'todo cap'], row);
  assert.ok(row.includes('<span class="ham-detour">Needs human decision</span>') && tooltip(row).includes('Needs a human decision') && !tooltip(row).includes('(its PR)'), row);
}
// 045 needs attention like the other parking stages.
assert.ok(stageTrackerNeedsAttention({ kind: 'issue', stage: 45 }) && stageTrackerNeedsAttention({ kind: 'pr', stage: 85 }) && !stageTrackerNeedsAttention({ kind: 'issue', stage: 10 }));
// ...but an issue whose single covering PR sits on 085 still rides the PR's
// merge gate, with the "(its PR)" suffix.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 356, kind: 'issue', stage: 40, linked_prs: [{ number: 109, author_kind: 'hive', stage: 85 }] }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, 'active detoured', 'todo cap'], row);
assert.ok(tooltip(row).includes('Needs a human decision (its PR)'), row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 107, kind: 'pr', stage: 50, modifiers: ['needs-decision'] }, stages, ctx);
assert.ok(row.includes('ham-mod-needs-decision">needs human decision</span>') && tooltip(row).includes('Flags: needs human decision'), row);
// Whose move it is, as an ink-coloured icon after the word: Review = hive,
// PR in progress = the PR author, Claimed = the claimant; Open/Actionable carry none.
assert.ok(row.includes('<svg class="ham-ico ham-actor-ico" viewBox="0 0 16 16"') && row.includes('aria-label="Next move: hive"') && row.includes('<span class="ham-seg-t">CI / Review</span><svg class="ham-ico ham-actor-ico"'), row);
assert.ok(!row.includes('ham-author-ico ham-actor') && !row.includes('ham-actor-ico ham-author-'), 'actor icon must not carry the author colour classes: ' + row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 108, kind: 'pr', stage: 72, author_kind: 'clanker' }, stages, ctx);
assert.ok(row.includes('aria-label="Next move: clanker"') && row.includes('<span class="ham-detour">Returned to contributor</span><svg class="ham-ico ham-actor-ico"'), row);
// 071 needs-author: a human's red PR rides the Review column as its own
// detour, the person as the mover.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 109, kind: 'pr', stage: 71, author: 'sam', author_kind: 'human', history: [{stage:50},{stage:71}] }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, 'active detoured', T, 'todo cap']);
assert.ok(row.includes('aria-label="Next move: human"') && row.includes('<span class="ham-detour">Needs author</span><svg class="ham-ico ham-actor-ico"'), row);
assert.ok(tooltip(row).includes('Trail: 050 → 071'), tooltip(row));
row = stageTrackerRowHtml({ repo: 'acme/console', number: 7, kind: 'issue', stage: 40, linked_prs: [{ number: 108, author_kind: 'clanker', stage: 30 }] }, stages, ctx);
assert.ok(subs(row).length === 0 && strip(row).includes('aria-label="Next move: clanker"') && strip(row).includes('<span class="ham-seg-t">PR in progress</span><svg class="ham-ico ham-actor-ico"'), 'single covering PR: the issue strip lights PR in progress with the PR author as the mover: ' + row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 7, kind: 'issue', stage: 40, linked_prs: [{ number: 108, author_kind: 'clanker', stage: 30 }, { number: 109, author_kind: 'hive', stage: 50 }] }, stages, ctx);
assert.ok(!strip(row).includes('ham-actor-ico') && !strip(row).includes('aria-current') && subs(row).length === 2 && subs(row)[1].includes('aria-label="Next move: clanker"') && subs(row)[1].includes('<span class="ham-seg-t">PR in progress</span><svg class="ham-ico ham-actor-ico"'), 'two covering PRs: the sub-rows, not the issue, light their stage: ' + row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 8, kind: 'issue', stage: 20, claimant_kind: 'hive' }, stages, ctx);
assert.ok(row.includes('aria-label="Next move: hive"') && row.includes('<span class="ham-seg-t">Claimed</span><svg class="ham-ico ham-actor-ico"'), row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 8, kind: 'issue', stage: 20 }, stages, ctx);
assert.ok(!row.includes('ham-actor-ico') && row.includes('<span class="ham-seg-t">Claimed</span>'), 'no claimant known, no icon: ' + row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 9, kind: 'issue', stage: 10 }, stages, ctx);
assert.ok(!row.includes('ham-actor-ico') && !tooltip(row).includes('Next move'), row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 105, kind: 'pr', stage: 90 }, stages, ctx);
assert.ok(row.includes('aria-label="Next move: human"') && row.includes('<span class="ham-detour">Ready for human merge</span><svg class="ham-ico ham-actor-ico"'), row);

// Merged: everything neutral-done, the cap carries the only check.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 106, kind: 'pr', stage: 110 }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, D, 'active cap merged']);
assert.ok(/<li class="ham-seg active cap merged"[^>]*><svg class="ham-ico"/.test(row), 'merged cap is the lit segment with the check icon');

// Hive-authored PR on its own: hive icon on its PR-column entry, no chips.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 108, kind: 'pr', stage: 50, author: 'hive-bot', author_kind: 'hive' }, stages, ctx);
assert.strictEqual(chips(row), 0);
assert.strictEqual(prCol(row).length, 1);
assert.ok(prCol(row)[0].includes('ham-author-ico ham-author-hive') && lead(row) === 'no linked issue', row);
// PR seen on its own row (Pull requests view) whose open issue is listed:
// the issue — icon, #N link, title — on the left, the PR on the right.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 110, kind: 'pr', title: 'Fix sort', stage: 50, author: 'hive-bot', author_kind: 'hive', nested_under: 9, linked_issues: [{ number: 9, title: 'Ordering drifts', url: 'https://x/9', state: 'open', listed: true }] }, stages, ctx);
assert.strictEqual(lead(row), '#9 Ordering drifts', 'issue slot names the issue: ' + lead(row));
assert.ok(/<a class="ham-ref" href="https:\/\/x\/9"[^>]*>#9<\/a>/.test(row) && !/<a class="ham-ref"[^>]*title=/.test(row), 'issue link, no tooltip on it: ' + row);
assert.ok(prCol(row)[0].includes('<span class="ham-pr-num">#110</span>') && prCol(row)[0].includes('<span class="ham-pr-title">Fix sort</span>'), prCol(row)[0]);
assert.ok(/<a class="ham-pr-ref"[^>]*title="Fix sort · Waiting for review/.test(row), 'row tooltip rides the PR link: ' + row);
assert.strictEqual(titles(row, 'Fix sort'), 1);
// A PR whose only linked issue GitHub reports closed (a superseded PR whose
// body still says Fixes #N): the issue slot keeps "#N title" as a link,
// struck through under a "closed" chip, so the pairing reads at a glance.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 111, kind: 'pr', title: 'Docs', stage: 95, linked_issues: [{ number: 13409, state: 'closed', title: 'Usage on --help', url: 'https://x/13409' }] }, stages, ctx);
assert.strictEqual(lead(row), '#13409 Usage on --help closed', lead(row));
assert.ok(/<div class="ham-head-main ham-issue-closed">/.test(row) && /<a class="ham-ref" href="https:\/\/x\/13409"[^>]*>#13409<\/a>/.test(row), 'closed issue stays a link in a struck-through slot: ' + row);
assert.ok(row.includes('<span class="ham-title" title="Usage on --help">Usage on --help</span> <span class="ham-badge ham-issue-closed-chip">closed</span>'), 'title then closed chip: ' + row);
assert.ok(!row.includes('ham-no-issue-t') && !/<a class="ham-ref"[^>]*title=/.test(row) && prCol(row).length === 1 && prCol(row)[0].includes('#111'), row);
// The same with no title resolvable: a muted "(closed issue)" placeholder, never a blank.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 111, kind: 'pr', title: 'Docs', stage: 95, linked_issues: [{ number: 13409, state: 'closed', url: 'https://x/13409' }] }, stages, ctx);
assert.strictEqual(lead(row), '#13409 (closed issue) closed', lead(row));
assert.ok(row.includes('<span class="ham-title ham-no-issue-t">(closed issue)</span> <span class="ham-badge ham-issue-closed-chip">closed</span>'), row);
// A linked issue of unknown state that is not listed still goes in the issue slot as a link, no chip; a missing title is said so.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 112, kind: 'pr', title: 'Fix', stage: 50, linked_issues: [{ number: 77, url: 'https://x/77' }] }, stages, ctx);
assert.strictEqual(lead(row), '#77 (title unavailable)', lead(row));
assert.ok(!row.includes('ham-issue-closed'), 'no closed marker on an issue of unknown state: ' + row);
assert.deepStrictEqual(stageTrackerPRIssueSlot({ linked_issues: [{ number: 5, state: 'closed' }, { number: 6, state: 'open', title: 'T' }] }), { number: 6, title: 'T', url: '' }, 'an open linked issue beats a closed one');
assert.deepStrictEqual(stageTrackerPRIssueSlot({ linked_issues: [{ number: 4 }, { number: 5, state: 'closed', title: 'C', url: 'u' }] }), { number: 5, title: 'C', url: 'u', closed: true, placeholder: '' }, 'a known-closed issue beats one of unknown state');
assert.deepStrictEqual(stageTrackerPRIssueSlot({ linked_issues: [{ number: 5, state: 'closed' }] }), { number: 5, title: '', url: '', closed: true, placeholder: STAGE_TRACKER_CLOSED_ISSUE_TITLE });
assert.deepStrictEqual(stageTrackerPRIssueSlot({}), { number: 0, text: 'no linked issue' });

// 040 shares the PR in progress column with 030; the chevron tooltip names the real stage.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 9, kind: 'issue', title: 'Ordering drifts', stage: 40, linked_prs: [{ number: 103, title: 'Backport', author_kind: 'clanker', stage: 30 }, { number: 102, title: 'Fix it', url: 'u', author_kind: 'human', stage: 70, stage_since: new Date(now - 3600000).toISOString(), modifiers: ['needs-rebase'], caption: 'Rebasing onto main' }] }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, B, B, B, 'blank cap']);
assert.ok(!strip(row).includes('aria-current'), 'issue strip has no lit chevron once PRs cover it');
assert.ok(row.includes('<span class="ham-title" title="Ordering drifts">Ordering drifts</span>'), 'issue keeps its own title on the left');
assert.ok(tooltip(row).includes('Covering PR: #102, #103'));
// Nested sub-rows, furthest along first: "[icon] #N title" at the PR column, then the PR's strip from that column on.
let sr = subs(row);
assert.strictEqual(sr.length, 2, 'one sub-row per covering PR');
assert.ok(prCol(sr[0])[0].includes('ham-author-human') && prCol(sr[0])[0].includes('#102') && prCol(sr[0])[0].includes('<span class="ham-pr-title">Fix it</span>'), prCol(sr[0])[0]);
assert.deepStrictEqual(segStates(sr[0]), [B, B, B, D, 'active detoured', T, 'todo cap'], 'PR strip starts at PR in progress');
assert.ok(sr[0].includes('<span class="ham-detour">Fixing</span><svg class="ham-ico ham-actor-ico"') && sr[0].includes('aria-label="Next move: human"') && sr[0].indexOf('<span class="ham-since">1h</span>') < sr[0].indexOf('aria-label="Pipeline progress"') && sr[0].includes('ham-mod-needs-rebase">needs rebase</span>'), 'Fixing is the author\'s move; the chip says what is being fixed: ' + sr[0]);
assert.ok(/^ data-ham-kind="pr" data-ham-stage="70">/.test(sr[0]) && /<a class="ham-pr-ref"[^>]*title="Fix it · Fixing review findings Rebasing onto main/.test(sr[0]), 'sub-row tooltip rides the PR link, not the container: ' + sr[0].slice(0, 400));
assert.ok(prCol(sr[1])[0].includes('ham-author-clanker') && prCol(sr[1])[0].includes('#103'));
assert.deepStrictEqual(segStates(sr[1]), [B, B, B, 'active', T, T, 'todo cap'], 'a PR at 030 lights PR in progress on its own sub-row');
assert.ok(!row.slice(0, row.indexOf('<div class="ham-sub"')).includes('ham-pr-ref'), 'the issue title line itself lists no PRs');
row = stageTrackerRowHtml({ repo: 'acme/console', number: 10, kind: 'issue', stage: 30 }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, 'active', T, T, 'todo cap'], 'kick running, no PR yet: the issue lights PR in progress and draws the steps ahead');
assert.strictEqual(subs(row).length, 0, 'no PR yet');
// A merged covering PR: single entry with the green cap lit; nested when there are two.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 11, kind: 'issue', stage: 40, linked_prs: [{ number: 120, title: 'Done', author_kind: 'hive', stage: 110 }] }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, D, 'active cap merged']);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 11, kind: 'issue', stage: 40, linked_prs: [{ number: 121, title: 'Follow-up', author_kind: 'hive', stage: 50 }, { number: 120, title: 'Done', author_kind: 'hive', stage: 110 }] }, stages, ctx);
assert.deepStrictEqual(segStates(subs(row)[0]), [B, B, B, D, D, D, 'active cap merged'], 'furthest along first');
// Title line: lead cell (Open..Claimed) and PR cell (PR in progress..Merge gate) each truncate with an ellipsis; the full title is on the element itself.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 12, kind: 'issue', title: 'A very long issue title that must not wrap', stage: 10 }, stages, ctx);
// No PR to list: the lead cell widens over the empty PR column so a long title has room before it truncates.
assert.ok(/<li class="ham-seg ham-head-lead ham-head-wide"><div class="ham-head-main">[\s\S]*?<span class="ham-title" title="A very long issue title that must not wrap">A very long issue title that must not wrap<\/span>[\s\S]*?<\/li><li class="ham-seg cap"><span class="ham-badges"><\/span><\/li>/.test(row), row);
// Sort key: a covered issue counts as far along as its furthest PR.
assert.strictEqual(stageTrackerSortKey({ kind: 'issue', stage: 40, linked_prs: [{ number: 1, stage: 30 }, { number: 2, stage: 90 }] }, stages).anchor, 80);
// An issue parked on 045 (or a pre-045 085) with no PR sorts at Actionable, not past the merge gate.
assert.strictEqual(stageTrackerSortKey({ kind: 'issue', stage: 45 }, stages).anchor, 10);
assert.strictEqual(stageTrackerSortKey({ kind: 'issue', stage: 15 }, stages).anchor, 10, '015 sorts with Actionable');
assert.strictEqual(stageTrackerSortKey({ kind: 'issue', stage: 85 }, stages).anchor, 10);
assert.strictEqual(stageTrackerSortKey({ kind: 'pr', stage: 85 }, stages).anchor, 80);
assert.strictEqual(stageTrackerSortKey({ kind: 'issue', stage: 40, linked_prs: [{ number: 1 }] }, stages).anchor, 30, 'unknown PR stage falls back to the issue');
assert.strictEqual(stageTrackerSortKey({ kind: 'pr', stage: 72 }, stages).anchor, 50);

// Header: names once, aligned with the chevrons, plus the since column.
const head = stageTrackerHeaderHtml(stages);
assert.deepStrictEqual(Array.from(head.matchAll(/<span class="ham-seg-t">([^<]*)<\/span>/g)).map(m => m[1]), ['Open', 'Actionable', 'Claimed', 'PR in progress', 'CI / Review', 'Merge gate']);
assert.ok(head.includes('<span class="ham-since">since</span>'));

// Unknown / zero stage lights "Open".
row = stageTrackerRowHtml({ repo: 'acme/console', number: 1, kind: 'issue', stage: 0 }, stages, ctx);
assert.deepStrictEqual(segStates(row), ['active', T, T, T, T, T, 'todo cap']);

// Every PR stage has an active word and an actor on its segment; the word
// is what the chevron says, the actor is whose move it is. The active
// chevron now leads with its stage icon, so the regex skips that svg.
const prWords = {};
for (const [st, word, actor] of [[50, 'CI / Review', 'hive'], [60, 'Ready for auto merge', 'hive'], [70, 'Fixing', 'author'], [71, 'Needs author', 'author'], [72, 'Returned to contributor', 'author'], [74, 'Adopted by hive', 'hive'], [80, 'Ready for auto merge', 'hive'], [85, 'Needs human decision', 'human'], [90, 'Ready for human merge', 'human'], [100, 'Ready for auto merge', 'hive']]) {
  const r = stageTrackerRowHtml({ repo: 'acme/console', number: 500 + st, kind: 'pr', stage: st, author_kind: 'clanker' }, stages, ctx);
  const m = /aria-current="step"><svg class="ham-ico ham-active-ico"[^>]*>[\s\S]*?<\/svg><span class="ham-(?:seg-t|detour)">([^<]*)<\/span><svg class="ham-ico ham-actor-ico"[^>]*aria-label="Next move: ([a-z]+)"[^>]*>/.exec(r);
  assert.ok(m, 'stage ' + st + ' has no active word + actor: ' + r);
  assert.deepStrictEqual([m[1], m[2]], [word, actor === 'author' ? 'clanker' : actor], 'stage ' + st);
  prWords[st] = m[1];
}
// The responsive caption names the column, the detour word when there is one,
// and the next mover for the strip's active segment; it stays empty when no
// active segment is in range.
assert.strictEqual(stageTrackerCurrentStageHtml({ repo: 'acme/console', number: 570, kind: 'pr', stage: 70, author_kind: 'clanker' }, stages), '<p class="ham-current-stage">CI / Review · Fixing · Next move: clanker</p>');
assert.strictEqual(stageTrackerCurrentStageHtml({ repo: 'acme/console', number: 560, kind: 'pr', stage: 60 }, stages), '<p class="ham-current-stage">Ready for auto merge · Next move: hive</p>', 'no detour → column word only');
assert.strictEqual(stageTrackerCurrentStageHtml({ repo: 'acme/console', number: 570, kind: 'pr', stage: 70, author_kind: 'clanker' }, stages, { from: 500 }), '', 'no caption when the active segment is out of range');
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

// Golden: one visible title per item across every row shape. A PR must never
// be its own parent issue, and must never appear under the Issues filter.
const g = [
  { kind: 'issue', repo: 'g/r', number: 1, title: 'Lone issue', stage: 20, author: 'ann', author_kind: 'human' },
  { kind: 'issue', repo: 'g/r', number: 2, title: 'Issue with one PR', stage: 40, linked_prs: [{ number: 21, title: 'The one PR', author: 'hive-bot', author_kind: 'hive', stage: 50 }] },
  { kind: 'pr', repo: 'g/r', number: 21, title: 'The one PR', stage: 50, author: 'hive-bot', author_kind: 'hive', nested_under: 2 },
  { kind: 'issue', repo: 'g/r', number: 3, title: 'Issue with two PRs', stage: 40, linked_prs: [{ number: 31, title: 'First PR', author: 'bo', author_kind: 'clanker', stage: 72 }, { number: 32, title: 'Second PR', author: 'cy', author_kind: 'human', stage: 90 }] },
  { kind: 'pr', repo: 'g/r', number: 31, title: 'First PR', stage: 72, author: 'bo', author_kind: 'clanker', nested_under: 3 },
  { kind: 'pr', repo: 'g/r', number: 32, title: 'Second PR', stage: 90, author: 'cy', author_kind: 'human', nested_under: 3 },
  { kind: 'pr', repo: 'g/r', number: 40, title: 'Standalone PR', stage: 80, author: 'cy', author_kind: 'human', modifiers: ['needs-rebase'] },
];
const golden = stageTrackerVisible(g, { kind: 'all', repo: '' }).map(it => stageTrackerRowHtml(it, stages, ctx)).join('\n');
for (const it of g) assert.strictEqual(titles(golden, it.title), 1, 'exactly one visible title for #' + it.number + ' (' + it.title + ')');
assert.strictEqual((golden.match(/<div class="ham-row"/g) || []).length, 4, 'four top-level rows: 3 issues + 1 standalone PR');
assert.strictEqual((golden.match(/<div class="ham-sub"/g) || []).length, 2, 'two sub-rows, only under the two-PR issue');
assert.strictEqual((golden.match(/ham-head-wide/g) || []).length, 2, 'wide lead wherever the title line has no PR entry: PR-less issue, two-PR issue (PRs in sub-rows)');
assert.strictEqual((golden.match(/<span class="ham-pr-num">#40<\/span>/g) || []).length, 1, 'the standalone PR sits once in the PR column of its own row');
assert.strictEqual((golden.match(/ham-no-issue-t">no linked issue</g) || []).length, 1, 'exactly one placeholder issue slot: the standalone PR');
for (const r of golden.split('<div class="ham-row"').slice(1)) assert.ok(!/<li class="ham-seg ham-head-lead[^"]*">(?:(?!<\/li>)[\s\S])*ham-pr-ref/.test(r), 'a PR never sits in the issue slot: ' + r.slice(0, 300));
assert.ok(golden.includes('ham-mod-needs-rebase">needs rebase</span>'), 'standalone PR keeps its chip on the title line');
assert.deepStrictEqual(stageTrackerVisible(g, { kind: 'issue', repo: '' }).map(it => it.kind), ['issue', 'issue', 'issue'], 'Issues filter never shows a PR');
assert.deepStrictEqual(stageTrackerVisible(g, { kind: 'pr', repo: '' }).map(it => it.number), [21, 31, 32, 40], 'Pull requests filter flattens every PR once');

// Hover text budget per row (owner request): only the #N link, the PR link,
// the item title and the active chevron carry title="…". The row container,
// sub-row container, kind / author / actor icons, chips, since cells and
// every non-active chevron carry none; icons expose their label via aria-label.
const tags = (html) => Array.from(html.matchAll(/<(\w+)\b([^>]*)>/g)).map(m => ({ tag: m[1], attrs: m[2] }));
const titled = (html) => tags(html).filter(t => / title="/.test(t.attrs));
const expectTitles = (html, want, what) => assert.deepStrictEqual(titled(html).map(t => t.tag + '.' + ((/class="([^" ]+)/.exec(t.attrs) || [])[1] || '')), want, what + ': ' + html);
assert.ok(!/<svg[^>]*>\s*<title>/.test(golden), 'no svg <title> tooltips anywhere in the tracker');
assert.ok(!/<(div|span)[^>]*class="ham-(row|sub|since|badge|kind-ico)[^>]*title="/.test(golden), 'containers, since cells, chips and kind icons carry no title');
for (const r of golden.split('<div class="ham-row"').slice(1)) {
  const seg = Array.from(r.matchAll(/<li class="ham-seg ([^"]*)"([^>]*)>/g));
  for (const m of seg) {
    const isActive = /\bactive\b/.test(m[1]);
    assert.strictEqual(/ title="/.test(m[2]), isActive, (isActive ? 'active chevron needs a title' : 'inactive chevron must have no title') + ': ' + m[0]);
  }
}
// Issue with no PR: #N link, issue title, active chevron.
row = stageTrackerRowHtml(g[0], stages, ctx);
expectTitles(row, ['a.ham-ref', 'span.ham-title', 'li.ham-seg'], 'lone issue');
assert.ok(/<li class="ham-seg active" title="Claimed" aria-current="step">/.test(row), 'active chevron names the stage (no claimant known, no mover line): ' + row);
assert.ok(/<a class="ham-ref"[^>]*title="[^"]*@ann[^"]*"/.test(row), 'the #N link carries the full row tooltip');
// Issue with one PR: #N link, issue title, PR link (its title + author), active chevron.
row = stageTrackerRowHtml(g[1], stages, ctx);
expectTitles(row, ['a.ham-ref', 'span.ham-title', 'a.ham-pr-ref', 'li.ham-seg'], 'issue with one PR');
assert.ok(/<a class="ham-pr-ref"[^>]*title="The one PR Author: hive · @hive-bot"/.test(row), row);
assert.ok(/<li class="ham-seg active" title="Review pending · Next move: hive" aria-current="step">/.test(row), 'active chevron: stage + whose move: ' + row);
// Issue with two PRs: #N link + title on the issue line, nothing lit there;
// each sub-row has its PR link (full PR tooltip) and its active chevron.
row = stageTrackerRowHtml(g[3], stages, ctx);
expectTitles(row.split('<div class="ham-sub"')[0], ['a.ham-ref', 'span.ham-title'], 'two-PR issue line');
for (const s of subs(row)) expectTitles(s, ['a.ham-pr-ref', 'li.ham-seg'], 'sub-row');
assert.ok(/<a class="ham-pr-ref"[^>]*title="First PR · Back with the contributor[^"]*@bo \(author\/clanker\)[^"]*"/.test(subs(row)[1]), 'sub-row PR link: PR title, then the PR tooltip: ' + subs(row)[1]);
// Standalone PR: the PR link (its title, then the row tooltip) and the active
// chevron; the placeholder issue slot carries none.
row = stageTrackerRowHtml(g[6], stages, ctx);
expectTitles(row, ['a.ham-pr-ref', 'li.ham-seg'], 'standalone PR');
assert.ok(/<a class="ham-pr-ref"[^>]*title="Standalone PR · Not yet admitted to auto merge[^"]*@cy \(author\/human\)/.test(row), 'PR link: title, then the row tooltip: ' + row);
assert.ok(row.includes('aria-label="human · @cy"') && !row.includes('<title>'), 'author icon label is aria-only: ' + row);
// Since cells are untouched: plain text, no title.
assert.ok(/<span class="ham-since( ham-stalled)?">[^<]*<\/span>/.test(row) && !/ham-since[^>]*title=/.test(golden), 'since cells unchanged');
// Merged: the lit cap is the only chevron with hover text.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 106, kind: 'pr', stage: 110 }, stages, ctx);
assert.ok(/<li class="ham-seg active cap merged" title="Merged — [^"]*" aria-current="step">/.test(row) && titled(strip(row)).length === 1, row);
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node Hive activity monitor fixture failed: %v\n%s", err, out)
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
		jsConstLine(t, html, "STAGE_TRACKER_PATH"),
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_DECISION"),
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_NEEDS_DECISION"),
		jsConstLine(t, html, "STAGE_TRACKER_CLOSED_ISSUE_TITLE"),
		jsConstLine(t, html, "STAGE_TRACKER_UNTITLED_ISSUE"),
		jsConstLine(t, html, "STAGE_TRACKER_TRIAGE_NEEDED"),
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_DECISION_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_PRE_MERGE_SOAK"),
		jsFunc(t, html, "stageTrackerDefaultFilter"),
		"var _stageTrackerFilter = stageTrackerDefaultFilter();",
		jsFunc(t, html, "stageTrackerAnchor"),
		jsFunc(t, html, "stageTrackerLinkedSorted"),
		jsFunc(t, html, "stageTrackerSortKey"),
		jsFunc(t, html, "stageTrackerNeedsAttention"),
		jsFunc(t, html, "stageTrackerWaitSince"),
		jsFunc(t, html, "stageTrackerLabelFamily"),
		jsFunc(t, html, "stageTrackerNormalizeFilter"),
		jsFunc(t, html, "stageTrackerFilterActive"),
		jsFunc(t, html, "stageTrackerMatches"),
		jsFunc(t, html, "stageTrackerVisible"),
		jsFunc(t, html, "stageTrackerSort"),
		jsFunc(t, html, "stageTrackerPage"),
		jsFunc(t, html, "stageTrackerLabelFacets"),
		jsFunc(t, html, "stageTrackerCountsText"),
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

// Normalisation tolerates junk and partial objects (older stored state).
const n = stageTrackerNormalizeFilter({ kind: 'bogus', labels: 'nope', age: '2h', sort: 'x', dir: 'sideways', size: 7, page: -3 });
assert.deepStrictEqual([n.kind, n.labels, n.age, n.sort, n.dir, n.size, n.page], ['all', [], 'any', 'furthest', 'desc', STAGE_TRACKER_DEFAULT_PAGE_SIZE, 1]);
assert.deepStrictEqual(stageTrackerNormalizeFilter({ kind: 'pr', repo: 'a/b' }).labels, [], 'the pre-filter shape still works');
assert.deepStrictEqual(stageTrackerNormalizeFilter({ labels: ['Author/Human'] }).labels, ['author/human']);

// Sorting: furthest along first (then longest on step); waiting; newest; number; direction flips.
assert.deepStrictEqual(nums(stageTrackerSort(items, stages, F({ sort: 'furthest' }))), [4, 3, 2, 5, 1]);
assert.deepStrictEqual(nums(stageTrackerSort(items, stages, F({ sort: 'furthest', dir: 'asc' }))), [1, 5, 2, 3, 4]);
assert.deepStrictEqual(nums(stageTrackerSort(items, stages, F({ sort: 'waiting' }))), [4, 1, 5, 2, 3], 'longest wait first; an unstaged item waits since it was opened');
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
assert.strictEqual(c.header, '120 of 150 items · 7 need attention');
assert.strictEqual(c.badge, 150, 'without open counts the pill is every item');
assert.strictEqual(c.badgeTitle, '150 items open · 7 need attention');
c = stageTrackerCountsText(stageTrackerPage(many, F({ size: 50, page: 2 })), 150, 7, { issues: 100, prs: 50 }, 9);
assert.strictEqual(c.header, '150 items (100 issues / 50 PRs) · 120 match filters · 7 need attention');
assert.strictEqual(c.badge, 150);
assert.strictEqual(c.badgeTitle, '150 items (100 issues / 50 PRs) open · 9 need attention', 'pill tooltip counts attention over every item');
c = stageTrackerCountsText(stageTrackerPage(many.slice(0, 1), F()), 1, 1);
assert.strictEqual(c.showing, 'showing 1–1 of 1');
assert.strictEqual(c.header, '1 item · 1 needs attention');
c = stageTrackerCountsText(stageTrackerPage([], F()), 5, 0);
assert.strictEqual(c.showing, 'showing 0 of 0 (5 filtered out)');
assert.strictEqual(c.header, '0 of 5 items · nothing needs attention');

// Label facets: family labels always offered, others only when shared; selected chips stay.
let fac = stageTrackerLabelFacets(items, F());
assert.deepStrictEqual(fac.chips.map(x => x.label), ['stage/010-actionable', 'stage/020-claimed', 'stage/050-review', 'stage/080-merge-gate', 'needs-rebase', 'author/clanker', 'author/hive', 'author/human', 'role/docs', 'bug']);
assert.deepStrictEqual(fac.chips.find(x => x.label === 'author/clanker').count, 2);
assert.ok(stageTrackerLabelFacets(items.concat([{ kind: 'pr', repo: 'a/b', number: 6, labels: ['do-not-merge/hold'] }]), F()).chips.some(x => x.label === 'do-not-merge/hold'), 'hold labels are offered even when only one item is held');
fac = stageTrackerLabelFacets(items.concat([{ kind: 'issue', repo: 'a/b', number: 6, labels: ['lonely'] }]), F({ labels: ['lonely'] }));
assert.ok(fac.chips.some(x => x.label === 'lonely' && x.on), 'a selected single-item label is still offered so it can be cleared');
assert.strictEqual(stageTrackerLabelFacets(items.concat([{ kind: 'issue', repo: 'a/b', number: 7, labels: ['solo'] }]), F()).chips.some(x => x.label === 'solo'), false);
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node Hive activity monitor fixture failed: %v\n%s", err, out)
	}
}
