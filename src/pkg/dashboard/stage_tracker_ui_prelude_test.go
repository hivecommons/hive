package dashboard

import (
	"strings"
	"testing"
)

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
		jsConstLine(t, html, "STAGE_TRACKER_COVERED_BY_PR"),
		jsFunc(t, html, "stageTrackerLeadStage"),
		jsFunc(t, html, "stageTrackerLead"),
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
		jsFunc(t, html, "stageTrackerIsReviewNote"),
		jsFunc(t, html, "stageTrackerFoldedReviewNotes"),
		jsFunc(t, html, "stageTrackerReviewNotesHtml"),
		jsFunc(t, html, "stageTrackerFoldedCount"),
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
