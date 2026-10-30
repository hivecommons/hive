package dashboard

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

// TestStageTrackerHumanReasonNeverShipsBareMarker (hivecommons/hive#15773,
// "when i see 'pr in progress' on the chevron - i have no idea when it
// needs a human at that stage"): every item whose actor is a person
// carries a short human_reason, across every stage, kind and parking
// flag the actor logic knows.
func TestStageTrackerHumanReasonNeverShipsBareMarker(t *testing.T) {
	now := time.Date(2026, 10, 28, 18, 0, 0, 0, time.UTC)
	stages := []int{0, trackerNumberActionable, trackerNumberTriageNeeded, trackerNumberPossiblyExists, trackerNumberClaimed, trackerNumberPRInProgress, trackerNumberCoveredByPR, trackerNumberIssueDecision, trackerNumberReviewPending, trackerNumberReviewed, trackerNumberFixing, trackerNumberNeedsAuthor, trackerNumberReturned, trackerNumberAdopted, trackerNumberAwaitingMerge, trackerNumberNeedsDecision, trackerNumberReadyForHuman, trackerNumberPreMergeSoak}
	variants := []func(*StageTrackerItem){
		func(*StageTrackerItem) {},
		func(it *StageTrackerItem) { it.Modifiers = []string{modNeedsHuman} },
		func(it *StageTrackerItem) { it.Modifiers = []string{modNeedsDecision} },
		func(it *StageTrackerItem) {
			it.Modifiers = []string{modUnauthorized}
			it.Reason = prstage.StageReasonUnauthorizedPrefix + prstage.UnauthorizedStepApproveWorkflowRuns
		},
		func(it *StageTrackerItem) { it.SentinelAlert = true; it.SentinelReason = "secret in diff" },
		func(it *StageTrackerItem) { it.Advisory = &StageTrackerAdvisory{Caption: "advisory"} },
		func(it *StageTrackerItem) {
			it.DecisionNeeded = &StageTrackerDecision{AskedBy: decisionAskedByReviewer, Question: "Is the schema change intended?"}
		},
		func(it *StageTrackerItem) {
			it.DecisionNeeded = &StageTrackerDecision{AskedBy: decisionAskedBySentinel}
		},
		func(it *StageTrackerItem) {
			it.DecisionNeeded = &StageTrackerDecision{AskedBy: decisionAskedByReviewer, RedundantOf: 41}
		},
		func(it *StageTrackerItem) { it.Superseded = &github.ReviewQueueSuperseded{CloserPR: 77} },
		func(it *StageTrackerItem) {
			it.Reason = prstage.ContributorLaneReasonWorkflowPath
			it.ManualMerge = true
		},
	}
	seen := 0
	for _, kind := range []string{"pr", "issue"} {
		for _, ak := range []string{authorKindHuman, authorKindHive, authorKindClanker, ""} {
			for _, st := range stages {
				for vi, v := range variants {
					it := StageTrackerItem{Kind: kind, Repo: "acme/docs", Number: 1000 + vi, Author: "pat", AuthorKind: ak, Stage: st}
					v(&it)
					it.StageActor = stageTrackerStageActor(it, nil, now)
					if it.StageActor == nil || it.StageActor.Kind != StageActorHuman {
						continue
					}
					seen++
					got := stageTrackerHumanReason(it, nil)
					if got == "" {
						t.Errorf("%s stage %03d variant %d (%s): human marker without human_reason (why=%q)", kind, st, vi, ak, it.StageActor.Why)
						continue
					}
					if n := utf8.RuneCountInString(got); n > stageTrackerHumanReasonMax {
						t.Errorf("%s stage %03d variant %d: human_reason %q is %d runes (max %d)", kind, st, vi, got, n, stageTrackerHumanReasonMax)
					}
				}
			}
		}
	}
	if seen < 20 {
		t.Fatalf("only %d human-actor variants exercised; the matrix no longer reaches the human branches", seen)
	}
	if got := stageTrackerHumanReason(StageTrackerItem{Kind: "pr", Stage: trackerNumberFixing, StageActor: &StageTrackerActor{Kind: StageActorHive}}, nil); got != "" {
		t.Fatalf("a hive-actor row must not carry a human_reason, got %q", got)
	}
}

