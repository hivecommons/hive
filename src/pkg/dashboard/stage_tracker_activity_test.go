package dashboard

import (
	"os/exec"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

func TestStageTrackerActivityRequiredCI(t *testing.T) {
	now := time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, state, conclusion, want string
		known                         bool
	}{
		{"running", "in_progress", "", "running", true},
		{"queued retry", "queued", "", "queued", true},
		{"failure", "completed", "failure", "failing", true},
		{"success", "completed", "success", "passing", true},
		{"conditional skip", "completed", "skipped", "skipped", true},
		{"unknown required set", "completed", "failure", "unknown", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := github.PullRequest{HeadSHA: "head", CIActivity: &github.PRCIActivity{
				Head: "head", Source: "github-check-runs", ObservedAt: &now, RequiredKnown: tc.known, Required: []string{"build"}, StatusesKnown: true,
				RequiredSource: "protection", RequiredChecks: []github.PRRequiredCheck{{Name: "build", AppKnown: true}},
				Checks: []github.PRActivityCheck{{Name: "build", State: tc.state, Conclusion: tc.conclusion},
					{Name: "tide", State: "queued"}, {Name: "optional", State: "completed", Conclusion: "failure"}},
			}}
			got := stageTrackerCIActivity(pr, now)
			if got.State != tc.want || got.Active != (tc.want == "running") {
				t.Fatalf("activity = %+v, want %s", got, tc.want)
			}
			pr.CIActivity.Head = "old-head"
			if got := stageTrackerCIActivity(pr, now); got.State != "unknown" || got.Active {
				t.Fatalf("old head leaked: %+v", got)
			}
		})
	}
	pr := github.PullRequest{HeadSHA: "head", CIActivity: &github.PRCIActivity{Head: "head", RequiredKnown: true, Required: []string{"missing"},
		RequiredSource: "protection", RequiredChecks: []github.PRRequiredCheck{{Name: "missing", AppKnown: true}}, StatusesKnown: true}}
	if got := stageTrackerCIActivity(pr, now); got.State != "missing" {
		t.Fatalf("missing run became a verdict: %+v", got)
	}
	pr.CIActivity.Checks = []github.PRActivityCheck{{Name: "missing", State: "in_progress"}}
	for _, at := range []time.Time{now.Add(-trackerActivityFreshWindow - time.Second), now.Add(time.Second)} {
		pr.CIActivity.ObservedAt = &at
		if got := stageTrackerCIActivity(pr, now); got.Active {
			t.Fatalf("stale or future CI manufactured current activity: %+v", got)
		}
	}
}

func trackerActivityFixture(now time.Time) (github.PullRequest, review.DispatchState, *StatusPayload) {
	pr := github.PullRequest{Repo: "org/repo", Number: 7, Author: "author", HeadSHA: "head"}
	dispatch := review.DispatchState{GeneratedAt: now, Pending: []review.PendingReview{
		{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Agent: "reviewer", Dispatched: now},
	}}
	status := &StatusPayload{Agents: []FrontendAgent{
		{Name: "reviewer", Busy: "working", LastAction: &FrontendAgentLastAction{Repo: pr.Repo, Number: pr.Number, Action: github.AuditActionAgentCommentCreated, Current: true, At: now.Format(time.RFC3339Nano)}},
		{Name: "fixer", Busy: "working", LastAction: &FrontendAgentLastAction{Repo: pr.Repo, Number: pr.Number, Action: github.AuditActionAgentCommentCreated, Current: true, At: now.Format(time.RFC3339Nano)}},
	}}
	status.Agents[1].CodeWork = &crux.CodeWork{Repo: pr.Repo, Head: pr.HeadSHA, Path: "source.go",
		Source: crux.CodeWorkSource, At: now, ObservedAt: now, KickAt: now}
	return pr, dispatch, status
}

