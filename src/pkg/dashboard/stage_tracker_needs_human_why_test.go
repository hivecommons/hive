package dashboard

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

const filedForHelpBody = "Set the GitHub topics and homepage so the repo is discoverable.\n\n<!-- hive:filed-for-help outreach -->"

func TestStageTrackerNeedsHumanWhy(t *testing.T) {
	cases := []struct {
		name       string
		item       StageTrackerItem
		wantSource string
		wantReason string
	}{
		{
			name:       "filed-for-help issue nobody claimed",
			item:       StageTrackerItem{Kind: "issue", Number: 9, FiledForHelp: github.ParseFiledForHelpMarker(filedForHelpBody)},
			wantSource: needsHumanSourceFiledForHelp,
			wantReason: "filed for help by outreach: needs a person outside the hive",
		},
		{
			name:       "decision requested and unanswered, with the question",
			item:       StageTrackerItem{Kind: "issue", Number: 1, Stage: stagelabels.IssueNeedsDecision.Number(), DecisionNeeded: &StageTrackerDecision{Question: "Ship it behind a flag?"}},
			wantSource: needsHumanSourceDecision,
			wantReason: "Ship it behind a flag?",
		},
		{
			name:       "decision requested and unanswered, no question text",
			item:       StageTrackerItem{Kind: "pr", Number: 2, Stage: stagelabels.NeedsDecision.Number()},
			wantSource: needsHumanSourceDecision,
			wantReason: needsHumanReasonDecisionUnanswered,
		},
		{
			name:       "decision gate label",
			item:       StageTrackerItem{Kind: "issue", Number: 3, Labels: []string{github.DefaultDecisionNeededLabel}, Modifiers: []string{modHold}},
			wantSource: needsHumanSourceDecision,
			wantReason: needsHumanReasonDecisionUnanswered,
		},
		{
			name:       "level-gate held PR",
			item:       StageTrackerItem{Kind: "pr", Number: 4, Labels: []string{github.DefaultReadyForReviewLabel}, Modifiers: []string{modHold}},
			wantSource: needsHumanSourceLevelGate,
			wantReason: needsHumanReasonLevelGate,
		},
		{
			name:       "hold the hive placed",
			item:       StageTrackerItem{Kind: "issue", Number: 5, Modifiers: []string{modHold}, Hold: &StageTrackerHold{By: "hive", Pill: "Held by hive: reporter trust"}},
			wantSource: needsHumanSourceHiveHold,
			wantReason: "Held by hive: reporter trust",
		},
		{
			name:       "needs approval: App lacks permission",
			item:       StageTrackerItem{Kind: "pr", Number: 6, Stage: stagelabels.AwaitingMergeAdmission.Number(), Modifiers: []string{modUnauthorized}},
			wantSource: needsHumanSourceApproval,
			wantReason: needsHumanReasonApprovalPerm,
		},
		{
			name:       "needs approval: human merge",
			item:       StageTrackerItem{Kind: "pr", Number: 7, Stage: stagelabels.ReadyForHumanMerge.Number()},
			wantSource: needsHumanSourceApproval,
			wantReason: needsHumanReasonApprovalMerge,
		},
		{
			name:       "needs-human marker",
			item:       StageTrackerItem{Kind: "issue", Number: 8, Modifiers: []string{modNeedsHuman}},
			wantSource: needsHumanSourceMarker,
			wantReason: needsHumanReasonMarker,
		},
		{name: "blocked on an owner action names its reason", item: StageTrackerItem{Kind: "issue", Number: 16, Stage: 10, Modifiers: []string{modBlocked}, BlockedOwnerReason: "needs an owner action: grant the hub app contents:write"}, wantSource: needsHumanSourceBlockedOnOwner, wantReason: "needs an owner action: grant the hub app contents:write"},
		{name: "a held owner-action block is a pause, not a wait", item: StageTrackerItem{Kind: "issue", Number: 17, Modifiers: []string{modBlocked, modHold}, BlockedOwnerReason: "needs an owner action", Hold: &StageTrackerHold{By: "alice"}}},
		{name: "plain open issue stays Open", item: StageTrackerItem{Kind: "issue", Number: 10}},
		{name: "plain actionable issue", item: StageTrackerItem{Kind: "issue", Number: 11, Stage: 10}},
		{name: "claimed filed-for-help issue is being worked", item: StageTrackerItem{Kind: "issue", Number: 12, Stage: trackerNumberClaimed, FiledForHelp: "outreach"}},
		{name: "person's own hold is a pause, not a wait", item: StageTrackerItem{Kind: "issue", Number: 13, FiledForHelp: "outreach", Modifiers: []string{modHold}, Hold: &StageTrackerHold{By: "alice"}}},
		{name: "advisory asks for nothing", item: StageTrackerItem{Kind: "issue", Number: 14, FiledForHelp: "outreach", Advisory: &StageTrackerAdvisory{Kind: StageTrackerAdvisoryKind}}},
		{name: "decided row is the hive's to act on", item: StageTrackerItem{Kind: "pr", Number: 15, Stage: stagelabels.NeedsDecision.Number(), Decided: &StageTrackerDecided{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stageTrackerNeedsHumanWhyFor(tc.item)
			if tc.wantSource == "" {
				if got != nil {
					t.Fatalf("want no needs-human state, got %+v", got)
				}
				return
			}
			if got == nil || got.Source != tc.wantSource || got.Reason != tc.wantReason {
				t.Fatalf("got %+v, want source %q reason %q", got, tc.wantSource, tc.wantReason)
			}
		})
	}
}

// The count, the per-item flag, the one-line requirement and the
// waiting-on-you list all follow the same derivation.
func TestStageTrackerNeedsHumanWhyFeedsFlagCountAndWaitingList(t *testing.T) {
	items := []StageTrackerItem{
		{Repo: "hivecommons/quorum-companion", Kind: "issue", Number: 9, FiledForHelp: "outreach"},
		{Repo: "hivecommons/quorum-companion", Kind: "issue", Number: 10},
	}
	needs, _ := stageTrackerHumanCounts(items)
	if needs != 1 || !items[0].NeedsHuman || items[0].NeedsHumanWhy == nil || items[1].NeedsHuman || items[1].NeedsHumanWhy != nil {
		t.Fatalf("flags wrong: needs=%d %+v / %+v", needs, items[0], items[1])
	}
	w := stageTrackerWaitingOnHuman(items)
	if w.Count != 1 || w.Items[0].Number != 9 || !items[0].WaitingOnYou {
		t.Fatalf("waiting list = %+v", w)
	}
	a := stageTrackerHumanAttention(items[0])
	if a == nil || a.Reason != items[0].NeedsHumanWhy.Reason {
		t.Fatalf("human attention = %+v", a)
	}
}

// The SPA side: an Open issue with needs_human_why reads "Needs human" in
// the Open column with the later stages not applicable, and joins the filter.
func TestStageTrackerNeedsHumanOpenChevronAndFilter(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		".ham-seg.active.ham-open-needs-human {",
		".ham-seg.ham-na {",
		"function stageTrackerOpenNeedsHuman(item)",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("index.html missing %q", want)
		}
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH: the Needs human chevron was NOT executed by this run")
	}
	runStageTrackerRowActionsScript(t, "const assert = require('assert');\n"+strings.Join([]string{
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_HUMAN_MODS"),
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_HUMAN_STAGES"),
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_HUMAN_AUTHOR_STAGES"),
		jsFunc(t, html, "stageTrackerItemOnHold"),
		jsFunc(t, html, "stageTrackerItemNeedsHuman"),
		jsFunc(t, html, "stageTrackerNeedsHumanWhyOf"),
		jsFunc(t, html, "stageTrackerOpenNeedsHuman"),
	}, "\n")+`
const filed = { repo: 'hivecommons/quorum-companion', kind: 'issue', number: 9, stage: 0, modifiers: [], needs_human: true,
  needs_human_why: { source: 'filed-for-help', reason: 'filed for help by outreach: needs a person outside the hive' } };
const plain = { repo: 'hivecommons/quorum-companion', kind: 'issue', number: 10, stage: 0, modifiers: [] };
assert.strictEqual(stageTrackerOpenNeedsHuman(filed), true);
assert.strictEqual(stageTrackerItemNeedsHuman(filed), true);
assert.strictEqual(stageTrackerOpenNeedsHuman(plain), false);
assert.strictEqual(stageTrackerItemNeedsHuman(plain), false);
// A hive gate hold still counts: the hive placed it, a person lifts it.
const gated = Object.assign({}, filed, { modifiers: ['hold'] });
assert.strictEqual(stageTrackerItemNeedsHuman(gated), true);
// An advisory never does.
assert.strictEqual(stageTrackerOpenNeedsHuman(Object.assign({}, filed, { advisory: { kind: 'advisory' } })), false);
// A PR keeps its real stage chevron: Needs human is the Open column only.
assert.strictEqual(stageTrackerOpenNeedsHuman(Object.assign({}, filed, { kind: 'pr' })), false);
`)
}