// TestStageTrackerHumanReasonWords: the chevron clause names what the
// person is wanted for, in the words the owner asked for.
func TestStageTrackerHumanReasonWords(t *testing.T) {
	human := &StageTrackerActor{Kind: StageActorHuman, Why: "a maintainer must decide"}
	for _, tc := range []struct {
		name string
		it   StageTrackerItem
		want string
	}{
		{"085", StageTrackerItem{Kind: "pr", Stage: trackerNumberNeedsDecision, StageActor: human}, "needs a decision"},
		{"085 reviewer", StageTrackerItem{Kind: "pr", Stage: trackerNumberNeedsDecision, StageActor: human, DecisionNeeded: &StageTrackerDecision{AskedBy: decisionAskedByReviewer}}, "reviewer's question"},
		{"085 redundant", StageTrackerItem{Kind: "pr", Stage: trackerNumberNeedsDecision, StageActor: human, DecisionNeeded: &StageTrackerDecision{AskedBy: decisionAskedByReviewer, RedundantOf: 9}}, "close as redundant?"},
		{"sentinel", StageTrackerItem{Kind: "pr", Stage: trackerNumberNeedsDecision, StageActor: human, SentinelAlert: true}, "security finding"},
		{"045", StageTrackerItem{Kind: "issue", Stage: trackerNumberIssueDecision, StageActor: human}, "needs a decision"},
		{"045 unauthorized", StageTrackerItem{Kind: "issue", Stage: trackerNumberIssueDecision, StageActor: human, Reason: prstage.StageReasonUnauthorizedPrefix + prstage.UnauthorizedStepApproveWorkflowRuns}, "hive not permitted"},
		{"090 workflow files", StageTrackerItem{Kind: "pr", Stage: trackerNumberReadyForHuman, StageActor: human, Reason: prstage.ContributorLaneReasonWorkflowPath}, "workflow files"},
		{"090 plain", StageTrackerItem{Kind: "pr", Stage: trackerNumberReadyForHuman, StageActor: human}, "a maintainer merges"},
		{"advisory", StageTrackerItem{Kind: "issue", Stage: trackerNumberActionable, StageActor: human, Advisory: &StageTrackerAdvisory{Caption: "x"}}, "read & close"},
		{"triage", StageTrackerItem{Kind: "issue", Stage: trackerNumberTriageNeeded, StageActor: human}, "needs triage"},
		{"prior art", StageTrackerItem{Kind: "issue", Stage: trackerNumberPossiblyExists, StageActor: human}, "confirm prior art"},
		{"070 human author", StageTrackerItem{Kind: "pr", Stage: trackerNumberFixing, Author: "pat", StageActor: human}, "@pat pushes a fix"},
		{"071", StageTrackerItem{Kind: "pr", Stage: trackerNumberNeedsAuthor, Author: "pat", StageActor: human}, "@pat must act"},
		{"superseded", StageTrackerItem{Kind: "pr", Stage: trackerNumberReturned, StageActor: human, Superseded: &github.ReviewQueueSuperseded{CloserPR: 3}}, "superseded — close?"},
		{"fallback clips the why", StageTrackerItem{Kind: "pr", Stage: trackerNumberReviewed, StageActor: &StageTrackerActor{Kind: StageActorHuman, Why: "the hive is waiting for the repository owner to flip the switch on auto-merge"}}, "the hive is waiting for the repository…"},
	} {
		if got := stageTrackerHumanReason(tc.it, nil); got != tc.want {
			t.Errorf("%s: human_reason = %q, want %q", tc.name, got, tc.want)
		}
	}
	covering := &StageTrackerItem{Kind: "pr", Number: 15759, Stage: trackerNumberNeedsDecision, StageActor: human}
	if got := stageTrackerHumanReason(StageTrackerItem{Kind: "issue", Stage: trackerNumberCoveredByPR, StageActor: human}, covering); got != "#15759 needs a decision" {
		t.Fatalf("covered issue human_reason = %q", got)
	}
	covering.Stage, covering.Reason = trackerNumberReadyForHuman, prstage.ContributorLaneReasonWorkflowPath
	if got := stageTrackerHumanReason(StageTrackerItem{Kind: "issue", Stage: trackerNumberCoveredByPR, StageActor: human}, covering); got != "#15759 needs a human merge" {
		t.Fatalf("covered issue (090 PR) human_reason = %q", got)
	}
}

