package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

func TestStageTrackerRevisionAdministrationIsNotCodeWork(t *testing.T) {
	now := time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)
	pr, dispatch, status := trackerActivityFixture(now)
	dispatch.Fixes = []review.PendingFix{{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Agent: "fixer", Dispatched: now}}
	it := StageTrackerItem{Kind: "pr", Repo: pr.Repo, Number: pr.Number, Head: pr.HeadSHA, Stage: trackerNumberFixing}
	verified := *status.Agents[1].CodeWork
	status.Agents[1].CodeWork = nil
	for _, action := range []string{github.AuditActionAgentCommentCreated, github.AuditActionIssueClaimed,
		github.AuditActionAgentLabelApplied, github.AuditActionAgentReviewRequested} {
		status.Agents[1].LastAction.Action = action
		stageTrackerApplyCodeActivity(&it, dispatch, status, now)
		if it.CodeActivity.Active || it.CodeActivity.State != "assigned" || it.DisplayColumn != trackerNumberReviewPending {
			t.Fatalf("%s manufactured source work: %+v", action, it.CodeActivity)
		}
	}
	for _, mutate := range []func(*crux.CodeWork){
		func(a *crux.CodeWork) { a.Repo = "other/repo" },
		func(a *crux.CodeWork) { a.Head = "old-head" },
		func(a *crux.CodeWork) { a.Source = "agent-says-investigating" },
		func(a *crux.CodeWork) { a.At = now.Add(-time.Second) },
		func(a *crux.CodeWork) { a.KickAt = now.Add(-time.Minute) },
		func(a *crux.CodeWork) { a.ObservedAt = time.Time{} },
	} {
		observation := verified
		mutate(&observation)
		status.Agents[1].CodeWork = &observation
		stageTrackerApplyCodeActivity(&it, dispatch, status, now)
		if it.CodeActivity.Active {
			t.Fatalf("unbound source observation manufactured coding: %+v", observation)
		}
	}
	status.Agents[1].CodeWork = &verified
	dispatch.Fixes[0].Number++
	stageTrackerApplyCodeActivity(&it, dispatch, status, now)
	if it.CodeActivity.Active {
		t.Fatal("another PR's assignment manufactured coding")
	}
}

func TestStageTrackerRevisionOwnReceiptSettlesConcurrentAssignment(t *testing.T) {
	now := time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)
	pr, dispatch, status := trackerActivityFixture(now)
	dispatch.Pending = []review.PendingReview{
		{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Perspective: "quality", Head: "alpha", Agent: "A", ReviewModel: "model-A", Dispatched: now},
		{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Perspective: "quality", Head: "beta", Agent: "B", ReviewModel: "model-B", Dispatched: now},
	}
	status.Agents[0].Name = "B"
	bundle := &evidence.Bundle{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, UpdatedAt: now,
		Verdicts: []evidence.Verdict{
			{Perspective: "quality", Backend: "backend-A", Model: "model-A", Verdict: "approve", RecordedAt: now},
			{Perspective: "quality", Backend: "backend-B", Model: "model-B", Verdict: "approve", RecordedAt: now},
		}}
	agg := &review.Aggregate{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, RecordedAt: now,
		Verdict: "approve", MergeEligible: true, Perspectives: map[review.Perspective]review.Verdict{"quality": "approve"},
		Heads:          map[review.Perspective][]string{"quality": {"alpha", "beta"}},
		ReportReceipts: []review.ReportReceipt{{Agent: "A", Perspective: "quality", Head: "alpha", ReceivedAt: now}}}
	for _, stale := range []review.ReportReceipt{
		{Agent: "A", Perspective: "quality", Head: "beta", ReceivedAt: now},
		{Agent: "B", Perspective: "quality", Head: "alpha", ReceivedAt: now},
		{Agent: "B", Perspective: "quality", Head: "beta", Decided: true, ReceivedAt: now},
		{Agent: "B", Perspective: "quality", Head: "beta", ReceivedAt: now.Add(-time.Second)},
		{Agent: "B", Perspective: "quality", Head: "beta", ReceivedAt: now.Add(time.Second)},
	} {
		agg.ReportReceipts = append(agg.ReportReceipts[:1], stale)
		if got := stageTrackerReviewActivity(pr, bundle, agg, dispatch, status, now); !got.Active || got.Actor != "B" {
			t.Fatalf("A's bundle/tied or mismatched receipt hid B: %+v", got)
		}
	}
	agg.ReportReceipts[1] = review.ReportReceipt{Agent: "B", Perspective: "quality", Head: "beta", ReceivedAt: now}
	if got := stageTrackerReviewActivity(pr, bundle, agg, dispatch, status, now); got.Active || got.State != "approved" {
		t.Fatalf("B's own report did not settle B: %+v", got)
	}
	agg.HeadSHA = "old-head"
	if got := stageTrackerReviewActivity(pr, bundle, agg, dispatch, status, now); !got.Active || got.Actor != "B" {
		t.Fatalf("old-head receipts hid current-round B: %+v", got)
	}
}

