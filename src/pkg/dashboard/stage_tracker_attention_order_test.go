package dashboard

import (
	"os/exec"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// stageTrackerTierJS is the dashboard's attention-tier helper and the
// constants it reads, for node fixtures that sort rows or sub-rows. The
// caller supplies stageTrackerNeedsAttention and what it needs.
func stageTrackerTierJS(t *testing.T, html string) string {
	t.Helper()
	return strings.Join([]string{
		jsConstLine(t, html, "STAGE_TRACKER_TIER_ATTENTION"),
		jsConstLine(t, html, "STAGE_TRACKER_TIER_NORMAL"),
		jsConstLine(t, html, "STAGE_TRACKER_TIER_SUNK"),
		jsConstLine(t, html, "STAGE_TRACKER_SUNK_MODS"),
		jsFunc(t, html, "stageTrackerAttentionTier"),
	}, "\n")
}

// stageTrackerAttentionTier puts the rows a person must act on first and
// the rows nobody should act on last, and the payload pre-sort applies
// it within a stage ahead of the wait, repo and number keys.
func TestStageTrackerAttentionTier(t *testing.T) {
	cases := []struct {
		name string
		item StageTrackerItem
		want int
	}{
		{"plain row on a hive stage", StageTrackerItem{Stage: stagelabels.ReviewPending.Number(), Modifiers: []string{}}, stageTrackerTierNormal},
		{"needs-decision stage", StageTrackerItem{Stage: stagelabels.NeedsDecision.Number()}, stageTrackerTierAttention},
		{"ready-for-human-merge stage", StageTrackerItem{Stage: stagelabels.ReadyForHumanMerge.Number()}, stageTrackerTierAttention},
		{"needs-author stage", StageTrackerItem{Stage: stagelabels.NeedsAuthor.Number(), AuthorKind: authorKindHuman}, stageTrackerTierAttention},
		{"returned-to-contributor stage", StageTrackerItem{Stage: stagelabels.ReturnedToContributor.Number()}, stageTrackerTierAttention},
		{"stalled pill", StageTrackerItem{Stage: stagelabels.ReviewPending.Number(), Modifiers: []string{modStalled}}, stageTrackerTierAttention},
		{"unauthorized pill", StageTrackerItem{Stage: stagelabels.AwaitingMergeAdmission.Number(), Modifiers: []string{modUnauthorized}}, stageTrackerTierAttention},
		{"needs-rebase pill", StageTrackerItem{Stage: stagelabels.ReviewPending.Number(), Modifiers: []string{modNeedsRebase}, AuthorKind: authorKindHuman}, stageTrackerTierAttention},
		{"regressed on a hive stage is handled, not attention", StageTrackerItem{Stage: stagelabels.ReviewPending.Number(), Modifiers: []string{modRegressed}}, stageTrackerTierNormal},
		{"regressed onto a human stage", StageTrackerItem{Stage: stagelabels.NeedsDecision.Number(), Modifiers: []string{modRegressed}}, stageTrackerTierAttention},
		{"hold sinks", StageTrackerItem{Stage: stagelabels.ReviewPending.Number(), Modifiers: []string{modHold}}, stageTrackerTierSunk},
		{"hold sinks even on a human stage", StageTrackerItem{Stage: stagelabels.NeedsDecision.Number(), Modifiers: []string{modHold, modNeedsDecision}}, stageTrackerTierSunk},
		{"draft sinks", StageTrackerItem{Stage: 0, Modifiers: []string{modDraft}}, stageTrackerTierSunk},
		{"hold beats stalled", StageTrackerItem{Stage: stagelabels.Fixing.Number(), Modifiers: []string{modStalled, modHold}}, stageTrackerTierSunk},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stageTrackerAttentionTier(tc.item); got != tc.want {
				t.Fatalf("tier = %d, want %d", got, tc.want)
			}
		})
	}

	// The pre-sort: stage desc, then tier, then longest wait, repo, number.
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	since := func(d time.Duration) *time.Time { ts := base.Add(-d); return &ts }
	items := []StageTrackerItem{
		{Repo: "a/b", Number: 1, Stage: 50, StageSince: since(9 * 24 * time.Hour), Modifiers: []string{}},                  // oldest plain
		{Repo: "a/b", Number: 2, Stage: 50, StageSince: since(time.Hour), Modifiers: []string{modStalled}},                 // attention, young
		{Repo: "a/b", Number: 3, Stage: 50, StageSince: since(30 * 24 * time.Hour), Modifiers: []string{modHold}},          // sunk, oldest of all
		{Repo: "a/b", Number: 4, Stage: 50, StageSince: since(2 * time.Hour), Modifiers: []string{}},                       // plain, young
		{Repo: "a/b", Number: 5, Stage: 50, StageSince: since(3 * time.Hour), Modifiers: []string{modDraft}},               // sunk
		{Repo: "a/b", Number: 6, Stage: 50, StageSince: since(4 * time.Hour), Modifiers: []string{modUnauthorized}},        // attention, older than #2
		{Repo: "a/b", Number: 7, Stage: 80, StageSince: since(time.Minute), Modifiers: []string{}},                         // further stage wins over tier
		{Repo: "a/b", Number: 8, Stage: 50, StageSince: since(10 * 24 * time.Hour), Modifiers: []string{modRegressed}},     // handled regression = plain, oldest plain
		{Repo: "a/b", Number: 9, Stage: 50, StageSince: since(10 * 24 * time.Hour), Modifiers: []string{modNeedsDecision}}, // deferred-shaped pill shown → attention
	}
	sort.SliceStable(items, stageTrackerLess(items))
	got := make([]int, 0, len(items))
	for _, it := range items {
		got = append(got, it.Number)
	}
	want := []int{7, 9, 6, 2, 8, 1, 4, 3, 5}
	if !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// TestStageTrackerAttentionOrderJS drives the dashboard's sort and sub-row
