package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestStageTrackerRepoGroupingWiring pins the repo chip / group-by-repo
// surface: the Group select beside Sort, the settings-modal Repositories
// section, the hive.tracker.* storage keys, the token palette the chips
// tint from and the render path that keys group headers for the reconcile.
func TestStageTrackerRepoGroupingWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`const STAGE_TRACKER_REPO_CHIP_KEY = 'hive.tracker.repochip';`,
		`const STAGE_TRACKER_REPO_CHIP_DEFAULT = 'multi';`,
		`const STAGE_TRACKER_GROUP_KEY = 'hive.tracker.group';`,
		`const STAGE_TRACKER_GROUP_COLLAPSED_KEY = 'hive.tracker.group.collapsed';`,
		`const STAGE_TRACKER_GROUP_DEFAULT = 'none';`,
		`const STAGE_TRACKER_GROUP_KEY_PREFIX = 'group:';`,
		`const STAGE_TRACKER_REPO_TINTS = 6;`,
		`id="activity-monitor-group" class="ham-select" data-change-action="hamGroup" data-arg-types="v"`,
		`<option value="none">None</option><option value="repo">Repo</option>`,
		`setVal('activity-monitor-group', _stageTrackerGroup);`,
		`id="sts-repos"`,
		`data-change-action="stageTrackerRepoChipChange" data-arg-types="v"`,
		`id="sts-group" data-change-action="hamGroup" data-arg-types="v"`,
		`+ stageTrackerRepoSectionHtml(state.repoChip, state.group)`,
		`repoChip: stageTrackerRepoChipSetting(), group: _stageTrackerGroup }));`,
		`function stageTrackerRepoChipHtml(repo, ctx)`,
		`function stageTrackerGroups(rows, opts)`,
		`function stageTrackerGroupPage(groups, f)`,
		`function stageTrackerGroupHeaderHtml(g, stages, ctx)`,
		`function hamRepoChip(repo)`,
		`function hamGroupToggle(repo)`,
		`function hamGroup(value)`,
		`if (!_stageTrackerSig && !_stageTrackerEtag) stageTrackerLoadGrouping();`,
		`if (stageTrackerRepoChipOn(stageTrackerRepoChipSetting(), repos.length)) ctx.repoChips = { primary: stageTrackerPrimaryRepo(), active: f.repo };`,
		`? { key: e.group.key, html: stageTrackerGroupHeaderHtml(e.group, stages, ctx) }`,
		`.ham-repo-chip {`,
		`.ham-repo-chip.ham-repo-primary {`,
		`.ham-group {`,
		`.ham-group.ham-leaving {`,
		`.ham-group.ham-entering {`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing repo chip / grouping wiring %q", want)
		}
	}
	tokens := tokensCSS(t)
	for i := 0; i < 6; i++ {
		slot := string(rune('0' + i))
		if !strings.Contains(html, ".ham-repo-tint-"+slot+" { --ham-repo-tint: var(--ham-repo-tint-"+slot+"); }") {
			t.Errorf("index.html lacks the palette class for slot %s", slot)
		}
		if !strings.Contains(tokens, "--ham-repo-tint-"+slot+": var(--") {
			t.Errorf("tokens.css lacks --ham-repo-tint-%s", slot)
		}
	}
	// The chip and group CSS rides tokens only (ADR-0018): no literal colour.
	start := strings.Index(html, "/* Repo chip before the number:")
	end := strings.Index(html, ".ham-badges { position: absolute;")
	if start < 0 || end < start {
		t.Fatal("repo chip CSS block not found")
	}
	if css := html[start:end]; strings.Contains(css, "#") || strings.Contains(css, "rgb(") || strings.Contains(css, "rgba(") {
		t.Errorf("repo chip / group CSS must use tokens only, found a literal colour:\n%s", css)
	}
}

// runNodeStdin feeds the script to node over stdin: the lifted harnesses
// are past Linux's per-argument limit for -e.
func runNodeStdin(node, script string) ([]byte, error) {
	cmd := exec.Command(node, "-")
	cmd.Stdin = strings.NewReader(script)
	return cmd.CombinedOutput()
}