func TestStageTrackerActivityParallelAndReviewHead(t *testing.T) {
	now := time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)
	pr, dispatch, status := trackerActivityFixture(now)
	pr.CIActivity = &github.PRCIActivity{Head: pr.HeadSHA, Source: "github-check-runs", ObservedAt: &now, RequiredKnown: true,
		RequiredSource: "protection", RequiredChecks: []github.PRRequiredCheck{{Name: "build", AppKnown: true}},
		Required: []string{"build"}, Checks: []github.PRActivityCheck{{Name: "build", State: "in_progress"}}}
	ci := stageTrackerCIActivity(pr, now)
	rev := stageTrackerReviewActivity(pr, nil, nil, dispatch, status, now)
	if !ci.Active || !rev.Active || rev.Actor != "reviewer" {
		t.Fatalf("tracks not parallel: CI=%+v Review=%+v", ci, rev)
	}
	dispatch.Pending = nil
	dispatch.Waits = []review.ReviewWaitRecord{{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, FirstAt: now, LastAt: now}}
	if got := stageTrackerReviewActivity(pr, nil, nil, dispatch, status, now); got.State != "unassigned" || got.Actor != "" || got.Active {
		t.Fatalf("unassigned reviewer invented: %+v", got)
	}
	bundle := &evidence.Bundle{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, UpdatedAt: now,
		Verdicts: []evidence.Verdict{{Perspective: "quality", Verdict: "changes_requested", RecordedAt: now}}}
	agg := &review.Aggregate{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Verdict: review.Verdict("changes_requested")}
	dispatch.Waits = nil
	pr.CIActivity.Checks[0].State, pr.CIActivity.Checks[0].Conclusion = "completed", "success"
	if stageTrackerCIActivity(pr, now).State != "passing" || stageTrackerReviewActivity(pr, bundle, agg, dispatch, status, now).State != "changes_requested" {
		t.Fatal("green CI suppressed review findings")
	}
	pr.CIActivity.Checks[0].Conclusion = "failure"
	agg.Verdict, agg.MergeEligible = review.Verdict("approve"), true
	agg.Perspectives = map[review.Perspective]review.Verdict{"quality": "approve"}
	bundle.Verdicts[0].Verdict = "approve"
	if stageTrackerCIActivity(pr, now).State != "failing" || stageTrackerReviewActivity(pr, bundle, agg, dispatch, status, now).State != "approved" {
		t.Fatal("review approval suppressed CI failure")
	}
	pr.HeadSHA = "new-head"
	if got := stageTrackerReviewActivity(pr, bundle, agg, dispatch, status, now); got.State != "unknown" || got.Actor != "" {
		t.Fatalf("old review survived new push: %+v", got)
	}
}

func TestStageTrackerActivitySourceRepairAndWaits(t *testing.T) {
	now := time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)
	pr, dispatch, status := trackerActivityFixture(now)
	it := StageTrackerItem{Repo: pr.Repo, Number: pr.Number, Kind: "pr", Author: pr.Author, Head: pr.HeadSHA, Stage: trackerNumberFixing}
	stageTrackerApplyCodeActivity(&it, dispatch, status, now)
	if it.DisplayColumn != trackerNumberReviewPending || it.CodeActivity.Active || it.CodeActivity.State != "waiting_fixer" {
		t.Fatalf("070 manufactured fixing: %+v", it.CodeActivity)
	}
	dispatch.Fixes = []review.PendingFix{{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Agent: "fixer", Dispatched: now}}
	stageTrackerApplyCodeActivity(&it, dispatch, status, now)
	if it.Stage != trackerNumberFixing || it.DisplayColumn != trackerNumberPRInProgress || !it.CodeActivity.Active || it.CodeActivity.Actor != "fixer" {
		t.Fatalf("repair failed to return visually: %+v", it)
	}
	if !stageTrackerReviewActivity(pr, nil, nil, dispatch, status, now).Active {
		t.Fatal("fixer hid concurrent reviewer")
	}
	it.Head = "new-head"
	it.Push = &StageTrackerPush{State: pushStatePushed, Since: &now}
	stageTrackerApplyCodeActivity(&it, dispatch, status, now)
	if it.DisplayColumn != trackerNumberReviewPending || it.CodeActivity.Active || it.CodeActivity.State != "pushed" {
		t.Fatalf("new push failed to return to tracks: %+v", it.CodeActivity)
	}
	dispatch.Fixes[0].HeadSHA = it.Head
	resumed := now.Add(time.Second)
	status.Agents[1].LastAction.At = resumed.Format(time.RFC3339Nano)
	status.Agents[1].CodeWork.Head, status.Agents[1].CodeWork.At, status.Agents[1].CodeWork.ObservedAt = it.Head, resumed, resumed
	stageTrackerApplyCodeActivity(&it, dispatch, status, resumed)
	if it.DisplayColumn != trackerNumberPRInProgress || !it.CodeActivity.Active {
		t.Fatal("a persistent push marker hid later current-head source work")
	}
	status.Agents[1].LastAction.At = now.Format(time.RFC3339Nano)
	it.Push = nil
	dispatch.Fixes = nil
	for _, stage := range []int{trackerNumberNeedsAuthor, trackerNumberReturned, trackerNumberAdopted, trackerNumberNeedsDecision} {
		it.Stage = stage
		stageTrackerApplyCodeActivity(&it, dispatch, status, now)
		if it.CodeActivity.Active || (stage == trackerNumberNeedsDecision && it.DisplayColumn != 0) ||
			(stage != trackerNumberNeedsDecision && it.DisplayColumn != trackerNumberPRInProgress) {
			t.Fatalf("owner/adoption/human gate manufactured coding at %d: %+v", stage, it)
		}
	}
	dispatch.Fixes = []review.PendingFix{{Repo: pr.Repo, Number: pr.Number, HeadSHA: it.Head, Agent: "fixer", Dispatched: now}}
	it.Stage = trackerNumberFixing
	stageTrackerApplyCodeActivity(&it, dispatch, status, now.Add(trackerActivityFreshWindow+time.Second))
	if it.CodeActivity.Active {
		t.Fatal("stale worker manufactured active coding")
	}
}

func TestStageTrackerActivityJavaScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	if output, err := exec.Command(node, "--test", "stage_tracker_activity.test.js").CombinedOutput(); err != nil {
		t.Fatalf("activity JavaScript: %v\n%s", err, output)
	}
}

func TestStageTrackerActivityLinkedPRsPreserveCountsAndHeads(t *testing.T) {
	items := []StageTrackerItem{
		{Repo: "org/repo", Kind: "issue", Number: 1, LinkedPRs: []StageTrackerLinkedPR{{Number: 7}, {Number: 8}}},
		{Repo: "org/repo", Kind: "pr", Number: 7, Head: "head-7", Stage: trackerNumberFixing, DisplayColumn: trackerNumberPRInProgress,
			CIActivity: &StageTrackerActivity{Head: "head-7", State: "running"}, ReviewActivity: &StageTrackerActivity{Head: "head-7", State: "reviewing"}},
		{Repo: "org/repo", Kind: "pr", Number: 8, Head: "head-8", Stage: trackerNumberReviewPending,
			CIActivity: &StageTrackerActivity{Head: "head-8", State: "passing"}, ReviewActivity: &StageTrackerActivity{Head: "head-8", State: "changes_requested"}},
	}
	issues, prs := stageTrackerOpenCounts(items)
	stageTrackerResolveLinkedPRs(items, func(string, int) bool { return true })
	gotIssues, gotPRs := stageTrackerOpenCounts(items)
	if issues != gotIssues || prs != gotPRs || issues != 1 || prs != 2 {
		t.Fatalf("parallel tracks changed item counts: before=%d/%d after=%d/%d", issues, prs, gotIssues, gotPRs)
	}
	linked := items[0].LinkedPRs
	if len(linked) != 2 || linked[0].CIActivity != items[1].CIActivity || linked[1].ReviewActivity != items[2].ReviewActivity ||
		linked[0].DisplayColumn != trackerNumberPRInProgress || linked[0].Head == linked[1].Head {
		t.Fatalf("linked tracks merged unrelated heads: %+v", linked)
	}
}
func TestStageTrackerActivityApprovalRequiresConsistentBundle(t *testing.T) {
	now := time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)
	pr, dispatch, status := trackerActivityFixture(now)
	dispatch.Pending = nil
	agg := &review.Aggregate{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA,
		Verdict: "approve", MergeEligible: true, Perspectives: map[review.Perspective]review.Verdict{"quality": "approve", "security": "approve"}}
	bundle := &evidence.Bundle{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, UpdatedAt: now,
		Verdicts: []evidence.Verdict{
			{Perspective: "quality", Backend: "backend", Model: "model", Verdict: "approve", RecordedAt: now},
			{Perspective: "security", Backend: "backend", Model: "model", Verdict: "changes_requested", RecordedAt: now},
		}}
	if got := stageTrackerReviewActivity(pr, bundle, agg, dispatch, status, now); got.State != "unknown" {
		t.Fatalf("conflicting bundle became approval: %+v", got)
	}
	bundle.Verdicts = bundle.Verdicts[:1]
	if got := stageTrackerReviewActivity(pr, bundle, agg, dispatch, status, now); got.State != "unknown" {
		t.Fatalf("missing perspective became approval: %+v", got)
	}
	bundle.Verdicts = append(bundle.Verdicts, evidence.Verdict{
		Perspective: "security", Backend: "backend", Model: "model", Verdict: "changes_requested", RecordedAt: now.Add(-time.Minute)},
		evidence.Verdict{Perspective: "security", Backend: "backend", Model: "model", Verdict: "approve", RecordedAt: now})
	dispatch.Recent = []review.RecentReview{{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Perspective: "quality", Agent: "actual-reviewer", Confirmed: now}}
	agg.ReportReceipts = []review.ReportReceipt{{Perspective: "quality", Agent: "actual-reviewer", ReceivedAt: now}}
	if got := stageTrackerReviewActivity(pr, bundle, agg, dispatch, status, now); got.State != "approved" || got.Actor != "actual-reviewer" {
		t.Fatalf("latest same-head approvals did not supersede findings: %+v", got)
	}
	bundle.Verdicts = append(bundle.Verdicts, evidence.Verdict{
		Perspective: "security", Backend: "backend", Model: "model", Verdict: "reject", RecordedAt: now})
	if got := stageTrackerReviewActivity(pr, bundle, agg, dispatch, status, now); got.State != "unknown" {
		t.Fatalf("equal-time conflicting verdict became approval: %+v", got)
	}
	agg.HeadSHA = "old-head"
	if got := stageTrackerReviewActivity(pr, bundle, agg, dispatch, status, now); got.State != "changes_requested" {
		t.Fatalf("old aggregate hid current bundle findings: %+v", got)
	}
}