// order under node: attention-first within a column for the furthest and
// waiting sorts, hold / draft / closed-issue rows last, the direction
// toggle flipping only the age keys, newest and number sorts untouched.
func TestStageTrackerAttentionOrderJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — tracker attention order was NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"const window = {};",
		jsConstLine(t, html, "STAGE_TRACKER_PAGE_SIZES"),
		jsConstLine(t, html, "STAGE_TRACKER_DEFAULT_PAGE_SIZE"),
		jsConstLine(t, html, "STAGE_TRACKER_AGE_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_ATTENTION_STAGES"),
		jsConstLine(t, html, "STAGE_TRACKER_SORTS"),
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL"),
		jsConstLine(t, html, "STAGE_TRACKER_PATH"),
		jsConstLine(t, html, "STAGE_TRACKER_PR_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_DECISION"),
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_NEEDS_DECISION"),
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_DECISION_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_TRIAGE_NEEDED"),
		jsConstLine(t, html, "STAGE_TRACKER_POSSIBLY_EXISTS"),
		jsConstLine(t, html, "STAGE_TRACKER_PRE_MERGE_SOAK"),
		jsConstLine(t, html, "STAGE_TRACKER_HUMAN_MODS"),
		stageTrackerTierJS(t, html),
		jsFunc(t, html, "stageTrackerDefaultFilter"),
		"var _stageTrackerFilter = stageTrackerDefaultFilter();",
		jsFunc(t, html, "stageTrackerAnchor"),
		jsFunc(t, html, "stageTrackerLinkedDrawnHere"),
		jsFunc(t, html, "stageTrackerLinkedSuperseder"),
		jsFunc(t, html, "stageTrackerLinkedSupersededGone"),
		jsFunc(t, html, "stageTrackerLinkedSupersededTip"),
		jsFunc(t, html, "stageTrackerLinkedSorted"),
		jsFunc(t, html, "stageTrackerSortKey"),
		jsFunc(t, html, "stageTrackerRegressionHandled"),
		jsFunc(t, html, "stageTrackerNeedsAttention"),
		jsFunc(t, html, "stageTrackerWaitSince"),
		jsConstBlock(t, html, "STAGE_TRACKER_ICONS"),
		stageTrackerActedJS(t, html),
		jsFunc(t, html, "stageTrackerNormalizeFilter"),
		jsFunc(t, html, "stageTrackerSort"),
	}, "\n") + `
const assert = require('assert');
const now = Date.parse('2026-01-01T12:00:00Z');
const H = 3600000, D = 24 * H;
const ago = (ms) => new Date(now - ms).toISOString();
const stages = [{ number: 50, label: 'Review pending' }, { number: 80, label: 'Merge gate' }, { number: 85, label: 'Needs decision' }, { number: 110, label: 'Merged' }];
const row = (number, over) => Object.assign({ kind: 'pr', repo: 'a/b', number, stage: 50, modifiers: [], created_at: ago(number * D) }, over);
const items = [
  row(1, { stage_since: ago(9 * D) }),                                           // plain, oldest plain
  row(2, { stage_since: ago(H), modifiers: ['stalled'] }),                        // attention, young
  row(3, { stage_since: ago(30 * D), modifiers: ['hold'] }),                      // sunk, oldest of all
  row(4, { stage_since: ago(2 * H) }),                                            // plain, young
  row(5, { stage_since: ago(3 * H), modifiers: ['draft'] }),                      // sunk
  row(6, { stage_since: ago(4 * H), modifiers: ['unauthorized'] }),               // attention, older than #2
  row(7, { stage: 80, stage_since: ago(60000) }),                                 // further column wins over tier
  row(8, { stage_since: ago(10 * D), modifiers: ['regressed'] }),                 // handled regression = plain
  row(9, { stage_since: ago(5 * H), closed_issue: { number: 42, state: 'closed' } }), // closed-issue-anchored PR sinks
  row(10, { stage_since: ago(20 * D), deferred_modifiers: ['needs-human'] }),      // deferred pill never promotes
];
const nums = (rows) => rows.map(it => it.number);
const F = (over) => Object.assign(stageTrackerDefaultFilter(), over);

assert.strictEqual(stageTrackerAttentionTier(items[1]), STAGE_TRACKER_TIER_ATTENTION);
assert.strictEqual(stageTrackerAttentionTier(items[0]), STAGE_TRACKER_TIER_NORMAL);
assert.strictEqual(stageTrackerAttentionTier(items[2]), STAGE_TRACKER_TIER_SUNK, 'hold sinks');
assert.strictEqual(stageTrackerAttentionTier(items[4]), STAGE_TRACKER_TIER_SUNK, 'draft sinks');
assert.strictEqual(stageTrackerAttentionTier(items[8]), STAGE_TRACKER_TIER_SUNK, 'closed-issue anchor sinks');
assert.strictEqual(stageTrackerAttentionTier(items[7]), STAGE_TRACKER_TIER_NORMAL, 'a handled regression is not attention');
assert.strictEqual(stageTrackerAttentionTier(items[9]), STAGE_TRACKER_TIER_NORMAL, 'deferred pills do not promote');
assert.strictEqual(stageTrackerAttentionTier({ stage: 85, modifiers: [] }), STAGE_TRACKER_TIER_ATTENTION, 'a human-gated stage is attention without a pill');
assert.strictEqual(stageTrackerAttentionTier({ stage: 85, modifiers: ['hold'] }), STAGE_TRACKER_TIER_SUNK, 'hold sinks even on a human-gated stage');

// Furthest: column first, then tier, then oldest on step.
assert.deepStrictEqual(nums(stageTrackerSort(items, stages, F({ sort: 'furthest' }))), [7, 6, 2, 10, 8, 1, 4, 3, 9, 5]);
// Ascending flips the column and age keys but keeps attention on top and sunk rows at the bottom of each column.
assert.deepStrictEqual(nums(stageTrackerSort(items, stages, F({ sort: 'furthest', dir: 'asc' }))), [2, 6, 4, 1, 8, 10, 5, 9, 3, 7]);
// Waiting: tier first, then longest wait.
assert.deepStrictEqual(nums(stageTrackerSort(items, stages, F({ sort: 'waiting' }))), [6, 2, 10, 8, 1, 4, 7, 3, 9, 5]);
assert.deepStrictEqual(nums(stageTrackerSort(items, stages, F({ sort: 'waiting', dir: 'asc' }))), [2, 6, 7, 4, 1, 8, 10, 5, 9, 3]);
// Newest and number ignore the tier.
assert.deepStrictEqual(nums(stageTrackerSort(items, stages, F({ sort: 'newest' }))), [1, 2, 3, 4, 5, 6, 7, 8, 9, 10]);
assert.deepStrictEqual(nums(stageTrackerSort(items, stages, F({ sort: 'number' }))), [10, 9, 8, 7, 6, 5, 4, 3, 2, 1]);
assert.deepStrictEqual(nums(stageTrackerSort(items, stages, F({ sort: 'number', dir: 'asc' }))), [1, 2, 3, 4, 5, 6, 7, 8, 9, 10]);
assert.deepStrictEqual(nums(items), [1, 2, 3, 4, 5, 6, 7, 8, 9, 10], 'sorting does not mutate the payload');

// Sub-rows under an issue tier the same way within a stage.
const issue = { kind: 'issue', repo: 'a/b', number: 99, stage: 50, linked_prs: [
  { number: 201, stage: 50, stage_since: ago(5 * D), modifiers: ['hold'] },
  { number: 202, stage: 50, stage_since: ago(2 * H), modifiers: ['stalled'] },
  { number: 203, stage: 50, stage_since: ago(4 * D), modifiers: [] },
  { number: 204, stage: 80, stage_since: ago(H), modifiers: ['draft'] },
] };
assert.deepStrictEqual(nums(stageTrackerLinkedSorted(issue, stages)), [204, 202, 203, 201], 'furthest stage first; within a stage attention, plain, then hold');
console.log('ok');
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("node tracker attention order fixture failed: %v\n%s", err, out)
	}
}