func TestStageTrackerRevisionPushAndRetryHaveFreshProvenance(t *testing.T) {
	now := time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)
	pr, dispatch, status := trackerActivityFixture(now)
	status.Agents[1].CodeWork = nil
	dispatch.Fixes = []review.PendingFix{{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Agent: "fixer", Dispatched: now}}
	for _, stage := range []int{trackerNumberFixing, trackerNumberNeedsAuthor, trackerNumberReturned, trackerNumberAdopted} {
		for _, state := range []string{pushStatePushed, pushStateChecking} {
			it := StageTrackerItem{Kind: "pr", Repo: pr.Repo, Number: pr.Number, Head: pr.HeadSHA, Stage: stage, Author: "owner",
				Push: &StageTrackerPush{State: state, Sha: pr.HeadSHA, Since: &now}}
			stageTrackerApplyCodeActivity(&it, dispatch, status, now)
			if it.CodeActivity.Actor != "" || it.CodeActivity.ObservedAt != nil || it.CodeActivity.Active ||
				it.CodeActivity.Source == "fix-dispatch" || it.DisplayColumn != trackerNumberReviewPending {
				t.Fatalf("worker/owner provenance leaked into %s: %+v", state, it.CodeActivity)
			}
			if state == pushStateChecking && strings.Contains(it.CodeActivity.Caption, "New head") {
				t.Fatal("same-head retry fabricated a new push")
			}
		}
	}
}

func TestStageTrackerRevisionCachedNameOnlyPolicyRemainsUnknown(t *testing.T) {
	now := time.Now()
	pr := github.PullRequest{HeadSHA: "head", CIActivity: &github.PRCIActivity{
		Head: "head", Source: "cached-checks", ObservedAt: &now,
		RequiredKnown: true, Required: []string{"build"},
		Checks: []github.PRActivityCheck{{Name: "build", State: "completed", Conclusion: "success"}},
	}}
	if got := stageTrackerCIActivity(pr, now); got.State != "unknown" || got.Active {
		t.Fatalf("old name-only payload manufactured passing CI: %+v", got)
	}
}

func TestStageTrackerRevisionMultipleRequiredAppConstraints(t *testing.T) {
	now := time.Now()
	first, second := int64(11), int64(22)
	pr := github.PullRequest{HeadSHA: "head", CIActivity: &github.PRCIActivity{
		Head: "head", Source: "checks", ObservedAt: &now,
		RequiredKnown: true, RequiredSource: "rules", Required: []string{"build"},
		RequiredChecks: []github.PRRequiredCheck{
			{Name: "build", AppID: &first, AppKnown: true},
			{Name: "build", AppID: &second, AppKnown: true},
		},
		Checks: []github.PRActivityCheck{
			{Name: "build", AppID: &first, State: "completed", Conclusion: "success"},
			{Name: "build", AppID: &second, State: "completed", Conclusion: "success"},
		},
	}}
	if got := stageTrackerCIActivity(pr, now); got.State != "passing" {
		t.Fatalf("distinct required producers became an ambiguous retry: %+v", got)
	}
	pr.CIActivity.Checks[0].Conclusion = "failure"
	if got := stageTrackerCIActivity(pr, now); got.State != "failing" {
		t.Fatalf("a required producer's failure was hidden: %+v", got)
	}
}