func TestStageTrackerActivityCIRetryAndMixedChecks(t *testing.T) {
	now := time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)
	pr, dispatch, status := trackerActivityFixture(now)
	pr.CIActivity = &github.PRCIActivity{Head: pr.HeadSHA, Source: "github-check-runs", ObservedAt: &now,
		RequiredSource: "protection", RequiredChecks: []github.PRRequiredCheck{{Name: "build", AppKnown: true}, {Name: "lint", AppKnown: true}},
		RequiredKnown: true, Required: []string{"build", "lint"}, Checks: []github.PRActivityCheck{
			{Name: "build", State: "completed", Conclusion: "failure"}, {Name: "lint", State: "in_progress"},
		}}
	if got := stageTrackerCIActivity(pr, now); got.State != "failing" || !got.Active {
		t.Fatalf("failing required check hid simultaneous running CI: %+v", got)
	}
	pr.CIActivity.Checks = append(pr.CIActivity.Checks, github.PRActivityCheck{Name: "build", State: "completed", Conclusion: "success"})
	if got := stageTrackerCIActivity(pr, now); got.State != "unknown" {
		t.Fatalf("ambiguous check apps manufactured success: %+v", got)
	}
	dispatch.Fixes = []review.PendingFix{{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Agent: "fixer", Dispatched: now}}
	status.Agents[1].AwaitingCI = true
	it := StageTrackerItem{Repo: pr.Repo, Kind: "pr", Number: pr.Number, Head: pr.HeadSHA, Stage: trackerNumberFixing}
	stageTrackerApplyCodeActivity(&it, dispatch, status, now)
	if it.DisplayColumn != trackerNumberReviewPending || it.CodeActivity.Active || it.CodeActivity.State != "waiting_ci" {
		t.Fatalf("CI polling regressed into coding: %+v", it)
	}
	it.Push = &StageTrackerPush{State: pushStateChecking, Sha: it.Head}
	stageTrackerApplyCodeActivity(&it, dispatch, status, now)
	if it.DisplayColumn != trackerNumberReviewPending || it.CodeActivity.Active || it.CodeActivity.Source != "recorded-ci-retry" {
		t.Fatalf("same-head retry invented coding or a new push: %+v", it)
	}
	it.Push = nil
	status.Agents[1].AwaitingCI = false
	status.Agents[1].LastAction.At = now.Add(-time.Second).Format(time.RFC3339Nano)
	status.Agents[1].CodeWork.At = now.Add(-time.Second)
	stageTrackerApplyCodeActivity(&it, dispatch, status, now)
	if it.CodeActivity.Active {
		t.Fatal("an action before fix assignment manufactured observed coding")
	}
	status.Agents[1].Paused = true
	status.Agents[1].LastAction.At = now.Format(time.RFC3339Nano)
	status.Agents[1].CodeWork.At = now
	stageTrackerApplyCodeActivity(&it, dispatch, status, now)
	if it.CodeActivity.Active {
		t.Fatal("a paused fixer manufactured observed coding")
	}
	status.Agents[1].Paused = false
	status.Agents[1].LastAction.Action = ""
	status.Agents[1].CodeWork = nil
	stageTrackerApplyCodeActivity(&it, dispatch, status, now)
	if it.CodeActivity.Active {
		t.Fatal("a missing action kind manufactured observed coding")
	}
}

func TestStageTrackerActivitySelectsObservedReviewer(t *testing.T) {
	now := time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)
	pr, dispatch, status := trackerActivityFixture(now)
	dispatch.Pending = append([]review.PendingReview{
		{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Agent: "idle-reviewer", Dispatched: now},
	}, dispatch.Pending...)
	if got := stageTrackerReviewActivity(pr, nil, nil, dispatch, status, now); !got.Active || got.Actor != "reviewer" {
		t.Fatalf("an idle assignment hid another head's active reviewer: %+v", got)
	}
	dispatch.Pending = dispatch.Pending[1:]
	dispatch.Pending[0].Decision = true
	if got := stageTrackerReviewActivity(pr, nil, nil, dispatch, status, now); got.State != "adjudicating" {
		t.Fatalf("adjudication was described as an independent reviewer: %+v", got)
	}
}