// TestStageTrackerCoveredIssueDecisionPending (hivecommons/hive#15773:
// #15503, a secondary issue "under #15502" covered by PR #15759 at 085,
// showed the PR's "Let hive decide" chip and "PR in progress 👤"): the
// issues riding a PR parked on a decision carry decision_pending_on, the
// covered-decision caption and a human_reason that names the PR; the PR
// alone keeps decision_needed.
func TestStageTrackerCoveredIssueDecisionPending(t *testing.T) {
	human := &StageTrackerActor{Kind: StageActorHuman, Login: "", Why: "a maintainer must decide: the reviewer asked whether the schema change is intended"}
	decision := &StageTrackerDecision{AskedBy: decisionAskedByReviewer, Question: "Is the schema change intended?"}
	lp := StageTrackerLinkedPR{Number: 15759, DrawnUnder: 15502, DecisionNeeded: decision}
	items := []StageTrackerItem{
		{Kind: "issue", Repo: "acme/docs", Number: 15502, Stage: trackerNumberCoveredByPR, Caption: "Covered by PR #15759 — a maintainer must decide", LinkedPRs: []StageTrackerLinkedPR{lp}, DecisionNeeded: decision, StageActor: &StageTrackerActor{Kind: StageActorHive}},
		{Kind: "issue", Repo: "acme/docs", Number: 15503, Stage: trackerNumberCoveredByPR, Caption: "Covered by PR #15759 — a maintainer must decide", LinkedPRs: []StageTrackerLinkedPR{lp}, DecisionNeeded: decision, StageActor: &StageTrackerActor{Kind: StageActorHive}},
		{Kind: "pr", Repo: "acme/docs", Number: 15759, Stage: trackerNumberNeedsDecision, DecisionNeeded: decision, StageActor: human},
		// A covered issue whose PR is simply being worked: no pending chip.
		{Kind: "issue", Repo: "acme/docs", Number: 15600, Stage: trackerNumberCoveredByPR, LinkedPRs: []StageTrackerLinkedPR{{Number: 15601}}, StageActor: &StageTrackerActor{Kind: StageActorHive}},
		{Kind: "pr", Repo: "acme/docs", Number: 15601, Stage: trackerNumberReviewPending, StageActor: &StageTrackerActor{Kind: StageActorHive}},
	}
	stageTrackerResolveActors(items)
	stageTrackerResolveHumanReasons(items)
	byNum := map[int]StageTrackerItem{}
	for _, it := range items {
		byNum[it.Number] = it
	}
	for _, n := range []int{15502, 15503} {
		it := byNum[n]
		if it.DecisionPendingOn != 15759 {
			t.Errorf("#%d decision_pending_on = %d, want 15759", n, it.DecisionPendingOn)
		}
		if it.DecisionNeeded != nil {
			t.Errorf("#%d still carries decision_needed; only the PR row offers the decision", n)
		}
		if it.Caption != "Covered by PR #15759 — waiting for a decision on the PR" {
			t.Errorf("#%d caption = %q", n, it.Caption)
		}
		if it.StageActor == nil || it.StageActor.Kind != StageActorHuman {
			t.Errorf("#%d actor = %+v, want the PR's human actor", n, it.StageActor)
		}
		if it.HumanReason != "#15759 needs a decision" {
			t.Errorf("#%d human_reason = %q", n, it.HumanReason)
		}
		if len(it.LinkedPRs) != 1 || it.LinkedPRs[0].HumanReason != "reviewer's question" {
			t.Errorf("#%d linked PR human_reason = %+v", n, it.LinkedPRs)
		}
	}
	if pr := byNum[15759]; pr.DecisionNeeded == nil || pr.DecisionPendingOn != 0 || pr.HumanReason != "reviewer's question" {
		t.Fatalf("PR row = decision_needed %v pending_on %d reason %q", pr.DecisionNeeded != nil, pr.DecisionPendingOn, pr.HumanReason)
	}
	if it := byNum[15600]; it.DecisionPendingOn != 0 || it.HumanReason != "" || !strings.HasPrefix(it.Caption, "") {
		t.Fatalf("issue riding a working PR: pending_on %d reason %q", it.DecisionPendingOn, it.HumanReason)
	}
}
