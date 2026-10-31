package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestStageTrackerHumanAttentionIsARequirementNotAnAffordance(t *testing.T) {
	tests := []struct {
		name string
		item StageTrackerItem
		want bool
	}{
		{"optional decision", StageTrackerItem{Kind: "pr", Stage: trackerNumberReviewPending, DecisionNeeded: &StageTrackerDecision{Question: "Optional adoption?"}}, false},
		{"person claimant", StageTrackerItem{Kind: "pr", Stage: trackerNumberCoveredByPR, StageActor: &StageTrackerActor{Kind: StageActorHuman, Login: "alice"}}, false},
		{"required review decision", StageTrackerItem{Kind: "pr", Stage: trackerNumberNeedsDecision, DecisionNeeded: &StageTrackerDecision{Question: "Confirm the compatibility break", AskedBy: "a reviewer"}}, true},
		{"required issue decision", StageTrackerItem{Kind: "issue", Stage: trackerNumberIssueDecision}, true},
		{"unknown reason", StageTrackerItem{Kind: "pr", Stage: trackerNumberNeedsDecision}, true},
		{"unknown reason with generic actor", StageTrackerItem{Kind: "pr", Stage: trackerNumberNeedsDecision,
			StageActor: &StageTrackerActor{Kind: StageActorHuman, Why: "a human decision is owed"}}, true},
		{"held", StageTrackerItem{Kind: "pr", Stage: trackerNumberNeedsDecision, Modifiers: []string{modHold}}, false},
		{"draft", StageTrackerItem{Kind: "pr", Stage: trackerNumberNeedsDecision, Modifiers: []string{modDraft}}, false},
		{"merged stale label", StageTrackerItem{Kind: "pr", Stage: stagelabels.MergeQueue.Number(), Modifiers: []string{modNeedsHuman}}, false},
		{"mirrored issue", StageTrackerItem{Kind: "issue", Stage: trackerNumberNeedsDecision, Mirrored: true}, false},
		{"answered", StageTrackerItem{Kind: "pr", Stage: trackerNumberReviewPending, Decided: &StageTrackerDecided{Effect: "pending"}}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.item.Number, tt.item.Head = 15985, "current-head"
			got := stageTrackerHumanAttention(tt.item)
			if (got != nil) != tt.want {
				t.Fatalf("attention = %+v, want present %v", got, tt.want)
			}
			if got == nil {
				return
			}
			if got.Number != tt.item.Number || got.Head != tt.item.Head || got.Kind != tt.item.Kind {
				t.Fatalf("wrong identity: %+v", got)
			}
			if strings.HasPrefix(tt.name, "unknown reason") && got.Reason != "Human decision required; reason unavailable" {
				t.Fatalf("unknown reason fabricated: %+v", got)
			}
			if tt.item.DecisionNeeded != nil && (got.Reason != tt.item.DecisionNeeded.Question || got.AskedBy != "a reviewer") {
				t.Fatalf("decision reason lost: %+v", got)
			}
		})
	}
}

func TestStageTrackerOperatorApprovalDoesNotHideCurrentReviewChanges(t *testing.T) {
	now := time.Date(2026, 10, 31, 11, 0, 0, 0, time.UTC)
	for _, stage := range []stagelabels.Stage{stagelabels.Fixing, stagelabels.NeedsAuthor, stagelabels.ReturnedToContributor, stagelabels.NeedsDecision, stagelabels.ReadyForHumanMerge} {
		for _, action := range []string{operatorActionLetHiveDecide, operatorActionApprove, operatorActionQueueMerge, operatorActionMergeNow} {
			t.Run(stage.String()+"/"+action, func(t *testing.T) {
				it := StageTrackerItem{Kind: "pr", Repo: "o/r", Number: 15977, Head: "current", Stage: stage.Number(),
					Author: "thatguywob", AuthorKind: authorKindClanker, Reason: "contributor-lane:changes-requested",
					Caption:        "Changes requested by the current-head review",
					DecisionNeeded: &StageTrackerDecision{Question: "Address the review findings"},
					LastOperatorAction: &StageTrackerOperatorAction{Action: action, Verdict: string(delegatedecision.VerdictApprove),
						HeadSHA: "current", StageAt: stage.Number(), At: now.Add(-time.Minute), Outcome: operatorActionOutcomeApplied}}
				d := stageTrackerDecided(it, automerge.SweepState{}, now, time.Hour)
				if d == nil || d.Effect != decidedEffectNone || d.Stage != 0 || d.CurrentBlocker == "" || d.Head != it.Head {
					t.Fatalf("approval wrongly outranked current review: %+v", d)
				}
				if strings.Contains(d.Text, "merge queue takes it") || !strings.Contains(d.Text, "waiting on @thatguywob") {
					t.Fatalf("wrong precedence/explanation: %+v", d)
				}
				beforeStage, beforeReason := it.Stage, it.Reason
				stageTrackerProjectDecided(&it, d, automerge.SweepState{}, now)
				if it.Stage != beforeStage || it.Reason != beforeReason || it.DecisionNeeded == nil {
					t.Fatalf("actual blocker/decision was cleared: %+v", it)
				}
				it.Head, it.Stage, it.Reason = "new-head", trackerNumberReadyForHuman, ""
				d = stageTrackerDecided(it, automerge.SweepState{}, now, time.Hour)
				if d.Effect != decidedEffectNone || d.Stage != 0 || d.CurrentBlocker != "" || !strings.Contains(d.Text, "previous head") {
					t.Fatalf("new head borrowed old approval/blocker proof: %+v", d)
				}
				it.LastOperatorAction.HeadSHA = "new-head"
				d = stageTrackerDecided(it, automerge.SweepState{}, now, time.Hour)
				if d.CurrentBlocker != "" {
					t.Fatalf("cleared review blocker lingered: %+v", d)
				}
			})
		}
	}
}

