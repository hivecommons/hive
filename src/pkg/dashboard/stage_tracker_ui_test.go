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
		// Waiting-on-you surfacing (hivecommons/hive#15638): second sidebar
		// badge, topbar pill, toolbar chip and the filter key they share.
		`id="activity-monitor-waiting-badge" class="badge-count oc-nav-count oc-nav-count--waiting" data-zero`,
		`id="oc-waiting-on-you" data-action="showWaitingOnYou"`,
		`id="activity-monitor-waiting" data-action="hamFilter" data-arg0="waitingOnYou" data-arg1="toggle"`,
		`if (f.waitingOnYou && !item.waiting_on_you) return false;`,
		`function stageTrackerRenderWaiting(d, nowMs)`,
		`function stageTrackerWaitingToast(w, nowMs)`,
		`if (typeof window.hiveKioskMode === 'function' && window.hiveKioskMode()) return;`,
		`const STAGE_TRACKER_WAITING_FILTER_PARAM = 'waiting-on-you';`,
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

// stageTrackerRowsPreludeJS lifts everything the pure row renderer needs
// out of index.html; stageTrackerRowsFixtureJS is the stage table and the
// row-inspection helpers the row tests share.
func stageTrackerRowsPreludeJS(t *testing.T, html string) string {
	t.Helper()
	return strings.Join([]string{
		"const window = {};",
		"function formatDashboardTime(d){return new Date(d).toISOString().slice(11,16)+' UTC';}",
		"function formatDashboardDate(d){return new Date(d).toISOString().slice(0,10);}",
		"function formatDashboardDateTime(d){return new Date(d).toISOString().slice(0,16);}",
		jsConstLine(t, html, "STAGE_TRACKER_PATH"),
		jsConstLine(t, html, "STAGE_TRACKER_PR_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_COLUMN"),
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
		jsConstLine(t, html, "STAGE_TRACKER_ACTIVE_WORD"),
		jsConstLine(t, html, "STAGE_TRACKER_MOD_TEXT"),
		jsConstLine(t, html, "STAGE_TRACKER_DRAFT_TIP"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTOR"),
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL"),
		stageTrackerSentinelJS(t, html),
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
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_HUMAN_STAGES"),
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_HUMAN_AUTHOR_STAGES"),
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_HUMAN_MODS"),
		jsFunc(t, html, "stageTrackerItemOnHold"),
		jsFunc(t, html, "stageTrackerItemNeedsHuman"),
		jsFunc(t, html, "stageTrackerLabelCompare"),
		jsFunc(t, html, "stageTrackerLabelDialogData"),
		stageTrackerActedJS(t, html),
		jsFunc(t, html, "stageTrackerNormalizeFilter"),
		jsFunc(t, html, "stageTrackerFilterActive"),
		jsFunc(t, html, "stageTrackerMatches"),
		jsFunc(t, html, "stageTrackerSort"),
		jsFunc(t, html, "stageTrackerPage"),
		jsFunc(t, html, "stageTrackerLabelFacets"),
		jsFunc(t, html, "stageTrackerCountsText"),
		jsFunc(t, html, "stageTrackerRowBreakdown"),
		jsFunc(t, html, "stageTrackerIcon"),
		jsFunc(t, html, "stageTrackerPRRef"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTOR_NONE"),
		jsConstLine(t, html, "STAGE_TRACKER_NOBODY_LEAD"),
		jsFunc(t, html, "stageTrackerStageActor"),
		jsFunc(t, html, "stageTrackerActorKind"),
		jsFunc(t, html, "stageTrackerActorTip"),
		jsConstLine(t, html, "STAGE_TRACKER_HIVE_PREFIXES"),
		jsConstLine(t, html, "STAGE_TRACKER_CLAIMANT_SOURCE"),
		jsFunc(t, html, "stageTrackerShortHive"),
		jsFunc(t, html, "stageTrackerClaimant"),
		jsConstLine(t, html, "STAGE_TRACKER_CLAIMANT_ICON"),
		jsFunc(t, html, "stageTrackerClaimantIconKind"),
		jsFunc(t, html, "stageTrackerClaimantShort"),
		jsFunc(t, html, "stageTrackerClaimantLine"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_STATIC"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_ACTIVE"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_HUMAN"),
		jsConstBlock(t, html, "STAGE_TRACKER_MOTION_BY_STAGE"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_AUTHOR_STAGES"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_HUMAN_MODS"),
		jsFunc(t, html, "stageTrackerMotion"),
		jsFunc(t, html, "stageTrackerHeaderHtml"),
		jsConstLine(t, html, "STAGE_TRACKER_TRAIN_ICON_PATH"),
		jsConstLine(t, html, "STAGE_TRACKER_TRAIN_ICON_TITLE"),
		jsFunc(t, html, "stageTrackerTrainIconHtml"),
		jsFunc(t, html, "stageTrackerHoldLabels"),
		jsFunc(t, html, "stageTrackerSoakLeft"),
		jsFunc(t, html, "stageTrackerStripItems"),
		jsFunc(t, html, "stageTrackerCurrentStageHtml"),
		jsFunc(t, html, "stageTrackerStageWord"),
		jsConstLine(t, html, "STAGE_TRACKER_HUMAN_MODS"),
		jsConstLine(t, html, "STAGE_TRACKER_HANDLED_TEXT"),
		jsConstLine(t, html, "STAGE_TRACKER_HANDLED_NOTE"),
		jsFunc(t, html, "stageTrackerRegressionHandled"),
		jsFunc(t, html, "stageTrackerRegressionText"),
		jsFunc(t, html, "stageTrackerStaleLabelsText"),
		jsFunc(t, html, "stageTrackerHopName"),
		jsFunc(t, html, "stageTrackerTrailText"),
		jsFunc(t, html, "stageTrackerTooltip"),
		jsFunc(t, html, "stageTrackerChipsHtml"),
		jsFunc(t, html, "stageTrackerHeadColsHtml"),
		jsFunc(t, html, "stageTrackerSinceHtml"),
		jsFunc(t, html, "stageTrackerSubRowHtml"),
		jsFunc(t, html, "stageTrackerPRPillsHtml"),
		jsFunc(t, html, "stageTrackerAgoText"),
		jsConstBlock(t, html, "STAGE_TRACKER_LINEAGE_REASON_TIP"),
		jsFunc(t, html, "stageTrackerLineageText"),
		jsFunc(t, html, "stageTrackerSupersededOriginalClause"),
		jsFunc(t, html, "stageTrackerLineageTip"),
		jsFunc(t, html, "stageTrackerLineageChipHtml"),
		jsFunc(t, html, "stageTrackerLinkedPRItem"),
		jsFunc(t, html, "stageTrackerLinkedPROpen"),
		jsFunc(t, html, "stageTrackerNestedOpenPRs"),
		jsConstLine(t, html, "STAGE_TRACKER_LINKED_TIP"),
		jsFunc(t, html, "stageTrackerSharedPRs"),
		jsFunc(t, html, "stageTrackerLinkedTip"),
		jsConstLine(t, html, "STAGE_TRACKER_LINKED_GLYPH"),
		jsFunc(t, html, "stageTrackerLinkedDrawnHere"),
		jsFunc(t, html, "stageTrackerLinkedSuperseder"),
		jsFunc(t, html, "stageTrackerLinkedSupersededGone"),
		jsFunc(t, html, "stageTrackerLinkedSupersededTip"),
		jsFunc(t, html, "stageTrackerLinkedSorted"),
		stageTrackerTierJS(t, html),
		jsFunc(t, html, "stageTrackerSortKey"),
		jsFunc(t, html, "esc"),
		jsFunc(t, html, "agentNowAgo"),
		jsFunc(t, html, "agentNowClockTime"),
		jsFunc(t, html, "stageTrackerHeadline"),
		jsFunc(t, html, "stageTrackerAnchor"),
		jsFunc(t, html, "stageTrackerSegments"),
		jsConstLine(t, html, "STAGE_TRACKER_OPEN_STALE_DAYS"),
		jsFunc(t, html, "stageTrackerOpenSince"),
		jsFunc(t, html, "stageTrackerBlockedTip"),
		jsFunc(t, html, "stageTrackerBlockedOnPR"),
		jsConstLine(t, html, "STAGE_TRACKER_BLOCKED_ON_PR_OPEN"),
		jsConstLine(t, html, "STAGE_TRACKER_BLOCKED_ON_PR_MERGED"),
		jsConstLine(t, html, "STAGE_TRACKER_BLOCKED_ON_PR_CLOSED"),
		jsFunc(t, html, "stageTrackerShortAgo"),
		jsFunc(t, html, "stageTrackerRefOf"),
		jsFunc(t, html, "stageTrackerRowToolsHtml"),
		jsFunc(t, html, "stageTrackerRowHtml"),
		jsFunc(t, html, "stageTrackerPRIssueSlot"),
		jsFunc(t, html, "stageTrackerClosedChipHtml"),
		jsFunc(t, html, "stageTrackerItemURL"),
		jsFunc(t, html, "stageTrackerItemKey"),
		jsFunc(t, html, "stageTrackerMoveAttrs"),
		jsFunc(t, html, "stageTrackerNeedsAttention"),
		jsFunc(t, html, "stageTrackerVisible"),
		stageTrackerCuesJS(t, html),
	}, "\n")
}

const stageTrackerRowsFixtureJS = `
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
  { number: 80, label: 'Awaiting merge admission', short: 'Merge queue' },
  { number: 85, label: 'Needs human decision', short: 'Needs human decision', branch_from: 80 }, { number: 90, label: 'Ready for human merge', short: 'Ready for human merge', branch_from: 80 },
  { number: 95, label: 'Pre-merge soak', short: 'Soaking', description: 'Lane admitted; the pre-merge soak window is running before the merge is attempted' },
  { number: 100, label: 'Merge queue', short: 'Merge queue' }, { number: 110, label: 'Merged', short: 'Merged' },
];
// The stage table the tooltip's trail names its hops from (fetchStageTracker fills it live).
var _stageTracker = { stages };
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
`

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
	script := stageTrackerRowsPreludeJS(t, html) + stageTrackerRowsFixtureJS + `

// Issue claimed 30 min ago: Open + Actionable done, Claimed active, five ahead, cap todo.
let row = stageTrackerRowHtml({ repo: 'acme/console', number: 7, kind: 'issue', title: 'Fix <login>', url: 'https://x/7', stage: 20, stage_since: new Date(now - 30*60000).toISOString(), caption: 'Claimed by scanner', author: 'alice', author_kind: 'human', author_kind_source: 'label' }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, 'active', T, T, T, 'todo cap'], 'an issue with no PR yet draws the whole strip ahead');
assert.ok(row.includes('aria-current="step"') && subs(row).length === 0);
assert.ok(row.includes('Fix &lt;login&gt;'), 'title escaped');
assert.strictEqual(prCol(row).length, 0, 'issue without a PR has an empty PR column');
assert.ok(tooltip(row).includes('(person'), 'author kind stays in the tooltip, in plain words');
assert.ok(row.includes('<span class="ham-since">30m</span>'), row);
// The age and the chip belong to the strip they describe, not to a title line
// that may wrap: the since text closes the strip row (an empty spacer keeps
// the title line's width) and the chip is the last thing in the title line's
// ghost strip, right above the strip's cap.
assert.ok(row.indexOf('<span class="ham-since">30m</span>') > row.indexOf('aria-label="Pipeline progress"'), 'since sits on the strip row, in line with the chevrons');
assert.ok(/aria-label="Pipeline progress">[\s\S]*?<\/ol>\s*<span class="ham-since">30m<\/span>\s*<\/div>/.test(row), 'the strip row ends with the since cell');
assert.ok(/<li class="ham-seg cap"><span class="ham-badges">[\s\S]*?<\/span><\/li><\/ol><span class="ham-since"><\/span><\/div>\s*<\/div>\s*<div class="ham-strip-row">/.test(row), 'the chip cell closes the title line, directly above its own strip row');
assert.ok(row.indexOf('<div class="ham-row-head">') < row.indexOf('ham-badges') && row.indexOf('ham-badges') < row.indexOf('aria-label="Pipeline progress"'), 'chip and strip share one row container');
assert.ok(row.includes('ham-kind-ico'), 'kind icon');
assert.strictEqual((strip(row).match(/<svg class="ham-ico( ham-history-ico)?"/g) || []).length, 6, 'icons on every drawn non-active chevron');
assert.ok(/<li class="ham-seg todo cap"><svg class="ham-ico ham-history-ico"/.test(strip(row)), 'the unmerged cap wears the history mark, not the ✓');
assert.ok(!strip(row).includes('✓'), 'no ✓ in the strip before the PR merges');
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

// Plain Open (no stage label, not held) says how long it has sat there, from
// the same instant the since column draws (stage_since), with the ISO time on
// the chevron tooltip; past STAGE_TRACKER_OPEN_STALE_DAYS it is tinted stale.
const openWord = (html) => (strip(html).match(/<span class="ham-seg-t">([^<]*)<\/span>/) || [])[1];
row = stageTrackerRowHtml({ repo: 'acme/console', number: 20, kind: 'issue', title: 'Fresh', stage: 0, stage_since: new Date(now - 14 * 3600000).toISOString(), created_at: new Date(now - 30 * 86400000).toISOString() }, stages, ctx);
assert.strictEqual(openWord(row), 'Open · since 14h', row);
assert.deepStrictEqual(segStates(row), ['active', T, T, T, T, T, 'todo cap'], 'under the stale threshold: no tint');
assert.ok(strip(row).includes('open since ' + new Date(now - 14 * 3600000).toISOString()), 'tooltip names the instant');
assert.ok(!strip(row).includes('(opened)'), 'stage_since is not the created_at fallback');
assert.strictEqual(stageTrackerSinceHtml({ stage_since: new Date(now - 14 * 3600000).toISOString() }, now), '<span class="ham-since">14h</span>', 'the since column shows the same age');
row = stageTrackerRowHtml({ repo: 'acme/console', number: 21, kind: 'issue', title: 'Old', stage: 0, stage_since: new Date(now - 3 * 86400000).toISOString() }, stages, ctx);
assert.strictEqual(openWord(row), 'Open · since 3d');
assert.deepStrictEqual(segStates(row), ['active ham-open-stale', T, T, T, T, T, 'todo cap'], 'past STAGE_TRACKER_OPEN_STALE_DAYS: stale tint');
row = stageTrackerRowHtml({ repo: 'acme/console', number: 22, kind: 'issue', title: 'Only created', stage: 0, created_at: new Date(now - 5 * 86400000).toISOString() }, stages, ctx);
assert.strictEqual(openWord(row), 'Open · since 5d', 'falls back to created_at');
assert.ok(strip(row).includes('open since ' + new Date(now - 5 * 86400000).toISOString() + ' (opened)'), 'the fallback is named');
assert.deepStrictEqual(stageTrackerOpenSince({ stage_since: '2026-01-01T00:00:00Z', created_at: '2025-01-01T00:00:00Z' }), { ms: Date.parse('2026-01-01T00:00:00Z'), fromCreated: false });
assert.deepStrictEqual(stageTrackerOpenSince({ created_at: '2025-01-01T00:00:00Z' }), { ms: Date.parse('2025-01-01T00:00:00Z'), fromCreated: true });
assert.deepStrictEqual(stageTrackerOpenSince({}), { ms: 0, fromCreated: false });
row = stageTrackerRowHtml({ repo: 'acme/console', number: 23, kind: 'pr', title: 'Held', stage: 0, stage_since: new Date(now - 3 * 86400000).toISOString(), modifiers: ['hold'] }, stages, ctx);
assert.strictEqual(openWord(row), 'Open · hold', 'Open · hold never carries the age (owner)');
assert.deepStrictEqual(segStates(row), ['active ham-open-hold', T, T, T, T, T, 'todo cap'], 'held: no stale tint either');
// Open · blocked (the blocked label) mirrors Open · hold but, unlike it,
// keeps the age — a stale blocked item is what the owner wants to see
// (hivecommons/hive#14194). hold wins when both are present.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 25, kind: 'issue', title: 'Stuck', stage: 0, stage_since: new Date(now - 7 * 3600000).toISOString(), labels: ['blocked'], modifiers: ['blocked'] }, stages, ctx);
assert.strictEqual(openWord(row), 'Open · blocked · since 7h', row);
assert.deepStrictEqual(segStates(row), ['active ham-open-blocked', T, T, T, T, T, 'todo cap'], 'blocked: its own class, no stale tint under the threshold');
assert.ok(strip(row).includes(' — carries the blocked label — open since '), 'chevron tooltip names the label and the instant: ' + strip(row));
assert.ok(tooltip(row).startsWith('Open · blocked'), tooltip(row));
row = stageTrackerRowHtml({ repo: 'acme/console', number: 26, kind: 'issue', title: 'Stuck long', stage: 0, stage_since: new Date(now - 3 * 86400000).toISOString(), modifiers: ['blocked'], blocked_on: 'acme/api#7' }, stages, ctx);
assert.strictEqual(openWord(row), 'Open · blocked · since 3d');
assert.deepStrictEqual(segStates(row), ['active ham-open-blocked ham-open-stale', T, T, T, T, T, 'todo cap'], 'blocked past STAGE_TRACKER_OPEN_STALE_DAYS: stale tint too');
assert.ok(strip(row).includes(' — blocked on acme/api#7 — '), 'a known blocker is named: ' + strip(row));
assert.strictEqual(stageTrackerBlockedTip({ blocked_on: ['acme/api#7', 'acme/api#8'] }), 'blocked on acme/api#7, acme/api#8');
assert.strictEqual(stageTrackerBlockedTip({}), 'carries the blocked label');
// A review-backlog spin-off blocked on a PR (hive:blocked-on marker, hivecommons/hive#14863): the pill names
// and links the PR, the tooltip says what lifts the block, the row title carries it too.
const blockedOnPR = { repo: 'hivecommons/hive', number: 14862, url: 'https://github.com/hivecommons/hive/pull/14862', state: 'open' };
row = stageTrackerRowHtml({ repo: 'hivecommons/hive', number: 14863, kind: 'issue', title: 'Review backlog: a', stage: 0, created_at: new Date(now - 7 * 3600000).toISOString(), labels: ['agent/reviewer', 'blocked'], modifiers: ['blocked'], caption: 'Blocked on PR #14862 — lifts when it merges', blocked_on_pr: blockedOnPR }, stages, ctx);
assert.strictEqual(openWord(row), 'Open · blocked · since 7h');
assert.ok(row.includes('<a class="ham-badge ham-badge-mod ham-mod-blocked" href="https://github.com/hivecommons/hive/pull/14862" target="_blank" rel="noopener" title="Blocked on PR hivecommons/hive#14862 (open) — lifts when it merges; re-triage if closed unmerged" aria-label="Blocked on PR hivecommons/hive#14862 (open) — lifts when it merges; re-triage if closed unmerged">blocked on #14862</a>'), 'pill links the blocker: ' + row);
assert.ok(strip(row).includes(' — blocked on hivecommons/hive#14862 — '), 'chevron names the blocker: ' + strip(row));
assert.ok(tooltip(row).includes('Blocked on PR hivecommons/hive#14862 (open) — lifts when it merges; re-triage if closed unmerged'), 'row title carries the blocker: ' + tooltip(row));
assert.ok(tooltip(row).includes('Blocked on PR #14862 — lifts when it merges'), 'caption in the row title: ' + tooltip(row));
assert.ok(!tooltip(row).includes('No stage recorded yet'), tooltip(row));
assert.strictEqual(stageTrackerBlockedOnPR({ repo: 'acme/console', blocked_on_pr: { repo: 'acme/api', number: 7, state: 'merged' } }).tip, 'Blocked on PR acme/api#7 (merged) — merged; the blocked sweep lifts the label next');
assert.strictEqual(stageTrackerBlockedOnPR({ repo: 'acme/console', blocked_on_pr: { repo: 'acme/api', number: 7 } }).short, 'acme/api#7', 'foreign repo keeps owner/repo');
assert.strictEqual(stageTrackerBlockedOnPR({ repo: 'console', blocked_on_pr: { repo: 'acme/console', number: 7, state: 'closed' } }).short, '#7', 'bare configured name matches its full repo');
assert.strictEqual(stageTrackerBlockedOnPR({ blocked_on_pr: { repo: 'acme/api', number: 0 } }), null);
assert.strictEqual(stageTrackerBlockedOnPR({}), null);
assert.strictEqual(stageTrackerBlockedTip({ blocked_on_pr: blockedOnPR }), 'blocked on hivecommons/hive#14862');
row = stageTrackerRowHtml({ repo: 'hivecommons/hive', number: 14868, kind: 'issue', title: 'No url', stage: 0, modifiers: ['blocked'], blocked_on_pr: { repo: 'hivecommons/hive', number: 14821 } }, stages, ctx);
assert.ok(row.includes('<span class="ham-badge ham-badge-mod ham-mod-blocked" title="Blocked on PR hivecommons/hive#14821 — lifts when it merges; re-triage if closed unmerged">blocked on #14821</span>'), 'without a url the pill still names the PR, as a span: ' + row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 29, kind: 'issue', title: 'Blocked no since', stage: 0, created_at: new Date(now - 3 * 86400000).toISOString(), modifiers: ['blocked'] }, stages, ctx);
assert.strictEqual(openWord(row), 'Open · blocked · since 3d', 'created_at fallback in the chevron');
assert.ok(row.includes('<span class="ham-since">3d</span>'), 'created_at fallback in the since column: ' + row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 30, kind: 'issue', title: 'Plain no since', stage: 0, created_at: new Date(now - 3 * 86400000).toISOString() }, stages, ctx);
assert.ok(!row.includes('<span class="ham-since">3d</span>'), 'only blocked rows borrow created_at for the since column');
row = stageTrackerRowHtml({ repo: 'acme/console', number: 27, kind: 'pr', title: 'Held and blocked', stage: 0, stage_since: new Date(now - 3 * 86400000).toISOString(), modifiers: ['blocked', 'hold'] }, stages, ctx);
assert.strictEqual(openWord(row), 'Open · hold', 'hold is a human instruction: it wins over blocked and still carries no age');
assert.deepStrictEqual(segStates(row), ['active ham-open-hold', T, T, T, T, T, 'todo cap']);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 28, kind: 'pr', title: 'Blocked later', stage: 50, modifiers: ['blocked'] }, stages, ctx);
assert.ok(!row.includes('ham-open-blocked') && !row.includes('Open · blocked'), 'blocked past Open is a chip, not an Open word');
// A draft PR with no stage (hivecommons/hive#14572): the far-right pill
// reads "draft" with the explanatory tooltip, the Open word reads "Draft",
// keeps its age but never tints stale — nothing is stuck, the lanes skip
// drafts until the author marks it ready.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 14427, kind: 'pr', title: 'WIP: image scanner', stage: 0, stage_since: new Date(now - 18 * 3600000).toISOString(), labels: ['do-not-merge/work-in-progress'], modifiers: ['draft'] }, stages, ctx);
assert.strictEqual(openWord(row), 'Draft · since 18h', row);
assert.deepStrictEqual(segStates(row), ['active ham-open-draft', T, T, T, T, T, 'todo cap'], 'draft: its own class, no hold/blocked class');
assert.ok(row.includes('ham-mod-draft" title="' + STAGE_TRACKER_DRAFT_TIP + '">draft</span>'), 'draft pill with tooltip: ' + row);
assert.ok(strip(row).includes(' — ' + STAGE_TRACKER_DRAFT_TIP + ' — open since '), 'chevron tooltip explains the draft: ' + strip(row));
assert.ok(tooltip(row).includes('Open · draft'), tooltip(row));
assert.ok(tooltip(row).includes('Flags: draft'), tooltip(row));
row = stageTrackerRowHtml({ repo: 'acme/console', number: 14428, kind: 'pr', title: 'Old draft', stage: 0, stage_since: new Date(now - 5 * 86400000).toISOString(), modifiers: ['draft'] }, stages, ctx);
assert.strictEqual(openWord(row), 'Draft · since 5d');
assert.deepStrictEqual(segStates(row), ['active ham-open-draft', T, T, T, T, T, 'todo cap'], 'a long-lived draft is not stale: it is waiting on its author');
assert.strictEqual(stageTrackerCurrentStageHtml({ stage: 0, stage_since: new Date(now - 18 * 3600000).toISOString(), modifiers: ['draft'] }, stages, null, now), '<p class="ham-current-stage">Draft · since 18h</p>', 'the compact caption carries it too');
// Draft plus a human hold: hold leads the pill ("hold +1") and the Open word,
// and the folded draft still explains itself on hover.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 31, kind: 'pr', title: 'Held draft', stage: 0, stage_since: new Date(now - 3 * 86400000).toISOString(), labels: ['do-not-merge/hold'], modifiers: ['hold', 'draft'] }, stages, ctx);
assert.strictEqual(openWord(row), 'Open · hold', 'hold wins the Open word over draft');
assert.deepStrictEqual(segStates(row), ['active ham-open-hold', T, T, T, T, T, 'todo cap']);
assert.ok(row.includes('ham-mod-hold" title="' + STAGE_TRACKER_DRAFT_TIP + '">hold +1</span>'), 'hold leads, draft folded: ' + row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 32, kind: 'pr', title: 'Still drafting', stage: 50, modifiers: ['draft'] }, stages, ctx);
assert.ok(!row.includes('ham-open-draft') && !row.includes('>Draft'), 'draft past Open is a chip, not an Open word: ' + row);
assert.ok(row.includes('ham-mod-draft"'), row);
assert.strictEqual(stageTrackerHeadline({ stage: 0, modifiers: ['draft'] }), 'Open · draft');
assert.strictEqual(stageTrackerHeadline({ stage: 0, modifiers: ['hold', 'draft'] }), 'Open · on hold', 'hold leads the headline too');
row = stageTrackerRowHtml({ repo: 'acme/console', number: 24, kind: 'issue', title: 'Staged', stage: 10, stage_since: new Date(now - 3 * 86400000).toISOString() }, stages, ctx);
assert.ok(!row.includes('since '), 'a staged item has no Open age');
assert.strictEqual(stageTrackerCurrentStageHtml({ stage: 0, stage_since: new Date(now - 14 * 3600000).toISOString() }, stages, null, now), '<p class="ham-current-stage">Open · since 14h</p>', 'the compact caption carries it too');

// Issue with exactly one covering PR (only its number known, legacy linked_pr):
// a single entry whose strip is driven by the PR; with no PR stage it lights
// PR in progress.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 8, kind: 'issue', url: 'https://github.com/acme/console/issues/8', stage: 100, mirrored: true, linked_pr: 102, caption: 'Covered by PR #102' }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, 'active', T, T, 'todo cap'], 'one covering PR: single entry');
assert.ok(tooltip(row).includes('Covering PR: #102'), tooltip(row));
assert.deepStrictEqual(prCol(row).length, 1, 'the covering PR sits in the PR column of the title line');
assert.strictEqual(subs(row).length, 0, 'no sub-rows for a single covering PR');
const prCell = (html) => (html.slice(0, html.indexOf('aria-label="Pipeline progress"')).match(/<li class="ham-seg ham-head-pr">([\s\S]*?)<\/li>/) || [])[1] || '';
assert.ok(prCell(row).startsWith(STAGE_TRACKER_LINKED_GLYPH + '<a class="ham-pr-ref"'), 'an in-row PR is led by the linked glyph: ' + prCell(row));
assert.ok(prCell(row).includes('title="Author: hive · ' + esc(STAGE_TRACKER_LINKED_TIP) + '"'), 'the PR link tooltip explains the shared row: ' + prCell(row));
// A PR listed by two open issues shows on both rows; every row but the
// lowest-numbered issue says where else it is.
const sharedCtx = Object.assign({}, ctx, { sharedPRs: stageTrackerSharedPRs([
  { kind: 'issue', repo: 'acme/console', number: 14175, linked_prs: [{ number: 14189 }] },
  { kind: 'issue', repo: 'acme/console', number: 14176, linked_prs: [{ number: 14189 }] },
  { kind: 'issue', repo: 'acme/console', number: 14177, linked_prs: [{ number: 14190 }] },
  { kind: 'pr', repo: 'acme/console', number: 14189 },
]) });
assert.deepStrictEqual(sharedCtx.sharedPRs, { 'acme/console#14189': [14175, 14176] }, 'only PRs listed by more than one issue');
const first = stageTrackerRowHtml({ repo: 'acme/console', number: 14175, kind: 'issue', title: 'A', stage: 20, linked_prs: [{ number: 14189, stage: 50 }] }, stages, sharedCtx);
const second = stageTrackerRowHtml({ repo: 'acme/console', number: 14176, kind: 'issue', title: 'B', stage: 20, linked_prs: [{ number: 14189, stage: 50 }] }, stages, sharedCtx);
assert.ok(prCell(first).includes(esc(STAGE_TRACKER_LINKED_TIP) + '"') && !prCell(first).includes('also under'), 'the lowest-numbered issue carries the plain hint');
assert.ok(prCell(second).includes(esc(STAGE_TRACKER_LINKED_TIP) + ' · also under #14175"'), 'the other row names the first: ' + prCell(second));
assert.strictEqual(stageTrackerLinkedTip({ repo: 'acme/console', number: 14176 }, 14189, sharedCtx), STAGE_TRACKER_LINKED_TIP + ' · also under #14175');
assert.strictEqual(stageTrackerLinkedTip({ repo: 'acme/console', number: 14175 }, 14189, sharedCtx), STAGE_TRACKER_LINKED_TIP);
assert.ok(!stageTrackerRowHtml({ repo: 'acme/console', number: 30, kind: 'pr', title: 'Alone', stage: 50 }, stages, sharedCtx).includes('ham-linked-ico'), 'a PR on its own row is not marked');
// With the PR's stage known the issue strip rides it (merge queue → Merge gate lit, hive move).
row = stageTrackerRowHtml({ repo: 'acme/console', number: 8, kind: 'issue', stage: 40, linked_prs: [{ number: 102, title: 'Fix it', author_kind: 'human', author: 'bob', stage: 100, stage_since: new Date(now - 7200000).toISOString(), modifiers: ['stalled'], caption: 'Admitted, merging' }] }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, 'active', 'todo cap']);
assert.ok(row.includes('<span class="ham-seg-t">Merging</span><svg class="ham-ico ham-actor-ico"') && row.includes('aria-label="Next move: hive"'), row);
assert.ok(row.includes('ham-mod-stalled">stalled</span>') && row.includes('<span class="ham-since ham-stalled">2h</span>'), 'chips + since come from the covering PR: ' + row);
assert.ok(tooltip(row).includes('Admitted, merging') && tooltip(row).includes('Covering PR: #102'), tooltip(row));
assert.ok(prCol(row)[0].includes('ham-author-human') && prCol(row)[0].includes('<span class="ham-pr-title">Fix it</span>'), prCol(row)[0]);
assert.strictEqual(subs(row).length, 0);
// Two covering PRs, one of unknown stage: that sub-row is a blank lead-in with nothing lit.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 8, kind: 'issue', stage: 40, linked_prs: [{ number: 102, stage: 100 }, { number: 104 }] }, stages, ctx);
assert.ok(subs(row)[1].includes('#104') && segStates(subs(row)[1]).every(st => st.startsWith('blank') || st === 'todo' || st === 'todo cap'), 'unknown PR stage: blank lead-in, nothing lit');
assert.ok(tooltip(row).includes('acme/console'), 'repo lives in the tooltip');
// Full PR row for 100: Merge gate lit as Merging with the hive actor icon (#15073 vocabulary).
row = stageTrackerRowHtml({ repo: 'acme/console', number: 102, kind: 'pr', stage: 100, author: 'hive-bot', author_kind: 'hive' }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, 'active', 'todo cap']);
assert.ok(strip(row).includes('title="Merge queue — admitted, merging · Next move: hive"'), 'chevron tooltip carries 100: ' + row);
assert.ok(text(row).includes('#102 Merging'), text(row));
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
row = stageTrackerRowHtml({ repo: 'acme/console', number: 102, kind: 'pr', title: 'Fix flaky', stage: 72, lane: 'contributor', tier: 'trusted', author: 'karibhou', author_kind: 'clanker', author_kind_source: 'inferred', modifiers: ['needs-rebase', 'needs-human'], caption: 'Returned to @karibhou', history: [{number:50,slug:'review-pending'},{number:60,slug:'reviewed'},{number:72,slug:'returned-to-contributor',from:60}] }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, 'active detoured', T, 'todo cap']);
// hivecommons/hive#15159: the chevron names the routing — the contributor's clanker, for a clanker's PR.
assert.ok(row.includes('<span class="ham-detour">Returned to @karibhou&#39;s clanker</span>'), row);
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
assert.ok(tooltip(row).includes('Flags: needs-rebase, needs-human') && tooltip(row).includes('Trail: CI / Review → Reviewed → Returned to contributor'), tooltip(row));

// PR reviewed (060): pre-admission (CI pending or soak), so CI / Review stays lit; the merge gate starts at 080.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 107, kind: 'pr', stage: 60 }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, 'active', T, 'todo cap'], '060 reviewed lights CI / Review, not the merge gate: ' + row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 109, kind: 'pr', stage: 80, caption: 'soak 20m left' }, stages, ctx);
assert.ok(row.includes('<span class="ham-seg-t">Ready for auto merge</span>') && strip(row).includes('title="Awaiting merge admission — not yet admitted — soak 20m left · Next move: hive"'), 'chevron tooltip carries 080: ' + row);

// PR regressed from the queue back to review-pending: Review lit, "Sent back" in the tooltip.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 103, kind: 'pr', stage: 70, modifiers: ['regressed', 'needs-rebase'], history: [{number:80,slug:'awaiting-merge-admission'},{number:100,slug:'merge-queue'},{number:70,slug:'fixing',from:100,regressed:true}] }, stages, ctx);
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
// A regression the hive is working through is not a call on a person: out of "Needs attention", no "Sent back" headline.
assert.ok(!stageTrackerNeedsAttention({ kind: 'issue', stage: 10, modifiers: ['regressed'], regressed_from: 40 }), 'regressed at Actionable is hive-handled');
assert.ok(!stageTrackerNeedsAttention({ kind: 'issue', stage: 30, modifiers: ['regressed', 'draft'], regressed_from: 40 }), 'draft is not a human gate');
assert.ok(stageTrackerNeedsAttention({ kind: 'issue', stage: 85, modifiers: ['regressed'], regressed_from: 100 }), 'regressed while parked on a person');
assert.ok(stageTrackerNeedsAttention({ kind: 'pr', stage: 70, modifiers: ['regressed', 'needs-rebase'], regressed_from: 100 }), 'regressed with a human-call modifier');
assert.notStrictEqual(stageTrackerHeadline({ kind: 'issue', stage: 10, modifiers: ['regressed'], regressed_from: 40 }), 'Sent back');
assert.strictEqual(stageTrackerHeadline({ kind: 'pr', stage: 70, modifiers: ['regressed', 'needs-rebase'], regressed_from: 100 }), 'Sent back');
// ...but an issue whose single covering PR sits on 085 still rides the PR's
// merge gate, with the "(its PR)" suffix.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 356, kind: 'issue', stage: 40, linked_prs: [{ number: 109, author_kind: 'hive', stage: 85 }] }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, 'active detoured', 'todo cap'], row);
assert.ok(tooltip(row).includes('Needs a human decision (its PR)'), row);
// A PR parked at 085 lights the 080 column as a detour; its chevron hover
// names the parking stage, not the column it rides on — the owner read
// "Awaiting merge admission … Next move: human" on hivecommons/hive#14821
// and #14822 while the engine still said 085.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 14821, kind: 'pr', stage: 85, caption: 'decision requested' }, stages, ctx);
const chevronTip = (r) => (/<li class="ham-seg active[^"]*"[^>]*title="([^"]*)" aria-current="step"/.exec(r) || [])[1] || '';
assert.ok(chevronTip(row).startsWith('Needs human decision') && !chevronTip(row).includes('Awaiting merge admission') && !chevronTip(row).includes('not yet admitted') && chevronTip(row).includes('Next move: human'), 'detour chevron names its own stage: ' + chevronTip(row));
row = stageTrackerRowHtml({ repo: 'acme/console', number: 14830, kind: 'pr', stage: 80, caption: 'CI green' }, stages, ctx);
assert.ok(chevronTip(row).startsWith('Awaiting merge admission — not yet admitted') && chevronTip(row).includes('Next move: hive'), '080 keeps its own hover: ' + chevronTip(row));
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
row = stageTrackerRowHtml({ repo: 'acme/console', number: 109, kind: 'pr', stage: 71, author: 'sam', author_kind: 'human', history: [{number:50,slug:'review-pending'},{number:71,slug:'needs-author',from:50}] }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, 'active detoured', T, 'todo cap']);
assert.ok(row.includes('aria-label="Next move: human"') && row.includes('<span class="ham-detour">Needs @sam</span><svg class="ham-ico ham-actor-ico"'), row);
assert.ok(tooltip(row).includes('Trail: CI / Review → Needs author'), tooltip(row));
// The trail names hops from the payload's {number, slug} entries (#14787
// showed "undefined → undefined → …" when it read a field the server never
// sent); the last STAGE_TRACKER_TRAIL_HOPS hops only; an unnamed hop is left
// out rather than printed as "undefined"; the clause is omitted when fewer
// than two hops can be named.
const live14787 = { kind: 'pr', number: 14787, stage: 85, modifiers: ['needs-human'], history: [
  { number: 50, slug: 'review-pending', at: '2026-10-27T21:27:53Z' }, { number: 85, slug: 'needs-decision', from: 50 },
  { number: 50, slug: 'review-pending', from: 85 }, { number: 71, slug: 'needs-author', from: 50 },
  { number: 50, slug: 'review-pending', from: 71 }, { number: 71, slug: 'needs-author', from: 50 },
  { number: 50, slug: 'review-pending', from: 71 }, { number: 85, slug: 'needs-decision', from: 50 }] };
assert.strictEqual(stageTrackerTrailText(live14787), 'CI / Review → Needs author → CI / Review → Needs human decision');
assert.ok(!stageTrackerTooltip(live14787, now).includes('undefined'), stageTrackerTooltip(live14787, now));
assert.strictEqual(stageTrackerTrailText({ history: [{ number: 50 }, { number: 999 }, { number: 85 }] }), 'CI / Review → Needs human decision', 'an unnamed hop is skipped');
assert.strictEqual(stageTrackerTrailText({ history: [{ number: 50 }, {}, { foo: 1 }] }), '', 'one nameable hop is no trail');
assert.strictEqual(stageTrackerTrailText({ history: [{ number: 50 }] }), '', 'a single hop is no trail');
assert.strictEqual(stageTrackerTrailText({ history: [] }), '');
assert.strictEqual(stageTrackerTrailText({}), '');
assert.strictEqual(stageTrackerTrailText({ history: [{ number: 0 }, { number: 10 }] }), 'Open → Actionable', '000 reads as Open');
assert.ok(!stageTrackerTooltip({ kind: 'pr', number: 1, stage: 50, history: [{ number: 999 }, { number: 998 }] }, now).includes('Trail:'), 'no Trail clause when nothing can be named');
const savedStages = _stageTracker;
_stageTracker = null;
assert.strictEqual(stageTrackerTrailText(live14787), 'review-pending → needs-author → review-pending → needs-decision', 'the slug stands in before the stage table loads');
assert.strictEqual(stageTrackerTrailText({ history: [{ number: 50 }, { number: 85 }] }), '', 'numbers alone, no table, no slug: nothing to name');
_stageTracker = savedStages;
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
assert.strictEqual(lead(row), '#13409 Usage on --help closed · this PR unmerged', lead(row));
assert.ok(/<div class="ham-head-main ham-issue-closed">/.test(row) && /<a class="ham-ref" href="https:\/\/x\/13409"[^>]*>#13409<\/a>/.test(row), 'closed issue stays a link in a struck-through slot: ' + row);
assert.ok(/<span class="ham-title" title="Usage on --help">Usage on --help<\/span> <span class="ham-badge ham-issue-closed-chip" title="Issue #13409 is closed, but this PR is still open and unmerged[^"]*">closed · this PR unmerged<\/span>/.test(row), 'title then closed chip naming the unmerged PR: ' + row);
assert.ok(!row.includes('ham-no-issue-t') && !/<a class="ham-ref"[^>]*title=/.test(row) && prCol(row).length === 1 && prCol(row)[0].includes('#111'), row);
// The same with no title resolvable: a muted "(closed issue)" placeholder, never a blank.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 111, kind: 'pr', title: 'Docs', stage: 95, linked_issues: [{ number: 13409, state: 'closed', url: 'https://x/13409' }] }, stages, ctx);
assert.strictEqual(lead(row), '#13409 (closed issue) closed · this PR unmerged', lead(row));
assert.ok(/<span class="ham-title ham-no-issue-t">\(closed issue\)<\/span> <span class="ham-badge ham-issue-closed-chip" title="[^"]*">closed · this PR unmerged<\/span>/.test(row), row);
// hivecommons/hive#15232: a PR whose closing link names a closed issue
// (closed_issue set by the server, hivecommons/hive#14655) still puts the
// ISSUE in the left slot — struck through under its chip — and itself in the
// PR column, exactly like the soft-linked case above; the PR never leads.
// When the server names the merged PR that closed the issue, the chip reads
// "closed by #N" and links to that PR.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 15191, kind: 'pr', title: 'Let hive decide', url: 'https://x/acme/console/pull/15191', stage: 90, author: 'clubanderson', author_kind: 'human',
  linked_issues: [{ number: 15173, state: 'closed', state_reason: 'completed', title: 'Sweep merges approved PRs', url: 'https://x/acme/console/issues/15173', closed_by: 15181 }],
  closed_issue: { number: 15173, state: 'closed', state_reason: 'completed', title: 'Sweep merges approved PRs', url: 'https://x/acme/console/issues/15173', closed_by: 15181 } }, stages, ctx);
assert.strictEqual(lead(row), '#15173 Sweep merges approved PRs closed by #15181', 'closed issue leads the row, with its closer: ' + lead(row));
assert.ok(/<div class="ham-head-main ham-issue-closed">/.test(row) && /<a class="ham-ref" href="https:\/\/x\/acme\/console\/issues\/15173"[^>]*>#15173<\/a>/.test(row), 'struck-through issue link in the slot: ' + row);
assert.ok(/<a class="ham-badge ham-issue-closed-chip ham-closed-by-chip" href="https:\/\/x\/acme\/console\/pull\/15181"[^>]*>closed by #15181<\/a>/.test(row), 'closed-by chip links the merged PR: ' + row);
assert.ok(!row.includes('ham-head-pr-anchor') && !row.includes('closes <s>'), 'the PR no longer takes the issue slot: ' + row);
assert.ok(prCol(row).length === 1 && prCol(row)[0].includes('#15191') && prCol(row)[0].includes('Let hive decide'), 'PR sits in the PR column: ' + prCol(row));
assert.ok(!row.includes('ham-closed-not-planned'), 'completed close: no moot chip');
// Closer unknown: the chip says the issue closed while this PR is unmerged
// (never a bare "closed" that reads as closed-by-this-PR); a not-planned
// close adds the moot chip.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 15191, kind: 'pr', title: 'Let hive decide', stage: 90,
  linked_issues: [{ number: 15173, state: 'closed', state_reason: 'not_planned', title: 'Sweep', url: 'https://x/15173' }],
  closed_issue: { number: 15173, state: 'closed', state_reason: 'not_planned', title: 'Sweep', url: 'https://x/15173' } }, stages, ctx);
assert.strictEqual(lead(row), '#15173 Sweep closed · this PR unmerged issue closed (not planned)', lead(row));
assert.ok(/<span class="ham-badge ham-issue-closed-chip" title="[^"]*">closed · this PR unmerged<\/span> <span class="ham-badge ham-issue-closed-chip ham-closed-not-planned"/.test(row), row);
// A closer equal to the row's own PR is not a duplicate: the unmerged chip.
assert.ok(/^<span class="ham-badge ham-issue-closed-chip" title="Issue #496 is closed, but this PR is still open and unmerged[^"]*">closed · this PR unmerged<\/span>$/.test(stageTrackerClosedChipHtml({ repo: 'acme/console', number: 500 }, { number: 496, closed_by: 500, title: 'T' })));
assert.deepStrictEqual(stageTrackerPRIssueSlot({ number: 500, linked_issues: [{ number: 496, state: 'closed', title: 'C', url: 'u', closed_by: 501 }] }), { number: 496, title: 'C', url: 'u', closed: true, placeholder: '', closed_by: 501 }, 'closed_by rides the slot');
assert.strictEqual(stageTrackerItemURL('acme/console', 501, 'pull', 'https://ghe.example/acme/console/pull/500'), 'https://ghe.example/acme/console/pull/501', 'closer URL follows the sibling host');
assert.strictEqual(stageTrackerItemURL('acme/console', 501, 'pull', ''), 'https://github.com/acme/console/pull/501');
// A linked issue of unknown state that is not listed still goes in the issue slot as a link, no chip; a missing title is said so.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 112, kind: 'pr', title: 'Fix', stage: 50, linked_issues: [{ number: 77, url: 'https://x/77' }] }, stages, ctx);
assert.strictEqual(lead(row), '#77 (title unavailable)', lead(row));
assert.ok(!row.includes('ham-issue-closed'), 'no closed marker on an issue of unknown state: ' + row);
assert.deepStrictEqual(stageTrackerPRIssueSlot({ linked_issues: [{ number: 5, state: 'closed' }, { number: 6, state: 'open', title: 'T' }] }), { number: 6, title: 'T', url: '' }, 'an open linked issue beats a closed one');
assert.deepStrictEqual(stageTrackerPRIssueSlot({ linked_issues: [{ number: 4 }, { number: 5, state: 'closed', title: 'C', url: 'u' }] }), { number: 5, title: 'C', url: 'u', closed: true, placeholder: '' }, 'a known-closed issue beats one of unknown state');
assert.deepStrictEqual(stageTrackerPRIssueSlot({ linked_issues: [{ number: 5, state: 'closed' }] }), { number: 5, title: '', url: '', closed: true, placeholder: STAGE_TRACKER_CLOSED_ISSUE_TITLE });
assert.deepStrictEqual(stageTrackerPRIssueSlot({}), { number: 0, text: 'no linked issue' });
// Soft links (hivecommons/hive#13544): a Refs #N / (#N, part 1) reference fills the issue slot under a "refs" chip; a closing link never wears one.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 13544, kind: 'pr', title: 'Model heads (#13542, part 1)', stage: 50, nested_under: 13542, linked_issues: [{ number: 13542, title: 'Split the reviewer', url: 'https://x/13542', state: 'open', listed: true, via: 'refs' }] }, stages, ctx);
assert.strictEqual(lead(row), '#13542 Split the reviewer refs', lead(row));
assert.ok(row.includes('<span class="ham-badge ham-issue-refs-chip" title="' + STAGE_TRACKER_SOFT_LINK_TIP + '">refs</span>'), 'refs chip: ' + row);
assert.ok(!row.includes('ham-no-issue'), 'no placeholder once a soft link resolves: ' + row);
assert.deepStrictEqual(stageTrackerPRIssueSlot({ linked_issues: [{ number: 6, state: 'open', title: 'T', via: 'title' }] }), { number: 6, title: 'T', url: '', soft: true });
assert.deepStrictEqual(stageTrackerPRIssueSlot({ linked_issues: [{ number: 6, via: 'mention' }] }), { number: 6, title: '', url: '', closed: false, placeholder: STAGE_TRACKER_UNTITLED_ISSUE, soft: true });
assert.deepStrictEqual(stageTrackerPRIssueSlot({ linked_issues: [{ number: 6, state: 'open', title: 'T', via: 'fixes' }] }), { number: 6, title: 'T', url: '' }, 'a hive-read closing keyword is not soft');
row = stageTrackerRowHtml({ repo: 'acme/console', number: 113, kind: 'pr', title: 'Fix', stage: 50, linked_issues: [{ number: 77, state: 'open', title: 'Closed by it' }] }, stages, ctx);
assert.ok(!row.includes('ham-issue-refs-chip'), 'closing link draws no refs chip: ' + row);

// Refs-only issue coverage shows tracker progress and the covering PR's base.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 9, kind: 'issue', title: 'Tracker', stage: 40, claim_reference: true, is_tracker: true, children: { total: 3, completed: 1 }, linked_prs: [{ number: 203, title: 'Draft work', stage: 10, base_ref: 'v6' }] }, stages, ctx);
assert.ok(row.includes('<span class="ham-badge ham-issue-refs-chip" title="' + STAGE_TRACKER_SOFT_LINK_TIP + '">refs</span>'), 'issue refs chip: ' + row);
assert.ok(row.includes('<span class="ham-badge">tracker 1/3</span>'), 'tracker progress: ' + row);
assert.ok(row.includes('<span class="ham-badge">v6</span>'), 'covering PR base: ' + row);

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
assert.ok(sr[0].includes('<span class="ham-detour">Fixing</span><svg class="ham-ico ham-actor-ico"') && sr[0].includes('aria-label="Next move: human"') && sr[0].indexOf('<span class="ham-since">1h</span>') > sr[0].indexOf('aria-label="Pipeline progress"') && sr[0].includes('ham-mod-needs-rebase">needs rebase</span>'), 'Fixing is the author\'s move; the chip says what is being fixed: ' + sr[0]);
assert.ok(/^ data-ham-kind="pr" data-ham-stage="70" data-ham-sub-key="acme\/console\/pr#102">/.test(sr[0]) && /<a class="ham-pr-ref"[^>]*title="Fix it · Fixing review findings Rebasing onto main/.test(sr[0]), 'sub-row tooltip rides the PR link, not the container: ' + sr[0].slice(0, 400));
assert.ok(prCol(sr[1])[0].includes('ham-author-clanker') && prCol(sr[1])[0].includes('#103'));
assert.deepStrictEqual(segStates(sr[1]), [B, B, B, 'active', T, T, 'todo cap'], 'a PR at 030 lights PR in progress on its own sub-row');
assert.ok(!row.slice(0, row.indexOf('<div class="ham-sub"')).includes('ham-pr-ref'), 'the issue title line itself lists no PRs');
// One-line sub-row header: connector, author icon + #N + title, lineage chip
// and base pill are direct siblings inside the single flex-row PR cell — no
// block wrapper or <br> between them — and the title carries its full text
// as hover for the ellipsis.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 9, kind: 'issue', title: 'Three PRs', stage: 40, linked_prs: [
  { number: 15343, title: 'docs: remove duplicate README run routes', author_kind: 'hive', stage: 50, base_ref: 'v5', lineage: { kind: 'adopted', from_number: 15226, from_author: 'x', from_author_kind: 'clanker' } },
  { number: 15344, title: 'other', stage: 30 },
] }, stages, ctx);
sr = subs(row);
{
  const head = sr[0].slice(0, sr[0].indexOf('aria-label="Pipeline progress"'));
  const cell = (head.match(/<li class="ham-seg ham-head-pr">([\s\S]*?)<\/li>/) || [])[1] || '';
  assert.ok(cell, 'sub-row header has one PR cell: ' + head);
  assert.ok(!/<br|<div|<p\b/.test(cell), 'no block-level element inside the PR cell: ' + cell);
  const order = [cell.indexOf('class="ham-linked-ico"'), cell.indexOf('<a class="ham-pr-ref"'), cell.indexOf('class="ham-badge ham-lineage"'), cell.lastIndexOf('<span class="ham-badge">v5</span>')];
  assert.ok(order.every((i, k) => i >= 0 && (k === 0 || i > order[k - 1])), 'connector, PR link, lineage chip, base pill in that order as siblings: ' + cell);
  assert.ok(cell.indexOf('<span class="ham-badge">v5</span>') > cell.indexOf('</a>', cell.indexOf('<a class="ham-pr-ref"')), 'the base pill is a sibling after the PR link, not inside it: ' + cell);
  assert.ok(/<a class="ham-pr-ref"[^>]*title="docs: remove duplicate README run routes · /.test(cell), 'the PR link hover starts with the full title: ' + cell);
  assert.ok(!head.includes('ham-lineage" title') || head.indexOf('ham-lineage') < head.indexOf('<li class="ham-seg cap">'), 'the lineage chip rides the PR cell, not the chip cap: ' + head);
  assert.strictEqual((head.match(/ham-lineage/g) || []).length, 1, 'the lineage chip is drawn once');
}
// The same order on an issue row with a single covering PR: lineage chip
// before the base pill, both after the PR link.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 9, kind: 'issue', title: 'One PR', stage: 40, linked_prs: [{ number: 15343, title: 'T', stage: 50, base_ref: 'v5', lineage: { kind: 'adopted', from_number: 15226 } }] }, stages, ctx);
{
  const cell = prCell(row);
  assert.ok(cell.indexOf('<a class="ham-pr-ref"') < cell.indexOf('ham-lineage') && cell.indexOf('ham-lineage') < cell.indexOf('<span class="ham-badge">v5</span>'), 'PR link, lineage, base pill: ' + cell);
}
// An incomplete lineage has no chip and must not add an extra separator.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 9, kind: 'issue', title: 'Empty lineage', stage: 40, linked_prs: [{ number: 15343, title: 'T', stage: 50, base_ref: 'v5', lineage: { kind: 'adopted' } }] }, stages, ctx);
assert.match(prCell(row), /<\/a> <span class="ham-badge">v5<\/span>/, 'empty lineage leaves one separator before the base pill');
assert.ok(!prCell(row).includes('ham-lineage'), 'empty lineage renders no chip');
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
assert.deepStrictEqual(Array.from(head.matchAll(/<span class="ham-seg-t">(?:<svg[\s\S]*?<\/svg>)?([^<]*)<\/span>/g)).map(m => m[1]), ['Open', 'Actionable', 'Claimed', 'PR in progress', 'CI / Review', 'Merge queue']);
// The Merge queue column wears the train icon (hover "Merge queue"), the
// same railcar as the train header chip and the 080 chevron (#15395).
assert.ok(head.includes('<span class="ham-seg-t">' + stageTrackerTrainIconHtml('ham-ico ham-train-icon ham-col-ico') + 'Merge queue</span>'), head);
assert.strictEqual((head.match(/<title>Merge queue<\/title>/g) || []).length, 1);
assert.ok(head.includes('<span class="ham-since">since</span>'));

// Unknown / zero stage lights "Open".
row = stageTrackerRowHtml({ repo: 'acme/console', number: 1, kind: 'issue', stage: 0 }, stages, ctx);
assert.deepStrictEqual(segStates(row), ['active', T, T, T, T, T, 'todo cap']);

// Every PR stage has an active word and an actor on its segment; the word
// is what the chevron says, the actor is whose move it is. The active
// chevron now leads with its stage icon, so the regex skips that svg.
const prWords = {};
for (const [st, word, actor] of [[50, 'CI / Review', 'hive'], [60, 'Review passed', 'hive'], [70, 'Fixing', 'author'], [71, 'Needs author', 'author'], [72, 'Returned to contributor', 'author'], [74, 'Adopted by hive', 'hive'], [80, 'Ready for auto merge', 'hive'], [85, 'Needs human decision', 'human'], [90, 'Ready for human merge', 'human'], [100, 'Merging', 'hive']]) {
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
assert.strictEqual(stageTrackerCurrentStageHtml({ repo: 'acme/console', number: 560, kind: 'pr', stage: 60 }, stages), '<p class="ham-current-stage">Review passed · Next move: hive</p>', 'no detour → column word only');
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
assert.ok(/<a class="ham-pr-ref"[^>]*title="The one PR Author: hive · @hive-bot · linked PR, shown on its issue&#39;s row"/.test(row), 'the PR link also explains the shared row: ' + row);
// tracker-cues: with no agent on the item the chevron is static (waiting);
// the pulse and the "Working now" lead need a worker in ctx.workers.
assert.ok(/<li class="ham-seg active" title="Review pending · Next move: hive" aria-current="step">/.test(row), 'active chevron: stage + whose move (nobody working: static): ' + row);
row = stageTrackerRowHtml(g[1], stages, Object.assign({}, ctx, { workers: { 'g/r#21': { agent: 'scout', action: 'agent_comment_created', at: new Date(now - 120000).toISOString() } } }));
assert.ok(/<li class="ham-seg active" data-motion="active" title="Working now: scout commented · 2m ago — Review pending · Next move: hive" aria-current="step">/.test(row), 'active chevron: working lead + pulse when an agent is on the nested PR: ' + row);
assert.ok(/class="ham-row cue-working"/.test(row), 'the row carries the working cue: ' + row);
assert.ok(!/<li class="ham-seg active" data-motion=[^>]*title="Claimed"/.test(stageTrackerRowHtml(g[0], stages, ctx)), 'Claimed is static: no data-motion');
// Issue with two PRs: #N link + title on the issue line, nothing lit there;
// each sub-row has its PR link (full PR tooltip) and its active chevron.
row = stageTrackerRowHtml(g[3], stages, ctx);
expectTitles(row.split('<div class="ham-sub"')[0], ['a.ham-ref', 'span.ham-title'], 'two-PR issue line');
for (const s of subs(row)) expectTitles(s, ['a.ham-pr-ref', 'li.ham-seg'], 'sub-row');
assert.ok(/<a class="ham-pr-ref"[^>]*title="First PR · Back with the contributor[^"]*@bo \(agent\)[^"]*"/.test(subs(row)[1]), 'sub-row PR link: PR title, then the PR tooltip: ' + subs(row)[1]);
// Standalone PR: the PR link (its title, then the row tooltip) and the active
// chevron; the placeholder issue slot carries none.
row = stageTrackerRowHtml(g[6], stages, ctx);
expectTitles(row, ['a.ham-pr-ref', 'li.ham-seg'], 'standalone PR');
assert.ok(/<a class="ham-pr-ref"[^>]*title="Standalone PR · Not yet admitted to auto merge[^"]*@cy \(person\)/.test(row), 'PR link: title, then the row tooltip: ' + row);
assert.ok(row.includes('aria-label="human · @cy"') && !row.includes('<title>'), 'author icon label is aria-only: ' + row);
// Since cells are untouched: plain text, no title.
assert.ok(/<span class="ham-since( ham-stalled)?">[^<]*<\/span>/.test(row) && !/ham-since[^>]*title=/.test(golden), 'since cells unchanged');
// Merged: the lit cap is the only chevron with hover text.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 106, kind: 'pr', stage: 110 }, stages, ctx);
assert.ok(/<li class="ham-seg active cap merged" title="Merged — [^"]*" aria-current="step">/.test(row) && titled(strip(row)).length === 1, row);
`
	// Over stdin: the lifted fixture exceeds Linux's per-argument limit.
	cmd := exec.Command(node, "-")
	cmd.Stdin = strings.NewReader(script)
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