func stageTrackerRepoGroupingHarness(t *testing.T, html string) string {
	t.Helper()
	return strings.Join([]string{
		"const assert = require('assert');",
		"const window = { _primaryRepo: 'hivecommons/hive' };",
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
		jsConstLine(t, html, "STAGE_TRACKER_REPO_CHIP_KEY"),
		jsConstBlock(t, html, "STAGE_TRACKER_REPO_CHIP_MODES"),
		jsConstLine(t, html, "STAGE_TRACKER_REPO_CHIP_DEFAULT"),
		jsConstLine(t, html, "STAGE_TRACKER_GROUP_KEY"),
		jsConstLine(t, html, "STAGE_TRACKER_GROUP_COLLAPSED_KEY"),
		jsConstLine(t, html, "STAGE_TRACKER_GROUPS"),
		jsConstLine(t, html, "STAGE_TRACKER_GROUP_DEFAULT"),
		jsConstLine(t, html, "STAGE_TRACKER_GROUP_KEY_PREFIX"),
		jsConstLine(t, html, "STAGE_TRACKER_REPO_TINTS"),
		jsConstLine(t, html, "STAGE_TRACKER_GROUP_DIST_STEPS"),
		"var _stageTrackerGroup = STAGE_TRACKER_GROUP_DEFAULT; var _stageTrackerCollapsed = {};",
		jsFunc(t, html, "stageTrackerDefaultFilter"),
		"var _stageTrackerFilter = stageTrackerDefaultFilter();",
		jsFunc(t, html, "stageTrackerWaitSince"),
		jsFunc(t, html, "stageTrackerLabelFamily"),
		stageTrackerActedJS(t, html),
		jsFunc(t, html, "stageTrackerNormalizeFilter"),
		jsFunc(t, html, "stageTrackerFilterActive"),
		jsFunc(t, html, "stageTrackerMatches"),
		jsFunc(t, html, "stageTrackerSort"),
		jsFunc(t, html, "stageTrackerPage"),
		jsFunc(t, html, "stageTrackerIcon"),
		jsFunc(t, html, "stageTrackerPRRef"),
		jsConstLine(t, html, "STAGE_TRACKER_ACTOR_NONE"),
		jsConstLine(t, html, "STAGE_TRACKER_NOBODY_LEAD"),
		jsFunc(t, html, "stageTrackerStageActor"),
		jsFunc(t, html, "stageTrackerActorKind"),
		jsFunc(t, html, "stageTrackerActorTip"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_STATIC"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_ACTIVE"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_HUMAN"),
		jsConstBlock(t, html, "STAGE_TRACKER_MOTION_BY_STAGE"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_AUTHOR_STAGES"),
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_HUMAN_MODS"),
		jsFunc(t, html, "stageTrackerMotion"),
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
		jsFunc(t, html, "stageTrackerShortAgo"),
		jsFunc(t, html, "stageTrackerRefOf"),
		jsFunc(t, html, "stageTrackerRowToolsHtml"),
		jsFunc(t, html, "stageTrackerRowHtml"),
		jsFunc(t, html, "stageTrackerPRIssueSlot"),
		jsFunc(t, html, "stageTrackerClosedChipHtml"),
		jsFunc(t, html, "stageTrackerSplitTitleTag"),
		jsFunc(t, html, "stageTrackerTitleHtml"),
		jsFunc(t, html, "stageTrackerItemURL"),
		jsFunc(t, html, "stageTrackerItemKey"),
		jsFunc(t, html, "stageTrackerMoveAttrs"),
		jsFunc(t, html, "stageTrackerNeedsAttention"),
		jsFunc(t, html, "stageTrackerVisible"),
		jsFunc(t, html, "stageTrackerIsReviewNote"),
		jsFunc(t, html, "stageTrackerFoldedReviewNotes"),
		jsFunc(t, html, "stageTrackerReviewNotesHtml"),
		jsFunc(t, html, "stageTrackerFoldedCount"),
		jsFunc(t, html, "stageTrackerRepoTint"),
		jsFunc(t, html, "stageTrackerRepoShort"),
		jsFunc(t, html, "stageTrackerIsPrimary"),
		jsFunc(t, html, "stageTrackerRepoChipOn"),
		jsFunc(t, html, "stageTrackerRepoChipHtml"),
		jsFunc(t, html, "stageTrackerColName"),
		jsFunc(t, html, "stageTrackerGroups"),
		jsFunc(t, html, "stageTrackerGroupPage"),
		jsFunc(t, html, "stageTrackerGroupHeaderHtml"),
		jsFunc(t, html, "stageTrackerRepoChipSetting"),
		jsFunc(t, html, "stageTrackerSetRepoChip"),
		// No am.* URL view (#15924): the group persists in localStorage.
		"var _stageTrackerUrlView = null;",
		jsFunc(t, html, "stageTrackerUrlView"),
		jsFunc(t, html, "stageTrackerGroupSetting"),
		jsFunc(t, html, "stageTrackerSetGroup"),
		jsFunc(t, html, "stageTrackerLoadCollapsed"),
		jsFunc(t, html, "stageTrackerSaveCollapsed"),
		jsFunc(t, html, "stageTrackerToggleCollapsed"),
		jsFunc(t, html, "stageTrackerLoadGrouping"),
		jsFunc(t, html, "stageTrackerPrimaryRepo"),
		jsFunc(t, html, "stageTrackerRepoSectionHtml"),
	}, "\n") + `
const now = Date.parse('2026-01-01T12:00:00Z');
const stages = [
  { number: 10, label: 'Actionable', short: 'Actionable' }, { number: 15, label: 'Triage needed', short: 'Triage needed' },
  { number: 20, label: 'Claimed', short: 'Claimed' }, { number: 30, label: 'PR in progress', short: 'PR in progress' },
  { number: 40, label: 'Covered by PR', short: 'Covered by PR' }, { number: 45, label: 'Needs human decision', short: 'Needs human decision' },
  { number: 50, label: 'Review pending', short: 'CI / Review' }, { number: 60, label: 'Reviewed', short: 'Reviewed' },
  { number: 70, label: 'Fixing', short: 'Fixing', branch_from: 50 }, { number: 71, label: 'Needs author', short: 'Needs author', branch_from: 50 },
  { number: 80, label: 'Awaiting merge admission', short: 'Merge gate' }, { number: 85, label: 'Needs human decision', short: 'Needs human decision', branch_from: 80 },
  { number: 90, label: 'Ready for human merge', short: 'Ready for human merge', branch_from: 80 },
  { number: 100, label: 'Merge queue', short: 'Merge queue' }, { number: 110, label: 'Merged', short: 'Merged' },
];
const P = (repo, n, stage, extra) => Object.assign({ kind: 'pr', repo, number: n, stage, title: 'PR ' + n, url: 'https://x/' + n, author: 'ann', author_kind: 'human', stage_since: '2026-01-01T11:00:00Z', created_at: '2026-01-01T10:00:00Z', modifiers: [] }, extra || {});
const I = (repo, n, stage, extra) => Object.assign({ kind: 'issue', repo, number: n, stage, title: 'Issue ' + n, url: 'https://x/' + n, stage_since: '2026-01-01T11:00:00Z', created_at: '2026-01-01T10:00:00Z', modifiers: [] }, extra || {});
`
}

// TestStageTrackerRepoChip runs the chip under node: text is the short repo
// name, owner/repo rides the tooltip, the primary repo's chip is muted, the
// palette slot is deterministic and in range, a chip sits before the issue
// slot's #N on issue and PR rows and before each nested PR's link, no chip
// is drawn without ctx.repoChips, and the default setting shows chips only
// when the payload spans several repos.
func TestStageTrackerRepoChip(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — repo chip was NOT executed by this run")
	}
	html := indexHTML(t)
	script := stageTrackerRepoGroupingHarness(t, html) + `
// setting → shown?
assert.strictEqual(stageTrackerRepoChipOn('multi', 1), false, 'default: one repo, no chip');
assert.strictEqual(stageTrackerRepoChipOn('multi', 2), true, 'default: two repos, chip');
assert.strictEqual(stageTrackerRepoChipOn('always', 1), true);
assert.strictEqual(stageTrackerRepoChipOn('never', 3), false);
assert.strictEqual(stageTrackerRepoChipOn(undefined, 1), false, 'unknown mode behaves like the default');

// short name / primary matching / tint
assert.strictEqual(stageTrackerRepoShort('hivecommons/dibs'), 'dibs');
assert.strictEqual(stageTrackerRepoShort('dibs'), 'dibs');
assert.ok(stageTrackerIsPrimary('hivecommons/hive', 'hivecommons/hive'));
assert.ok(stageTrackerIsPrimary('hivecommons/hive', 'hive'), 'a bare primary name matches the short name');
assert.ok(!stageTrackerIsPrimary('hivecommons/dibs', 'hivecommons/hive'));
assert.ok(!stageTrackerIsPrimary('hivecommons/hive', ''));
for (const r of ['hivecommons/hive', 'hivecommons/dibs', 'hivecommons/pluk', 'hivecommons/docs', 'hivecommons/homebrew-hive', '']) {
  const s = stageTrackerRepoTint(r);
  assert.ok(Number.isInteger(s) && s >= 0 && s < STAGE_TRACKER_REPO_TINTS, r + ' slot ' + s);
  assert.strictEqual(stageTrackerRepoTint(r), s, 'deterministic');
}

// the chip itself
const ctx = { nowMs: now, repoChips: { primary: 'hivecommons/hive', active: '' } };
const chip = stageTrackerRepoChipHtml('hivecommons/dibs', ctx);
assert.ok(/^<button type="button" class="ham-repo-chip ham-repo-tint-\d"/.test(chip), chip);
assert.ok(chip.includes('>dibs</button>'), 'short name is the text: ' + chip);
assert.ok(chip.includes('title="hivecommons/dibs — click to show only this repo"'), 'owner/repo in the tooltip: ' + chip);
assert.ok(chip.includes('data-action="hamRepoChip" data-arg0="hivecommons/dibs"'), 'click filters to the repo: ' + chip);
assert.ok(chip.includes('aria-pressed="false"'));
assert.ok(!chip.includes('ham-repo-primary'));
const prim = stageTrackerRepoChipHtml('hivecommons/hive', ctx);
assert.ok(prim.includes(' ham-repo-primary"') && prim.includes('>hive</button>') && prim.includes('title="hivecommons/hive (primary repo) — click to show only this repo"'), 'primary chip is muted but still shown: ' + prim);
const active = stageTrackerRepoChipHtml('hivecommons/dibs', { repoChips: { primary: '', active: 'hivecommons/dibs' } });
assert.ok(active.includes('aria-pressed="true"') && active.includes('showing only this repo; click to show all repos'), active);
assert.strictEqual(stageTrackerRepoChipHtml('hivecommons/dibs', { nowMs: now }), '', 'no ctx.repoChips → no chip');
assert.strictEqual(stageTrackerRepoChipHtml('', ctx), '', 'no repo → no chip');
assert.ok(stageTrackerRepoChipHtml('a/<b>', ctx).includes('&lt;b&gt;'), 'escaped');

// rows: issue row, PR row (issue slot), PR row without an issue, nested sub-rows
const issue = stageTrackerRowHtml(I('hivecommons/dibs', 456, 10), stages, ctx);
const head = issue.slice(0, issue.indexOf('aria-label="Pipeline progress"'));
assert.ok(/<svg class="ham-kind-ico"[\s\S]*?<\/svg><button type="button" class="ham-repo-chip[^>]*>dibs<\/button>\s*<a class="ham-ref"[^>]*>#456<\/a>/.test(head), 'chip sits between the kind icon and #N: ' + head);
assert.strictEqual((issue.match(/ham-repo-chip/g) || []).length, 1, 'one chip per plain row');
const pr = stageTrackerRowHtml(P('hivecommons/dibs', 7, 50, { linked_issues: [{ number: 3, title: 'Fix', state: 'open' }] }), stages, ctx);
assert.ok(/ham-repo-chip[^>]*>dibs<\/button>\s*<a class="ham-ref"[^>]*>#3<\/a>/.test(pr), 'PR row: chip before the issue slot #N: ' + pr.slice(0, 700));
assert.strictEqual((pr.match(/ham-repo-chip/g) || []).length, 1, 'the PR link in the PR column carries no second chip');
const lone = stageTrackerRowHtml(P('hivecommons/pluk', 8, 30), stages, ctx);
assert.ok(/<div class="ham-head-main ham-no-issue">\s*<svg class="ham-kind-ico"[\s\S]*?<\/svg><button type="button" class="ham-repo-chip[^>]*>pluk<\/button>/.test(lone), 'no-issue PR row still leads with the chip: ' + lone.slice(0, 600));
const multi = stageTrackerRowHtml(I('hivecommons/dibs', 9, 20, { linked_prs: [{ number: 11, stage: 50, title: 'A', author_kind: 'hive' }, { number: 12, stage: 30, title: 'B', author_kind: 'human' }] }), stages, ctx);
const subs = multi.split('<div class="ham-sub"').slice(1);
assert.strictEqual(subs.length, 2);
for (const s of subs) assert.ok(/<span class="ham-pr-line"><button type="button" class="ham-repo-chip[^>]*>dibs<\/button><a class="ham-pr-ref"/.test(s), 'each sub-row leads its PR link with the chip: ' + s.slice(0, 400));
assert.strictEqual((multi.match(/ham-repo-chip/g) || []).length, 3, 'issue line + two sub-rows');

// Without ctx.repoChips (single repo under the default setting) nothing changes.
const plain = stageTrackerRowHtml(I('hivecommons/dibs', 456, 10), stages, { nowMs: now });
assert.ok(!plain.includes('ham-repo-chip') && !plain.includes('ham-pr-line'), 'no chip, no wrapper when off');
const plainMulti = stageTrackerRowHtml(I('hivecommons/dibs', 9, 20, { linked_prs: [{ number: 11, stage: 50 }, { number: 12, stage: 30 }] }), stages, { nowMs: now });
assert.ok(!plainMulti.includes('ham-pr-line'), 'sub-row links are not wrapped when off');

// settings: default, persistence, unknown values
const store = {};
global.localStorage = { getItem: k => (k in store ? store[k] : null), setItem(k, v) { store[k] = String(v); } };
assert.strictEqual(stageTrackerRepoChipSetting(), 'multi', 'default = only when several repos');
assert.strictEqual(stageTrackerSetRepoChip('never'), 'never');
assert.strictEqual(store[STAGE_TRACKER_REPO_CHIP_KEY], 'never');
assert.strictEqual(stageTrackerRepoChipSetting(), 'never');
assert.strictEqual(stageTrackerSetRepoChip('bogus'), 'multi', 'unknown falls back to the default');
store[STAGE_TRACKER_REPO_CHIP_KEY] = 'garbage';
assert.strictEqual(stageTrackerRepoChipSetting(), 'multi');
const section = stageTrackerRepoSectionHtml('always', 'repo');
assert.ok(section.includes('id="sts-repos"') && /value="always" checked/.test(section) && !/value="multi" checked/.test(section), section);
assert.ok(/<option value="repo" selected>Repo<\/option>/.test(section) && /<option value="none">None<\/option>/.test(section), section);
`
	if out, err := runNodeStdin(node, script); err != nil {
		t.Fatalf("node repo chip fixture failed: %v\n%s", err, out)
	}
}

// TestStageTrackerGroupByRepo runs the grouping under node: groups come out
// primary repo first then by row count, counts and needs-attention are per
// group, the mini distribution follows the drawn column, the chosen sort
// holds within a group, a collapsed group is its header alone and the
// collapsed set round-trips through storage, paging repeats a header for a
// page that opens mid-group, and the header row is keyed group:owner/repo.
func TestStageTrackerGroupByRepo(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — group-by-repo was NOT executed by this run")
	}
	html := indexHTML(t)
	script := stageTrackerRepoGroupingHarness(t, html) + `
const rows = [
  P('hivecommons/dibs', 1, 50), P('hivecommons/dibs', 2, 80, { modifiers: ['stalled'] }), I('hivecommons/dibs', 3, 10),
  I('hivecommons/hive', 10, 20, { linked_prs: [{ number: 11, stage: 50 }] }), P('hivecommons/hive', 12, 85),
  I('hivecommons/pluk', 20, 10), I('hivecommons/pluk', 21, 15), I('hivecommons/pluk', 22, 20), P('hivecommons/pluk', 23, 30),
  I('hivecommons/docs', 30, 0),
];
const f = stageTrackerNormalizeFilter({ sort: 'furthest' });
const ordered = stageTrackerSort(rows, stages, f);
let groups = stageTrackerGroups(ordered, { primary: 'hivecommons/hive', collapsed: {}, stages });
assert.deepStrictEqual(groups.map(g => g.repo), ['hivecommons/hive', 'hivecommons/pluk', 'hivecommons/dibs', 'hivecommons/docs'], 'primary first, then row count desc, then name');
assert.deepStrictEqual(groups.map(g => g.count), [2, 4, 3, 1]);
assert.deepStrictEqual(groups.map(g => g.key), ['group:hivecommons/hive', 'group:hivecommons/pluk', 'group:hivecommons/dibs', 'group:hivecommons/docs']);
assert.deepStrictEqual(groups.map(g => g.attention), [1, 0, 1, 0], '085 and a stalled modifier need attention');
// distribution: columns Open, Actionable, Claimed, PR in progress, CI / Review, Merge gate, merged cap
assert.deepStrictEqual(groups[0].cols, [0, 0, 0, 0, 1, 1, 0], 'a covered issue rides its PR column (050 → CI / Review); 085 → Merge gate');
assert.deepStrictEqual(groups[1].cols, [0, 2, 1, 1, 0, 0, 0], 'pluk: 010 + 015 Actionable, 020 Claimed, 030 PR in progress');
assert.deepStrictEqual(groups[3].cols, [1, 0, 0, 0, 0, 0, 0], 'stage 0 is Open');
// the chosen sort holds within a group
assert.deepStrictEqual(groups[2].rows.map(r => r.number), [2, 1, 3], 'dibs furthest along first');
for (const g of groups) assert.deepStrictEqual(g.rows.map(r => r.number), ordered.filter(r => r.repo === g.repo).map(r => r.number), 'the list order holds within ' + g.repo);
assert.deepStrictEqual(stageTrackerGroups(stageTrackerSort(rows, stages, { sort: 'number', dir: 'asc' }), { primary: 'hivecommons/hive', stages })[2].rows.map(r => r.number), [1, 2, 3], 'number ascending within the group');
// no primary known: by count then name
assert.deepStrictEqual(stageTrackerGroups(ordered, { stages }).map(g => g.repo), ['hivecommons/pluk', 'hivecommons/dibs', 'hivecommons/hive', 'hivecommons/docs']);
assert.deepStrictEqual(stageTrackerGroups([], {}), []);

// header html
const ctx = { nowMs: now, repoChips: { primary: 'hivecommons/hive', active: '' } };
let hdr = stageTrackerGroupHeaderHtml(groups[2], stages, ctx);
assert.ok(/^<div class="ham-group" data-ham-repo="hivecommons\/dibs"/.test(hdr), hdr);
assert.ok(hdr.includes('<span class="ham-group-name">hivecommons/dibs</span>') && hdr.includes('<span class="ham-group-n">3 rows</span>') && hdr.includes('<span class="ham-group-attn">1 needs attention</span>'), hdr);
assert.ok(hdr.includes('data-action="hamGroupToggle" data-arg0="hivecommons/dibs"') && hdr.includes('aria-expanded="true"'), hdr);
assert.ok(/ham-repo-chip ham-repo-tint-\d"[^>]*>dibs<\/button>/.test(hdr), 'the header carries the chip: ' + hdr);
assert.ok(hdr.includes('class="ham-gd ham-gd-4" title="Merge gate: 1"') && hdr.includes('class="ham-gd ham-gd-0" title="Open: 0"'), 'distribution bars name their column: ' + hdr);
assert.ok(stageTrackerGroupHeaderHtml(groups[2], stages, { nowMs: now }).includes('ham-repo-chip'), 'the header chip shows even when row chips are off');
assert.ok(!stageTrackerGroupHeaderHtml(groups[3], stages, ctx).includes('ham-group-attn'), 'no attention count when nothing needs it');
assert.ok(stageTrackerGroupHeaderHtml(groups[3], stages, ctx).includes('>1 row</span>'));

// paging over groups: headers are free, a page that opens mid-group repeats its header
let page = stageTrackerGroupPage(groups, Object.assign({}, f, { size: 25, page: 1 }));
assert.deepStrictEqual(page.rows.map(e => e.item ? e.item.number : e.group.key), ['group:hivecommons/hive', 12, 10, 'group:hivecommons/pluk', 23, 22, 20, 21, 'group:hivecommons/dibs', 2, 1, 3, 'group:hivecommons/docs', 30]);
assert.deepStrictEqual([page.page, page.pages, page.from, page.to, page.total, page.shown], [1, 1, 1, 10, 10, 10]);
// 25 is the smallest page size offered; drive the splitting rule with a
// 4-row page through the normaliser hook: hive(2) + pluk(2 of 4) on page 1,
// pluk(2) + dibs(2) on page 2, dibs(1) + docs(1) on page 3.
const normalize = stageTrackerNormalizeFilter;
stageTrackerNormalizeFilter = (raw) => { const o = normalize(raw); if (raw && raw.__size) o.size = raw.__size; return o; };
let p1 = stageTrackerGroupPage(groups, { __size: 4, page: 1 });
assert.deepStrictEqual(p1.rows.map(e => e.item ? e.item.number : e.group.key), ['group:hivecommons/hive', 12, 10, 'group:hivecommons/pluk', 23, 22]);
assert.deepStrictEqual([p1.page, p1.pages, p1.from, p1.to, p1.total], [1, 3, 1, 4, 10]);
let p2 = stageTrackerGroupPage(groups, { __size: 4, page: 2 });
assert.deepStrictEqual(p2.rows.map(e => e.item ? e.item.number : e.group.key), ['group:hivecommons/pluk', 20, 21, 'group:hivecommons/dibs', 2, 1], 'page 2 opens mid-pluk and repeats its header');
assert.deepStrictEqual([p2.from, p2.to], [5, 8]);
let p3 = stageTrackerGroupPage(groups, { __size: 4, page: 3 });
assert.deepStrictEqual(p3.rows.map(e => e.item ? e.item.number : e.group.key), ['group:hivecommons/dibs', 3, 'group:hivecommons/docs', 30]);
assert.deepStrictEqual([p3.from, p3.to], [9, 10]);
assert.strictEqual(stageTrackerGroupPage(groups, { __size: 4, page: 9 }).page, 3, 'a page past the end clamps');
// a header exactly at a page boundary opens the next page, not the previous one
const boundary = stageTrackerGroups(stageTrackerSort(rows.filter(r => r.repo !== 'hivecommons/docs' && r.repo !== 'hivecommons/dibs'), stages, f), { primary: 'hivecommons/hive', stages });
assert.deepStrictEqual(stageTrackerGroupPage(boundary, { __size: 2, page: 1 }).rows.map(e => e.item ? e.item.number : e.group.key), ['group:hivecommons/hive', 12, 10]);
assert.deepStrictEqual(stageTrackerGroupPage(boundary, { __size: 2, page: 2 }).rows.map(e => e.item ? e.item.number : e.group.key), ['group:hivecommons/pluk', 23, 22]);
stageTrackerNormalizeFilter = normalize;

// collapsing: the header alone, total still counts its rows, from/to count what is shown
groups = stageTrackerGroups(ordered, { primary: 'hivecommons/hive', collapsed: { 'hivecommons/pluk': true }, stages });
assert.strictEqual(groups[1].collapsed, true);
hdr = stageTrackerGroupHeaderHtml(groups[1], stages, ctx);
assert.ok(/^<div class="ham-group ham-group-collapsed"/.test(hdr) && hdr.includes('aria-expanded="false"') && hdr.includes('aria-label="Expand hivecommons/pluk"') && hdr.includes('>4 rows</span>'), hdr);
page = stageTrackerGroupPage(groups, f);
assert.deepStrictEqual(page.rows.map(e => e.item ? e.item.number : e.group.key), ['group:hivecommons/hive', 12, 10, 'group:hivecommons/pluk', 'group:hivecommons/dibs', 2, 1, 3, 'group:hivecommons/docs', 30]);
assert.deepStrictEqual([page.total, page.shown, page.from, page.to], [10, 6, 1, 6], 'collapsed rows are not "filtered out"');
// every group collapsed: headers only
const allC = stageTrackerGroups(ordered, { collapsed: { 'hivecommons/hive': true, 'hivecommons/pluk': true, 'hivecommons/dibs': true, 'hivecommons/docs': true }, stages });
page = stageTrackerGroupPage(allC, f);
assert.deepStrictEqual(page.rows.map(e => e.group.key), ['group:hivecommons/pluk', 'group:hivecommons/dibs', 'group:hivecommons/hive', 'group:hivecommons/docs']);
assert.deepStrictEqual([page.shown, page.from, page.to, page.pages], [0, 0, 0, 1]);

// collapsed set persists per repo under hive.tracker.group.collapsed; group under hive.tracker.group
const store = {};
global.localStorage = { getItem: k => (k in store ? store[k] : null), setItem(k, v) { store[k] = String(v); } };
assert.deepStrictEqual(stageTrackerLoadCollapsed(), {});
let c = stageTrackerToggleCollapsed({}, 'hivecommons/pluk');
assert.deepStrictEqual(c, { 'hivecommons/pluk': true });
stageTrackerSaveCollapsed(c);
assert.strictEqual(store[STAGE_TRACKER_GROUP_COLLAPSED_KEY], '{"hivecommons/pluk":true}');
assert.deepStrictEqual(stageTrackerLoadCollapsed(), { 'hivecommons/pluk': true }, 'round-trips');
c = stageTrackerToggleCollapsed(c, 'hivecommons/pluk');
assert.deepStrictEqual(c, {}, 'toggling again expands');
store[STAGE_TRACKER_GROUP_COLLAPSED_KEY] = '[1,2]';
assert.deepStrictEqual(stageTrackerLoadCollapsed(), {}, 'a non-object is ignored');
store[STAGE_TRACKER_GROUP_COLLAPSED_KEY] = '{"a/b":true,"c/d":false,"e/f":1}';
assert.deepStrictEqual(stageTrackerLoadCollapsed(), { 'a/b': true, 'e/f': true }, 'only truthy entries collapse');
store[STAGE_TRACKER_GROUP_COLLAPSED_KEY] = 'not json';
assert.deepStrictEqual(stageTrackerLoadCollapsed(), {});
assert.strictEqual(stageTrackerGroupSetting(), 'none', 'default: no grouping');
assert.strictEqual(stageTrackerSetGroup('repo'), 'repo');
assert.strictEqual(store[STAGE_TRACKER_GROUP_KEY], 'repo');
assert.strictEqual(stageTrackerGroupSetting(), 'repo');
assert.strictEqual(stageTrackerSetGroup('nope'), 'none');
store[STAGE_TRACKER_GROUP_COLLAPSED_KEY] = '{"hivecommons/docs":true}';
store[STAGE_TRACKER_GROUP_KEY] = 'repo';
stageTrackerLoadGrouping();
assert.strictEqual(_stageTrackerGroup, 'repo');
assert.deepStrictEqual(_stageTrackerCollapsed, { 'hivecommons/docs': true });
assert.strictEqual(stageTrackerPrimaryRepo(), 'hivecommons/hive');
`
	if out, err := runNodeStdin(node, script); err != nil {
		t.Fatalf("node group-by-repo fixture failed: %v\n%s", err, out)
	}
}