func TestStageTrackerHumanAttentionTracksOnlyTheCurrentCoveringPR(t *testing.T) {
	pr := StageTrackerItem{Repo: "o/r", Kind: "pr", Number: 15985, Head: "current", Stage: trackerNumberNeedsDecision,
		DecisionNeeded: &StageTrackerDecision{Question: "Choose the compatibility policy", AskedBy: "a reviewer"},
		StageActor:     &StageTrackerActor{Kind: StageActorHuman, Login: "owner", HumanReason: "choose the compatibility policy"}}
	issue := StageTrackerItem{Repo: "o/r", Kind: "issue", Number: 15937, Stage: trackerNumberCoveredByPR, LinkedPR: pr.Number,
		StageActor: &StageTrackerActor{Kind: StageActorNone}, LinkedPRs: []StageTrackerLinkedPR{
			{Number: pr.Number, Head: pr.Head, DrawnUnder: 15862},
			{Number: 15998, Head: "other", Stage: trackerNumberReviewPending},
		}}
	rfc := StageTrackerItem{Repo: "o/r", Kind: "issue", Number: 15862, Stage: trackerNumberIssueDecision,
		DecisionNeeded: &StageTrackerDecision{Question: "Deploy the future fleet?"}}
	items := []StageTrackerItem{issue, pr, rfc}
	stageTrackerResolveActors(items)
	stageTrackerResolveHumanAttention(items)
	if items[0].HumanAttention != nil {
		t.Fatal("covered issue was falsely made human-gated")
	}
	a := items[0].LinkedPRs[0].HumanAttention
	if a == nil || a.Number != pr.Number || a.Reason != pr.DecisionNeeded.Question || a.Owner != "owner" {
		t.Fatalf("correct PR requirement not propagated: %+v", a)
	}
	if items[0].LinkedPRs[1].HumanAttention != nil {
		t.Fatal("unrelated child inherited another PR/RFC's decision")
	}
	if !strings.Contains(items[0].LinkedPRs[0].StageActor.HumanReason, "compatibility") {
		t.Fatalf("specific external PR reason replaced by generic ask: %+v", items[0].LinkedPRs[0].StageActor)
	}
	if items[0].StageActor.Kind != StageActorNone {
		t.Fatalf("external PR wrongly replaced the locally drawn sibling's actor: %+v", items[0].StageActor)
	}
	// A stale link must lose the old proof instead of acquiring the new head.
	items[1].Head = "new-head"
	stageTrackerResolveHumanAttention(items)
	if items[0].LinkedPRs[0].HumanAttention != nil {
		t.Fatal("old linked head borrowed the new head's requirement")
	}
	items[0].LinkedPRs[0].Head = "new-head"
	items[1].Stage, items[1].DecisionNeeded = trackerNumberReviewPending, nil
	stageTrackerResolveHumanAttention(items)
	if items[1].HumanAttention != nil || items[0].LinkedPRs[0].HumanAttention != nil {
		t.Fatal("answered/released requirement lingered on issue or PR")
	}
	items[1].Stage = trackerNumberNeedsDecision
	items[0].LinkedPRs[0].State = "closed"
	stageTrackerResolveHumanAttention(items)
	if items[0].LinkedPRs[0].HumanAttention != nil {
		t.Fatal("closed linked PR retained requirement")
	}
}
