package dashboard

import (
	"fmt"
	"os/exec"
	"strings"
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
		// A queued neighbour keeps an ambiguous-required ring in motion
		// (hivecommons/hive#16631).
		{"unknown required set", "completed", "failure", "running", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := github.PullRequest{HeadSHA: "head", CIActivity: &github.PRCIActivity{
				Head: "head", Source: "github-check-runs", ObservedAt: &now, RequiredKnown: tc.known, Required: []string{"build"}, StatusesKnown: true,
				RequiredSource: "protection", RequiredChecks: []github.PRRequiredCheck{{Name: "build", AppKnown: true}},
				Checks: []github.PRActivityCheck{{Name: "build", State: tc.state, Conclusion: tc.conclusion},
					{Name: "lint", State: "queued"}, {Name: "optional", State: "completed", Conclusion: "failure"}},
			}}
			got := stageTrackerCIActivity(pr, now)
			if got.State != tc.want || got.Active != (tc.want == "running" || tc.want == "queued") {
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
	// A running check is active whatever the poll's age: one fill, the
	// observation time in the tooltip (hivecommons/hive#16643).
	for _, at := range []time.Time{now.Add(-trackerActivityFreshWindow - time.Second), now.Add(time.Second)} {
		pr.CIActivity.ObservedAt = &at
		if got := stageTrackerCIActivity(pr, now); !got.Active || got.State != "running" {
			t.Fatalf("a stale poll dimmed running CI: %+v", got)
		}
	}
}

// A required Prow presubmit that never ran has no check run to link to;
// Tide's commit status carries the reason. The CI track borrows Tide's
// description and link so it reads differently from a failing check, and
// Tide itself never enters the tally (hivecommons/hive#17476).
func TestStageTrackerCIActivityTideExplainsMissingPresubmit(t *testing.T) {
	now := time.Date(2026, 11, 9, 21, 0, 0, 0, time.UTC)
	const tideDesc = "Not mergeable. Jobs native-queue-policy, verify PR contents have not succeeded."
	const tideURL = "https://prow.example/tide"
	fixture := func(verify ...github.PRActivityCheck) github.PullRequest {
		checks := append([]github.PRActivityCheck{
			{Name: "build", State: "completed", Conclusion: "success", URL: "https://ci.example/build"},
			{Name: "tide", State: "pending", Kind: "commit_status", URL: tideURL, Description: tideDesc},
		}, verify...)
		return github.PullRequest{HeadSHA: "head", CIActivity: &github.PRCIActivity{
			Head: "head", Source: "github-check-runs", ObservedAt: &now, StatusesKnown: true, StatusesObservedAt: &now,
			RequiredKnown: true, RequiredSource: "protection", Required: []string{"build", "verify PR contents"},
			RequiredChecks: []github.PRRequiredCheck{{Name: "build", AppKnown: true}, {Name: "verify PR contents", AppKnown: true}},
			Checks:         checks,
		}}
	}

	got := stageTrackerCIActivity(fixture(), now)
	if got.State != "missing" || got.ChecksTotal != 2 || got.ChecksPassed != 1 || got.ChecksFailed != 0 {
		t.Fatalf("never-ran presubmit: %+v, want missing 1/2 with tide uncounted", got)
	}
	if got.URL != tideURL || !strings.Contains(got.Detail, tideDesc) || !strings.Contains(got.Detail, "verify PR contents is a Prow presubmit") {
		t.Fatalf("never-ran presubmit carries no Tide link or words: url=%q detail=%q", got.URL, got.Detail)
	}

	// A pending Prow job keeps the link to its own run; Tide's words ride along.
	got = stageTrackerCIActivity(fixture(github.PRActivityCheck{Name: "verify PR contents", State: "pending", Kind: "commit_status", URL: "https://prow.example/job/1"}), now)
	if got.State != "queued" || got.URL != "https://prow.example/job/1" || !strings.Contains(got.Detail, tideDesc) {
		t.Fatalf("pending presubmit: %+v", got)
	}

	// A failing check is a failing check: its own link, no Tide sentence.
	got = stageTrackerCIActivity(fixture(github.PRActivityCheck{Name: "verify PR contents", State: "completed", Conclusion: "failure", URL: "https://ci.example/verify"}), now)
	if got.State != "failing" || got.URL != "https://ci.example/verify" || strings.Contains(got.Detail, "Tide") {
		t.Fatalf("failing check borrowed Tide: %+v", got)
	}

	// Tide in the pool says nothing worth showing.
	pr := fixture()
	pr.CIActivity.Checks[1].State, pr.CIActivity.Checks[1].Conclusion = "completed", "success"
	if got = stageTrackerCIActivity(pr, now); got.URL != "" || strings.Contains(got.Detail, "Tide") {
		t.Fatalf("mergeable Tide still annotated: %+v", got)
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
		StatusesKnown: true, StatusesObservedAt: &now,
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
	pr.CIActivity.StatusesKnown = false
	if stageTrackerCIActivity(pr, now).State != "unknown" || stageTrackerReviewActivity(pr, bundle, agg, dispatch, status, now).State != "changes_requested" {
		t.Fatal("an uncollected any-App status side invented green CI or hid review findings")
	}
	pr.CIActivity.StatusesKnown = true
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

func TestStageTrackerCodeActivityPreservesNativeNonCodingFacts(t *testing.T) {
	now := time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)
	for _, caption := range []string{
		"Conflicting with the base branch; bot-authored PR (@automation[bot]) — the hive does not fix bot PRs",
		"Rebasing · attempt 1/2",
		"On hold · waiting for a human",
	} {
		t.Run(caption, func(t *testing.T) {
			it := StageTrackerItem{Kind: "pr", Repo: "org/repo", Number: 7, Head: "head", Stage: trackerNumberFixing, Caption: caption}
			stageTrackerApplyCodeActivity(&it, review.DispatchState{}, nil, now)
			stageTrackerApplyCodeActivity(&it, review.DispatchState{}, nil, now)
			if it.Caption != caption || it.CodeActivity == nil || it.CodeActivity.Active ||
				it.CodeActivity.State != "waiting_fixer" || it.DisplayColumn != trackerNumberReviewPending {
				t.Fatalf("native fact was replaced or invented source work: %+v", it)
			}
		})
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
	if it.Stage != trackerNumberFixing || it.DisplayColumn != trackerNumberPRInProgress || !it.CodeActivity.Active || it.CodeActivity.Actor != "fixer" ||
		it.CodeActivity.State != "fixing" || it.CodeActivity.Caption != "Remediating · fixer" {
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
	stageTrackerApplyCodeActivity(&it, dispatch, status, resumed.Add(trackerActivityFreshWindow))
	if !it.CodeActivity.Active {
		t.Fatal("source work at the existing inclusive freshness boundary was discarded")
	}
	stageTrackerApplyCodeActivity(&it, dispatch, status, resumed.Add(trackerActivityFreshWindow+time.Second))
	if it.CodeActivity.Active || it.DisplayColumn != trackerNumberReviewPending {
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
	// Neither an approval nor "unknown": the words say the evidence does
	// not verify the aggregate (hivecommons/hive#16661).
	if got := stageTrackerReviewActivity(pr, bundle, agg, dispatch, status, now); got.State != reviewStateUnverified || !strings.Contains(got.Caption, "evidence not verified") {
		t.Fatalf("conflicting bundle became approval: %+v", got)
	}
	bundle.Verdicts = bundle.Verdicts[:1]
	if got := stageTrackerReviewActivity(pr, bundle, agg, dispatch, status, now); got.State != reviewStateUnverified {
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
	if got := stageTrackerReviewActivity(pr, bundle, agg, dispatch, status, now); got.State != reviewStateUnverified {
		t.Fatalf("equal-time conflicting verdict became approval: %+v", got)
	}
	agg.HeadSHA = "old-head"
	if got := stageTrackerReviewActivity(pr, bundle, agg, dispatch, status, now); got.State != "changes_requested" {
		t.Fatalf("old aggregate hid current bundle findings: %+v", got)
	}
}

func TestStageTrackerRemediationPausedWordingPreservesEvidence(t *testing.T) {
	now := time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)
	for _, prefix := range []string{"Remediation paused", "Fixing paused"} {
		it := StageTrackerItem{Kind: "pr", Repo: "org/repo", Number: 7, Head: "head", Stage: trackerNumberFixing,
			Caption: prefix + " — attempts exhausted; historical stage Fixing"}
		stageTrackerApplyCodeActivity(&it, review.DispatchState{}, nil, now)
		if it.CodeActivity == nil || it.CodeActivity.Active || it.CodeActivity.State != "waiting_fixer" ||
			it.DisplayColumn != trackerNumberReviewPending || it.CodeActivity.Blocker != "Fix task blocked — attempts exhausted; historical stage Fixing" {
			t.Fatalf("paused remediation or legacy caption invented work or rewrote history: %+v", it)
		}
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
	if got := stageTrackerCIActivity(pr, now); got.State != "running" || !got.Active || got.ChecksFailed != 1 || !strings.Contains(got.Caption, "1 failing") {
		t.Fatalf("failing required check hid simultaneous running CI: %+v", got)
	}
	pr.CIActivity.Checks = append(pr.CIActivity.Checks, github.PRActivityCheck{Name: "build", State: "completed", Conclusion: "success"})
	if got := stageTrackerCIActivity(pr, now); got.State != "running" || got.ChecksPassed != 0 || got.ChecksFailed != 0 {
		t.Fatalf("ambiguous check apps manufactured success: %+v", got)
	}
	pr.CIActivity.Checks[1] = github.PRActivityCheck{Name: "lint", State: "completed", Conclusion: "success"}
	if got := stageTrackerCIActivity(pr, now); got.State != "unknown" {
		t.Fatalf("ambiguous check apps manufactured success once settled: %+v", got)
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

func TestStageTrackerActivityCICaptionsReadInPlainLanguage(t *testing.T) {
	now := time.Date(2026, 11, 3, 23, 0, 0, 0, time.UTC)
	pr, _, _ := trackerActivityFixture(now)
	ci := func(checks ...github.PRActivityCheck) github.PullRequest {
		pr.CIActivity = &github.PRCIActivity{Head: pr.HeadSHA, Source: "github-check-runs", ObservedAt: &now, StatusesKnown: true,
			RequiredSource: "protection", RequiredChecks: []github.PRRequiredCheck{{Name: "build", AppKnown: true}, {Name: "lint", AppKnown: true}, {Name: "test", AppKnown: true}},
			RequiredKnown: true, Required: []string{"build", "lint", "test"}, Checks: checks}
		return pr
	}
	chk := func(name, state, conclusion string) github.PRActivityCheck {
		return github.PRActivityCheck{Name: name, State: state, Conclusion: conclusion}
	}
	done := func(name string) github.PRActivityCheck { return chk(name, "completed", "success") }
	cases := []struct {
		name          string
		pr            github.PullRequest
		state, want   string
		detailMention string
	}{
		{"missing", ci(done("build"), done("lint")), "missing", `CI waiting · required check 'test' hasn't reported for this head yet`, `check named 'test'`},
		{"running counts", ci(done("build"), chk("lint", "in_progress", ""), chk("test", "in_progress", "")), "running", `CI running · 'lint', 'test' pending`, "1 of the 3 required checks"},
		{"queued", ci(done("build"), done("lint"), chk("test", "queued", "")), "queued", `CI queued · 'test' waiting for a runner`, "not started"},
		{"failing", ci(done("build"), chk("lint", "completed", "failure"), done("test")), "failing", `CI failing · 'lint' red`, `'lint' finished without success`},
		{"skipped", ci(done("build"), done("lint"), chk("test", "completed", "skipped")), "skipped", `CI skipped · check 'test' skipped`, "does not count as a pass"},
		{"ambiguous", ci(done("build"), done("lint"), done("test"), chk("test", "completed", "failure")), "unknown", `CI unknown · 'test' reported but with no conclusion`, "several runs share the name"},
		{"green", ci(done("build"), done("lint"), done("test")), "passing", "CI green · all 3 required checks passed", "reported success"},
	}
	for _, tc := range cases {
		got := stageTrackerCIActivity(tc.pr, now)
		if got.State != tc.state || got.Caption != tc.want || !strings.Contains(got.Detail, tc.detailMention) {
			t.Fatalf("%s: want %s %q (detail mentioning %q), got %+v", tc.name, tc.state, tc.want, tc.detailMention, got)
		}
	}
	// Motion leads a settled word: a ring that is not full is running,
	// however red, unreported or ambiguous its neighbours (hivecommons/hive#16631).
	motion := []struct {
		name string
		pr   github.PullRequest
		want string
	}{
		{"red+running", ci(done("build"), chk("lint", "completed", "failure"), chk("test", "in_progress", "")), `CI running · 'test' pending · 1 failing`},
		{"missing+running", ci(done("build"), chk("lint", "in_progress", "")), `CI running · 'lint', 'test' pending`},
	}
	for _, tc := range motion {
		got := stageTrackerCIActivity(tc.pr, now)
		if got.State != "running" || got.Caption != tc.want || !got.Active {
			t.Fatalf("%s: want running %q active, got %+v", tc.name, tc.want, got)
		}
	}
	if got := stageTrackerCIActivity(ci(done("build"), done("lint"), done("lint"), chk("test", "queued", "")), now); got.State != "queued" || got.Caption != `CI queued · 'test' waiting for a runner` {
		t.Fatalf("ambiguous+queued: a waiting runner keeps the ring queued, not unknown: %+v", got)
	}
	if got := stageTrackerCIActivity(ci(done("build"), chk("lint", "completed", "failure"), done("test")), now); got.State != "failing" || got.Active {
		t.Fatalf("settled red head must not read as running: %+v", got)
	}
	if got := stageTrackerCIActivity(github.PullRequest{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA}, now); got.Caption != "CI · no checks observed for head head yet · refreshing" || got.Detail == "" {
		t.Fatalf("no-evidence caption: %+v", got)
	}
}

func TestStageTrackerActivityCICheckCounts(t *testing.T) {
	now := time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)
	required := func(names ...string) []github.PRRequiredCheck {
		out := make([]github.PRRequiredCheck, 0, len(names))
		for _, n := range names {
			out = append(out, github.PRRequiredCheck{Name: n, AppKnown: true})
		}
		return out
	}
	pr := github.PullRequest{HeadSHA: "head", CIActivity: &github.PRCIActivity{
		Head: "head", Source: "github-check-runs", ObservedAt: &now, RequiredKnown: true, StatusesKnown: true,
		Required: []string{"build", "lint", "test"}, RequiredSource: "protection", RequiredChecks: required("build", "lint", "test"),
		Checks: []github.PRActivityCheck{
			{Name: "build", State: "completed", Conclusion: "success"},
			{Name: "lint", State: "completed", Conclusion: "failure"},
			{Name: "test", State: "in_progress"},
			{Name: "optional", State: "completed", Conclusion: "failure"},
		},
	}}
	got := stageTrackerCIActivity(pr, now)
	if got.ChecksPassed != 1 || got.ChecksTotal != 3 || got.ChecksFailed != 1 || strings.Join(got.ChecksFailing, ",") != "lint" {
		t.Fatalf("required counts = %d/%d failed %d %v, want 1/3 failed 1 [lint] (optional checks do not count): %+v", got.ChecksPassed, got.ChecksTotal, got.ChecksFailed, got.ChecksFailing, got)
	}
	// Every required check green: the count is complete and no name is red.
	pr.CIActivity.Checks = []github.PRActivityCheck{
		{Name: "build", State: "completed", Conclusion: "success"}, {Name: "lint", State: "completed", Conclusion: "success"}, {Name: "test", State: "completed", Conclusion: "success"},
	}
	if got := stageTrackerCIActivity(pr, now); got.State != "passing" || got.ChecksPassed != 3 || got.ChecksTotal != 3 || got.ChecksFailed != 0 {
		t.Fatalf("green counts = %+v", got)
	}
	// A required check with no run yet still counts in the total, not in passed.
	pr.CIActivity.Checks = pr.CIActivity.Checks[:2]
	if got := stageTrackerCIActivity(pr, now); got.State != "missing" || got.ChecksPassed != 2 || got.ChecksTotal != 3 {
		t.Fatalf("missing counts = %+v", got)
	}
	// Required set unknown (an unprotected base such as v6): count every
	// check run observed on the head, one per name, latest run decides.
	earlier, later := now.Add(-2*time.Minute), now.Add(-time.Minute)
	pr.CIActivity.RequiredKnown, pr.CIActivity.Required, pr.CIActivity.RequiredChecks = false, nil, nil
	pr.CIActivity.Checks = []github.PRActivityCheck{
		{Name: "build", State: "completed", Conclusion: "success"},
		{Name: "flaky", State: "completed", Conclusion: "failure", Since: &earlier},
		{Name: "flaky", State: "completed", Conclusion: "success", Since: &later},
		{Name: "test", State: "queued"},
		{Name: "other-head", State: "completed", Conclusion: "success", Head: "old-head"},
	}
	got = stageTrackerCIActivity(pr, now)
	if got.State != "running" || got.ChecksTotal != 3 || got.ChecksPassed != 1 || got.ChecksFailed != 1 || strings.Join(got.ChecksFailing, ",") != "flaky" {
		t.Fatalf("head-wide counts = %d/%d failed %d %v (state %s), want 1/3 failed 1 [flaky] running: a red run of a name stays red while the queued one keeps the ring in motion", got.ChecksPassed, got.ChecksTotal, got.ChecksFailed, got.ChecksFailing, got.State)
	}
	// No evidence for the head at all: no count ("CI ?/?").
	pr.CIActivity.Checks = nil
	if got := stageTrackerCIActivity(pr, now); got.ChecksTotal != 0 || got.ChecksPassed != 0 {
		t.Fatalf("empty head produced a count: %+v", got)
	}
}

func TestStageTrackerActivityReviewPerspectiveCounts(t *testing.T) {
	now := time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)
	pr, dispatch, status := trackerActivityFixture(now)
	qualify := stageTrackerRepoQualifier(status)
	expected := stageTrackerExpectedPerspectives(nil)
	if len(expected) != len(review.DefaultPerspectives) {
		t.Fatalf("nil config did not fall back to the built-in set: %v", expected)
	}
	count := func(bundle *evidence.Bundle, agg *review.Aggregate, d review.DispatchState) *StageTrackerActivity {
		out := stageTrackerReviewActivity(pr, bundle, agg, d, status, now)
		stageTrackerCountPerspectives(out, pr, bundle, agg, d, qualify, expected)
		return out
	}
	// Swarm requested, nothing reported yet: 0 of the expected set.
	if got := count(nil, nil, dispatch); got.PerspectivesDone != 0 || got.PerspectivesTotal != len(expected) {
		t.Fatalf("fresh dispatch counts = %d/%d, want 0/%d: %+v", got.PerspectivesDone, got.PerspectivesTotal, len(expected), got)
	}
	// Three of five reported on the current head.
	agg := &review.Aggregate{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Verdict: "changes_requested",
		Perspectives: map[review.Perspective]review.Verdict{review.PerspectiveCorrectness: "approve", review.PerspectiveSecurity: "approve", review.PerspectiveStyle: "changes_requested"}}
	bundle := &evidence.Bundle{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, UpdatedAt: now, Verdicts: []evidence.Verdict{
		{Perspective: string(review.PerspectiveCorrectness), Backend: "b", Model: "m", Verdict: "approve", RecordedAt: now},
		{Perspective: string(review.PerspectiveSecurity), Backend: "b", Model: "m", Verdict: "approve", RecordedAt: now},
		{Perspective: string(review.PerspectiveStyle), Backend: "b", Model: "m", Verdict: "changes_requested", RecordedAt: now},
	}}
	if got := count(bundle, agg, dispatch); got.PerspectivesDone != 3 || got.PerspectivesTotal != len(expected) {
		t.Fatalf("partial counts = %d/%d (%s), want 3/%d", got.PerspectivesDone, got.PerspectivesTotal, got.State, len(expected))
	}
	// A perspective outside the configured set that was dispatched or reported
	// for this head widens the total rather than vanishing.
	dispatch.Pending = append(dispatch.Pending, review.PendingReview{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Perspective: "plan-match", Agent: "reviewer-2", Dispatched: now})
	if got := count(bundle, agg, dispatch); got.PerspectivesTotal != len(expected)+1 || got.PerspectivesDone != 3 {
		t.Fatalf("extra dispatched perspective counts = %d/%d, want 3/%d", got.PerspectivesDone, got.PerspectivesTotal, len(expected)+1)
	}
	// hive#16539: five verdicts exist for the head but the swarm is queued
	// again (a wait, no reviewer free): this round has not started, so the
	// word is "Review 0/5 · swarm requested", never "5/5 · swarm requested".
	full := &review.Aggregate{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Verdict: "approve",
		Perspectives: map[review.Perspective]review.Verdict{}}
	for _, p := range expected {
		full.Perspectives[p] = "approve"
	}
	waiting := review.DispatchState{GeneratedAt: now, Waits: []review.ReviewWaitRecord{{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Reason: "no-free-reviewer", FirstAt: now.Add(-8 * time.Minute), LastAt: now}}}
	if got := count(bundle, full, waiting); got.State != "unassigned" || got.PerspectivesDone != 0 || got.PerspectivesTotal != len(expected) {
		t.Fatalf("queued swarm counts = %d/%d (%s), want 0/%d unassigned", got.PerspectivesDone, got.PerspectivesTotal, got.State, len(expected))
	}
	// Evidence for another head counts nothing for this one.
	agg.HeadSHA, bundle.HeadSHA = "old-head", "old-head"
	dispatch.Pending = nil
	if got := count(bundle, agg, dispatch); got.PerspectivesDone != 0 || got.PerspectivesTotal != len(expected) {
		t.Fatalf("old-head evidence leaked into counts: %d/%d", got.PerspectivesDone, got.PerspectivesTotal)
	}
}

func TestStageTrackerActivityCIPartialRingIsRunning(t *testing.T) {
	now := time.Date(2026, 11, 4, 18, 0, 0, 0, time.UTC)
	name := func(i int) string { return fmt.Sprintf("check-%02d", i) }
	build := func(passed, pending int) github.PullRequest {
		var required []string
		var bindings []github.PRRequiredCheck
		var checks []github.PRActivityCheck
		for i := 0; i < passed+pending; i++ {
			required = append(required, name(i))
			bindings = append(bindings, github.PRRequiredCheck{Name: name(i), AppKnown: true})
			if i < passed {
				checks = append(checks, github.PRActivityCheck{Name: name(i), State: "completed", Conclusion: "success"})
			} else if i%2 == 0 {
				checks = append(checks, github.PRActivityCheck{Name: name(i), State: "in_progress"})
			}
			// Odd pending checks are not reported at all (a job that only
			// starts after the shards finish), as on hivecommons/hive#16628.
		}
		return github.PullRequest{HeadSHA: "head", CIActivity: &github.PRCIActivity{
			Head: "head", Source: "github-check-runs", ObservedAt: &now, RequiredKnown: true, StatusesKnown: true,
			Required: required, RequiredSource: "protection", RequiredChecks: bindings, Checks: checks}}
	}
	got := stageTrackerCIActivity(build(13, 6), now)
	if got.State != "running" || !got.Active || got.ChecksPassed != 13 || got.ChecksTotal != 19 || got.Caption != "CI running · 6 checks pending" {
		t.Fatalf("13/19 with 6 pending must be an active running track: %+v", got)
	}
	full := stageTrackerCIActivity(build(19, 0), now)
	if full.State != "passing" || full.Active || full.ChecksPassed != 19 || full.ChecksTotal != 19 {
		t.Fatalf("19/19 is a complete green ring: %+v", full)
	}
	// Required set unknown: the head-wide count still reads motion.
	loose := build(13, 6)
	loose.CIActivity.RequiredKnown, loose.CIActivity.Required, loose.CIActivity.RequiredChecks = false, nil, nil
	if got := stageTrackerCIActivity(loose, now); got.State != "running" || !got.Active || got.Caption != "CI running · 3 checks pending" {
		t.Fatalf("head-wide count with checks in progress must run: %+v", got)
	}
}

// hivecommons/hive#16630: a head with checks still reporting is running,
// never "activity unknown" or "no check results observed"; the unreported
// required check is one sentence in Detail, and "no verdict yet" / "no
// check results observed" are reserved for a head with zero check runs.
func TestStageTrackerActivityCIRunningWhileChecksPending(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	t1, t2, t3 := now.Add(-9*time.Minute), now.Add(-6*time.Minute), now.Add(-3*time.Minute)
	name := func(i int) string { return fmt.Sprintf("check-%02d", i) }
	run := func(n string, state, conclusion string, at *time.Time) github.PRActivityCheck {
		return github.PRActivityCheck{Name: n, State: state, Conclusion: conclusion, Since: at}
	}
	// Row A: 11 required checks, 7 green, 3 in progress, "test" has no run yet.
	rowA := func() github.PullRequest {
		var required []github.PRRequiredCheck
		var names []string
		var checks []github.PRActivityCheck
		for i := 0; i < 7; i++ {
			required, names = append(required, github.PRRequiredCheck{Name: name(i), AppKnown: true}), append(names, name(i))
			checks = append(checks, run(name(i), "completed", "success", &t1))
		}
		for i := 7; i < 10; i++ {
			required, names = append(required, github.PRRequiredCheck{Name: name(i), AppKnown: true}), append(names, name(i))
			checks = append(checks, run(name(i), "in_progress", "", &t3))
		}
		required, names = append(required, github.PRRequiredCheck{Name: "test", AppKnown: true}), append(names, "test")
		return github.PullRequest{HeadSHA: "head", CIActivity: &github.PRCIActivity{Head: "head", Source: "github-check-runs", ObservedAt: &t2, StatusesKnown: true,
			RequiredKnown: true, Required: names, RequiredSource: "protection", RequiredChecks: required, Checks: checks}}
	}
	// Row B: required set unknown, 66 check runs observed, 12 green, 54 running.
	rowB := func() github.PullRequest {
		var checks []github.PRActivityCheck
		for i := 0; i < 12; i++ {
			checks = append(checks, run(name(i), "completed", "success", &t1))
		}
		for i := 12; i < 66; i++ {
			checks = append(checks, run(name(i), "in_progress", "", &t3))
		}
		return github.PullRequest{HeadSHA: "head", CIActivity: &github.PRCIActivity{Head: "head", Source: "github-check-runs", ObservedAt: &t2, Checks: checks}}
	}
	twoLeft := rowA()
	twoLeft.CIActivity.Checks = twoLeft.CIActivity.Checks[:7]
	twoLeft.CIActivity.Checks = append(twoLeft.CIActivity.Checks, run(name(7), "completed", "success", &t2), run(name(8), "completed", "success", &t2), run(name(9), "in_progress", "", &t3))
	queuedOnly := rowA()
	for i := 7; i < 10; i++ {
		queuedOnly.CIActivity.Checks[i].State = "queued"
	}
	settledMissing := rowA()
	settledMissing.CIActivity.Checks = settledMissing.CIActivity.Checks[:7]
	empty := rowB()
	empty.CIActivity.Checks = nil
	cases := []struct {
		name             string
		pr               github.PullRequest
		state, caption   string
		passed, total    int
		pending          int
		since            *time.Time
		detailHas        string
		detailLacks      string
		blocker          string
		captionForbidden []string
	}{
		{"row A required: 7/11 with 3 running and test unreported", rowA(), "running", "CI running · 4 checks pending", 7, 11, 4, &t3,
			`Required check 'test' has not reported for this head yet.`, "Branch protection requires", "", []string{"unknown", "waiting", "not reported"}},
		{"row A with two left names them", twoLeft, "running", `CI running · 'check-09', 'test' pending`, 9, 11, 2, &t3,
			`'test' has not reported`, "", "", []string{"unknown"}},
		{"row A with only queued runs is queued, not missing", queuedOnly, "queued", "CI queued · 4 checks pending", 7, 11, 4, &t3,
			`'test' has not reported`, "", "", []string{"unknown", "waiting"}},
		{"row A with every run final keeps the missing verdict", settledMissing, "missing", `CI waiting · required check 'test' hasn't reported for this head yet`, 7, 11, 4, nil,
			`check named 'test'`, "", "test", nil},
		{"row B head-wide: 12/66 with 54 running", rowB(), "running", "CI running · 54 checks pending", 12, 66, 54, &t3,
			"12 of the 66 checks observed on the head have a final result", "no check results observed", "", []string{"unknown", "no verdict", "no check results"}},
		{"row B with zero runs is the only unknown", empty, "unknown", stageTrackerCINoDataCaption("head"), 0, 0, 0, nil,
			"has not yet seen", "", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stageTrackerCIActivity(tc.pr, now)
			if got.State != tc.state || got.Caption != tc.caption {
				t.Fatalf("state/caption = %s %q, want %s %q (%+v)", got.State, got.Caption, tc.state, tc.caption, got)
			}
			if got.ChecksPassed != tc.passed || got.ChecksTotal != tc.total || len(got.ChecksPending) != tc.pending || got.ChecksFailed != 0 {
				t.Fatalf("counts = %d/%d pending %v failed %d, want %d/%d pending %d", got.ChecksPassed, got.ChecksTotal, got.ChecksPending, got.ChecksFailed, tc.passed, tc.total, tc.pending)
			}
			if !strings.Contains(got.Detail, tc.detailHas) || (tc.detailLacks != "" && strings.Contains(got.Detail, tc.detailLacks)) {
				t.Fatalf("detail = %q, want mention of %q and none of %q", got.Detail, tc.detailHas, tc.detailLacks)
			}
			if strings.Count(got.Detail, "test") > 1 && tc.state != "missing" {
				t.Fatalf("detail repeats the unreported check: %q", got.Detail)
			}
			if got.Blocker != tc.blocker {
				t.Fatalf("blocker = %q, want %q", got.Blocker, tc.blocker)
			}
			if (tc.since == nil) != (got.Since == nil) || (tc.since != nil && !got.Since.Equal(*tc.since)) {
				t.Fatalf("since = %v, want %v (latest check-run timestamp on the head)", got.Since, tc.since)
			}
			for _, word := range tc.captionForbidden {
				if strings.Contains(strings.ToLower(got.Caption), word) {
					t.Fatalf("caption %q must not say %q while checks are pending", got.Caption, word)
				}
			}
		})
	}
	// Row B with a red check and runs still pending: motion leads the red
	// neighbour (hivecommons/hive#16631); the caption carries both.
	red := rowB()
	red.CIActivity.Checks[0] = run(name(0), "completed", "failure", &t1)
	if got := stageTrackerCIActivity(red, now); got.State != "running" || got.Caption != "CI running · 54 checks pending · 1 failing" || got.ChecksFailed != 1 || got.Blocker != "" {
		t.Fatalf("red head-wide: %+v", got)
	}
	// Running is active whatever the poll's age, in the head-wide path too
	// (hivecommons/hive#16643): the observation time rides the tooltip.
	for _, pr := range []github.PullRequest{rowB(), func() github.PullRequest { fresh := rowB(); fresh.CIActivity.ObservedAt = &now; return fresh }()} {
		if got := stageTrackerCIActivity(pr, now); !got.Active {
			t.Fatalf("running head-wide should be active: %+v", got)
		}
	}
}

// A requires_human aggregate whose perspectives only asked an approver a
// question (no changes_requested) is a verified head waiting on sign-off,
// not "findings" (hivecommons/hive#16607); one requesting changes is not.
func TestTrackerAggregateSignOffOnly(t *testing.T) {
	signOff := review.Aggregate{Verdict: review.VerdictRequiresHuman, DecisionRequested: true, DecisionQuestion: "@owner confirm?",
		Perspectives: map[review.Perspective]review.Verdict{review.PerspectiveSecurity: review.VerdictRequiresHuman, review.PerspectiveStyle: review.VerdictRequiresHuman}}
	if !trackerAggregateSignOffOnly(signOff) {
		t.Fatalf("sign-off aggregate not recognised: %+v", signOff)
	}
	withFix := signOff
	withFix.Perspectives = map[review.Perspective]review.Verdict{review.PerspectiveSecurity: review.VerdictRequiresHuman, review.PerspectiveCorrectness: review.VerdictChangesRequested}
	noQuestion := signOff
	noQuestion.DecisionRequested, noQuestion.DecisionQuestion = false, ""
	for name, agg := range map[string]review.Aggregate{"changes requested sibling": withFix, "no question": noQuestion} {
		if trackerAggregateSignOffOnly(agg) {
			t.Errorf("%s: wrongly recognised as sign-off only", name)
		}
	}
}

func TestStageTrackerActivityCIExcludesMergeGateContexts(t *testing.T) {
	now := time.Date(2026, 11, 9, 18, 0, 0, 0, time.UTC)
	green := func(name string) github.PRActivityCheck {
		return github.PRActivityCheck{Name: name, State: "completed", Conclusion: "success"}
	}
	build := func(gate string, gateState string, extra ...github.PRActivityCheck) github.PullRequest {
		required := []string{"build", "test", gate}
		bindings := []github.PRRequiredCheck{{Name: "build", AppKnown: true}, {Name: "test", AppKnown: true}, {Name: gate, AppKnown: true}}
		checks := append([]github.PRActivityCheck{green("build"), green("test"), {Name: gate, State: gateState, Kind: "commit_status"}}, extra...)
		return github.PullRequest{HeadSHA: "head", CIActivity: &github.PRCIActivity{
			Head: "head", Source: "github-check-runs", ObservedAt: &now, RequiredKnown: true, Required: required,
			RequiredSource: "protection", RequiredChecks: bindings, Checks: checks, StatusesKnown: true,
		}}
	}
	for _, gate := range []string{"tide", "dco", "DCO"} {
		t.Run(gate+" pending is not a test", func(t *testing.T) {
			pr := build(gate, "pending")
			got := stageTrackerCIActivity(pr, now)
			if got.State != "passing" || got.Active || got.ChecksTotal != 2 || got.ChecksPassed != 2 || len(got.ChecksPending) != 0 {
				t.Fatalf("%s counted toward CI: %+v", gate, got)
			}
			if got.Caption != "CI green · all 2 required checks passed" {
				t.Fatalf("caption = %q", got.Caption)
			}
			// The caller's activity keeps the gate for merge eligibility.
			if len(pr.CIActivity.Required) != 3 || len(pr.CIActivity.Checks) != 3 {
				t.Fatalf("activity mutated: %+v", pr.CIActivity)
			}
		})
	}
	t.Run("real pending check still counts", func(t *testing.T) {
		pr := build("tide", "pending", github.PRActivityCheck{Name: "lint", State: "in_progress"})
		pr.CIActivity.Required = append(pr.CIActivity.Required, "lint")
		pr.CIActivity.RequiredChecks = append(pr.CIActivity.RequiredChecks, github.PRRequiredCheck{Name: "lint", AppKnown: true})
		got := stageTrackerCIActivity(pr, now)
		if got.State != "running" || !got.Active || got.ChecksTotal != 3 || got.ChecksPassed != 2 {
			t.Fatalf("real check dropped: %+v", got)
		}
		if got.Caption != "CI running · 'lint' pending" {
			t.Fatalf("caption = %q", got.Caption)
		}
	})
	t.Run("required set unknown", func(t *testing.T) {
		pr := github.PullRequest{HeadSHA: "head", CIActivity: &github.PRCIActivity{Head: "head", Source: "github-check-runs", ObservedAt: &now,
			Checks: []github.PRActivityCheck{green("build"), {Name: "tide", State: "pending", Kind: "commit_status"}}}}
		got := stageTrackerCIActivity(pr, now)
		if got.State != "unknown" || got.Active || got.ChecksTotal != 1 || got.ChecksPassed != 1 || len(got.ChecksPending) != 0 {
			t.Fatalf("tide counted on head without required set: %+v", got)
		}
	})
}

// hivecommons/promptargs#467 at stage/060-reviewed: branch protection
// requires build-and-test (22), (24) and (26); only (24) and (26) exist on
// the head (both green, with DCO, FOSSA, mutation and friends), Tide is
// pending. The CI chevron read `CI 2/3 · running` for eight hours
// (hivecommons/hive#17508). A required name with no run on the head is a
// settled "missing", never motion, whether or not commit statuses were read;
// the tally counts what GitHub reports.
func TestStageTrackerCIActivityNeverReportedRequiredIsSettled(t *testing.T) {
	observed := time.Date(2026, 11, 9, 18, 24, 40, 0, time.UTC)
	now := observed.Add(8 * time.Hour)
	actions := int64(15368)
	done := func(name string) github.PRActivityCheck {
		return github.PRActivityCheck{Name: name, State: "completed", Conclusion: "success", Head: "045b8e1", AppID: &actions, Since: &observed}
	}
	for _, statusesKnown := range []bool{true, false} {
		pr := github.PullRequest{HeadSHA: "045b8e1", BaseRef: "main", CIActivity: &github.PRCIActivity{
			Head: "045b8e1", Source: "github-check-runs", ObservedAt: &observed, StatusesKnown: statusesKnown,
			RequiredKnown: true, RequiredSource: "protection",
			Required: []string{"build-and-test (22)", "build-and-test (24)", "build-and-test (26)"},
			RequiredChecks: []github.PRRequiredCheck{
				{Name: "build-and-test (22)", AppKnown: true, AppID: &actions},
				{Name: "build-and-test (24)", AppKnown: true, AppID: &actions},
				{Name: "build-and-test (26)", AppKnown: true, AppID: &actions},
			},
			Checks: []github.PRActivityCheck{
				done("build-and-test (24)"), done("build-and-test (26)"), done("DCO"), done("mutation"), done("verify"), done("FOSSA"), done("copilot-automation"),
				{Name: "tide", State: "pending", Kind: "commit_status", Head: "045b8e1", Description: "Not mergeable. Needs lgtm, approved labels."},
			},
		}}
		got := stageTrackerCIActivity(pr, now)
		// Statuses read: missing. Not read: unknown (a status could still
		// provide it) — either way settled, never active.
		want := "missing"
		if !statusesKnown {
			want = "unknown"
		}
		if got.Active || got.State != want || got.Blocker != "build-and-test (22)" {
			t.Fatalf("statusesKnown=%v: never-reported required check = %+v, want settled %s", statusesKnown, got, want)
		}
		if !strings.HasPrefix(got.Caption, "CI waiting · required check 'build-and-test (22)' hasn't reported") {
			t.Fatalf("statusesKnown=%v: caption = %q", statusesKnown, got.Caption)
		}
		if got.ChecksTotal != 3 || got.ChecksPassed != 2 || got.ChecksFailed != 0 || len(got.ChecksPending) != 1 {
			t.Fatalf("statusesKnown=%v: tally = %d/%d failed=%d pending=%v", statusesKnown, got.ChecksPassed, got.ChecksTotal, got.ChecksFailed, got.ChecksPending)
		}
		if strings.Contains(got.Caption, "running") || strings.Contains(got.Caption, "no conclusion") {
			t.Fatalf("statusesKnown=%v: caption claims motion or ambiguity: %q", statusesKnown, got.Caption)
		}
		// Every check GitHub reports is concluded: nothing claims motion,
		// so no stale re-list is owed however old the snapshot.
		if github.CIActivityClaimsMotion(pr.CIActivity) || github.CIActivityStale(pr.CIActivity, now, 10*time.Minute) {
			t.Fatalf("statusesKnown=%v: a concluded head claims motion", statusesKnown)
		}
	}
}

// A snapshot that still says a check runs is re-listed once it is older
// than dashboard.stage_tracker.ci_activity_stale_m; a fresh one, a
// concluded one, or one whose only motion is Tide's pending status is not.
func TestStageTrackerRelistsStaleRunningCI(t *testing.T) {
	now := time.Date(2026, 11, 10, 2, 50, 0, 0, time.UTC)
	running := func(age time.Duration, checks ...github.PRActivityCheck) *github.PRCIActivity {
		at := now.Add(-age)
		return &github.PRCIActivity{Head: "head", Source: "github-check-runs", ObservedAt: &at, Checks: checks}
	}
	inProgress := github.PRActivityCheck{Name: "build-and-test (22)", State: "in_progress"}
	green := github.PRActivityCheck{Name: "build-and-test (24)", State: "completed", Conclusion: "success"}
	tide := github.PRActivityCheck{Name: "tide", State: "pending", Kind: "commit_status"}
	for _, tc := range []struct {
		name string
		a    *github.PRCIActivity
		want bool
	}{
		{"running for 17m", running(17*time.Minute, inProgress, green), true},
		{"queued for 17m", running(17*time.Minute, github.PRActivityCheck{Name: "lint", State: "queued"}), true},
		{"running for 3m", running(3*time.Minute, inProgress), false},
		{"all concluded for 8h", running(8*time.Hour, green), false},
		{"only tide pending for 8h", running(8*time.Hour, green, tide), false},
		{"no observation time", &github.PRCIActivity{Head: "head", Checks: []github.PRActivityCheck{inProgress}}, false},
	} {
		if got := github.CIActivityStale(tc.a, now, 10*time.Minute); got != tc.want {
			t.Errorf("%s: stale = %v, want %v", tc.name, got, tc.want)
		}
	}
	if github.CIActivityStale(running(time.Hour, inProgress), now, 0) {
		t.Errorf("a disabled guard (0) relisted")
	}

	s := &Server{deps: &Dependencies{}}
	pr := github.PullRequest{Repo: "hivecommons/promptargs", Number: 467, HeadSHA: "head", CIActivity: running(17*time.Minute, inProgress)}
	if s.stageTrackerRelistStaleCI(pr, now) {
		t.Fatalf("relisted with no GitHub client")
	}
}