// TestStageTrackerGroupHeaderReconcile drives stageTrackerReconcile with
// group header rows in the list: a header keyed group:owner/repo keeps its
// node across refreshes while unchanged, is swapped when its counts change,
// moves with its group, and when a group's last row departs the header
// collapses out beside it (both removed once the exit animation ends).
func TestStageTrackerGroupHeaderReconcile(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — group header reconcile was NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"const assert = require('assert');",
		jsConstLine(t, html, "STAGE_TRACKER_MOTION_MAX_ROWS"),
		jsConstLine(t, html, "STAGE_TRACKER_EXIT_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_ENTER_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_HEADER_KEY"),
		jsConstLine(t, html, "STAGE_TRACKER_GROUP_KEY_PREFIX"),
		"var _stageTrackerRowSrc = new WeakMap();",
		jsFunc(t, html, "stageTrackerAfter"),
		jsFunc(t, html, "stageTrackerRowSrcKey"),
		jsFunc(t, html, "stageTrackerRowHasMove"),
		jsFunc(t, html, "stageTrackerRowAnimating"),
		jsFunc(t, html, "stageTrackerRowNode"),
		jsFunc(t, html, "stageTrackerRowSrcOf"),
		jsFunc(t, html, "stageTrackerRowMotionOn"),
		jsFunc(t, html, "stageTrackerRowExit"),
		jsFunc(t, html, "stageTrackerRowEnter"),
		jsFunc(t, html, "stageTrackerReconcile"),
	}, "\n") + `
let now = 0; const timers = [];
global.setTimeout = (fn, ms) => { timers.push({ at: now + (ms || 0), fn }); return timers.length; };
function flush() { timers.sort((a, b) => a.at - b.at); while (timers.length) { const t = timers.shift(); now = t.at; t.fn(); } }
function classes() { const set = new Set(); return { set, contains: c => set.has(c), add(...cs) { cs.forEach(c => set.add(c)); }, remove(...cs) { cs.forEach(c => set.delete(c)); } }; }
function base() {
  return { parentNode: null, children: [], dataset: {}, classList: classes(), offsetHeight: 24,
    style: { props: {}, setProperty(k, v) { this.props[k] = v; } },
    addEventListener() {}, removeEventListener() {},
    remove() { if (this.parentNode) { const c = this.parentNode.children; c.splice(c.indexOf(this), 1); this.parentNode = null; } },
    insertBefore(n, ref) { n.remove(); const i = ref ? this.children.indexOf(ref) : this.children.length; this.children.splice(i, 0, n); n.parentNode = this; },
    replaceChild(n, old) { const i = this.children.indexOf(old); n.remove(); this.children[i] = n; n.parentNode = this; old.parentNode = null; },
    querySelector() { return null; } };
}
global.document = { hidden: false, createElement(tag) { assert.strictEqual(tag, 'template'); const t = { content: {} }; Object.defineProperty(t, 'innerHTML', { set(v) { const n = base(); n.html = v; t.content.firstElementChild = n; } }); return t; } };
const el = base();
const H = { key: STAGE_TRACKER_HEADER_KEY, html: '<div class="ham-cols">cols</div>' };
const G = (repo, count) => ({ key: STAGE_TRACKER_GROUP_KEY_PREFIX + repo, html: '<div class="ham-group" data-ham-repo="' + repo + '">' + repo + ' ' + count + ' rows</div>' });
const R = (repo, n, active) => ({ key: repo + '/pr#' + n, html: '<div class="ham-row"><ol class="ham-strip">active=' + active + '</ol></div>' });
const keys = () => el.children.map(c => (c.classList.contains('ham-leaving') ? '-' : '') + c.dataset.hamKey);
const opts = { effect: 'wash', hidden: false };

// First render: hive group (2 rows), dibs group (1 row).
let r = stageTrackerReconcile(el, [H, G('o/hive', 2), R('o/hive', 1, 4), R('o/hive', 2, 3), G('o/dibs', 1), R('o/dibs', 9, 2)], opts);
assert.deepStrictEqual(r, { kept: 0, replaced: 0, entered: 6, exited: 0, deferred: 0 });
assert.deepStrictEqual(keys(), ['__cols', 'group:o/hive', 'o/hive/pr#1', 'o/hive/pr#2', 'group:o/dibs', 'o/dibs/pr#9']);
const hiveHdr = el.children[1], dibsHdr = el.children[4];
assert.strictEqual(hiveHdr.dataset.hamKey, 'group:o/hive', 'header rows are keyed group:owner/repo');

// Same payload: every node kept, header nodes included.
r = stageTrackerReconcile(el, [H, G('o/hive', 2), R('o/hive', 1, 4), R('o/hive', 2, 3), G('o/dibs', 1), R('o/dibs', 9, 2)], opts);
assert.deepStrictEqual(r, { kept: 6, replaced: 0, entered: 0, exited: 0, deferred: 0 });
assert.strictEqual(el.children[1], hiveHdr); assert.strictEqual(el.children[4], dibsHdr);

// A row joins dibs: its header's count changes, so the header is swapped
// (same key) while hive's header is kept; the groups re-order by count.
r = stageTrackerReconcile(el, [H, G('o/dibs', 2), R('o/dibs', 9, 2), R('o/dibs', 10, 1), G('o/hive', 2), R('o/hive', 1, 4), R('o/hive', 2, 3)], opts);
assert.deepStrictEqual(r, { kept: 5, replaced: 1, entered: 1, exited: 0, deferred: 0 });
assert.deepStrictEqual(keys(), ['__cols', 'group:o/dibs', 'o/dibs/pr#9', 'o/dibs/pr#10', 'group:o/hive', 'o/hive/pr#1', 'o/hive/pr#2']);
assert.notStrictEqual(el.children[1], dibsHdr, 'changed header is a fresh node');
assert.strictEqual(el.children[4], hiveHdr, 'unchanged header keeps its node across the move');
assert.ok(el.children[3].classList.contains('ham-entering'), 'the new row fades in');
flush();

// dibs empties: its last row and its header leave together, animated, and
// are removed once the exit animation ends; hive is untouched.
r = stageTrackerReconcile(el, [H, G('o/hive', 2), R('o/hive', 1, 4), R('o/hive', 2, 3)], opts);
assert.deepStrictEqual(r, { kept: 4, replaced: 0, entered: 0, exited: 3, deferred: 0 });
assert.deepStrictEqual(keys(), ['__cols', '-group:o/dibs', '-o/dibs/pr#9', '-o/dibs/pr#10', 'group:o/hive', 'o/hive/pr#1', 'o/hive/pr#2'], 'departing header + rows hold their place while collapsing');
assert.strictEqual(el.children[1].style.props['--ham-row-h'], '24px', 'the header exit is measured like a row');
assert.strictEqual(timers.length, 3);
flush();
assert.deepStrictEqual(keys(), ['__cols', 'group:o/hive', 'o/hive/pr#1', 'o/hive/pr#2'], 'header and rows gone after the exit animation');
assert.ok(now >= STAGE_TRACKER_EXIT_MS);

// With motion off (hidden tab) the header is simply removed.
r = stageTrackerReconcile(el, [H], { effect: 'wash', hidden: true });
assert.deepStrictEqual(keys(), ['__cols']);
assert.strictEqual(r.exited, 3);
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node group header reconcile fixture failed: %v\n%s", err, out)
	}
}
