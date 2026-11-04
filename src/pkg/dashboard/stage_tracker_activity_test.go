package dashboard

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"reflect"
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
		{"missing", ci(done("build"), done("lint")), "missing", `CI waiting · required check "test" hasn't reported for this head yet`, `check named "test"`},
		{"running counts", ci(done("build"), chk("lint", "in_progress", ""), chk("test", "in_progress", "")), "running", "CI running · 1/3 checks done", "1 of the 3 required checks"},
		{"queued", ci(done("build"), done("lint"), chk("test", "queued", "")), "queued", `CI queued · "test" waiting for a runner`, "not started"},
		{"failing", ci(done("build"), chk("lint", "completed", "failure"), done("test")), "failing", `CI failing · "lint" red`, `"lint" finished without success`},
		{"skipped", ci(done("build"), done("lint"), chk("test", "completed", "skipped")), "skipped", `CI skipped · check "test" skipped`, "does not count as a pass"},
		{"ambiguous", ci(done("build"), done("lint"), done("test"), chk("test", "completed", "failure")), "unknown", `CI unknown · "test" reported but with no conclusion`, "several runs share the name"},
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
		{"red+running", ci(done("build"), chk("lint", "completed", "failure"), chk("test", "in_progress", "")), "CI running · 2/3 checks done · 1 failing"},
		{"missing+running", ci(done("build"), chk("lint", "in_progress", "")), "CI running · 1/3 checks done"},
	}
	for _, tc := range motion {
		got := stageTrackerCIActivity(tc.pr, now)
		if got.State != "running" || got.Caption != tc.want || !got.Active {
			t.Fatalf("%s: want running %q active, got %+v", tc.name, tc.want, got)
		}
	}
	if got := stageTrackerCIActivity(ci(done("build"), done("lint"), done("lint"), chk("test", "queued", "")), now); got.State != "queued" || got.Caption != `CI queued · "test" waiting for a runner` {
		t.Fatalf("ambiguous+queued: a waiting runner keeps the ring queued, not unknown: %+v", got)
	}
	if got := stageTrackerCIActivity(ci(done("build"), chk("lint", "completed", "failure"), done("test")), now); got.State != "failing" || got.Active {
		t.Fatalf("settled red head must not read as running: %+v", got)
	}
	if got := stageTrackerCIActivity(github.PullRequest{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA}, now); got.Caption != "CI unknown · no check results observed for this head yet" || got.Detail == "" {
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

func TestStageTrackerReviewPerspectiveDetails(t *testing.T) {
	now := time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC)
	observed := now.Add(-15 * time.Minute)
	pr := github.PullRequest{Repo: "org/repo", Number: 7, HeadSHA: "head"}
	bundle := &evidence.Bundle{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, UpdatedAt: observed,
		Verdicts: []evidence.Verdict{
			{Perspective: "security", Verdict: "approve", RecordedAt: observed},
			{Perspective: "correctness", Backend: "old", Verdict: "approve", RecordedAt: observed.Add(-time.Minute)},
			{Perspective: "correctness", Backend: "new", Verdict: "changes_requested", RecordedAt: observed,
				Findings: []evidence.Finding{{Summary: "do not discard the settled verdict"}}},
			{Perspective: "quality", Verdict: "changes_requested", RecordedAt: observed.Add(-time.Minute),
				Findings: []evidence.Finding{{Summary: "superseded finding"}}},
			{Perspective: "quality", Verdict: "approve", RecordedAt: observed},
			{Verdict: "reject", RecordedAt: observed},
		}}
	want := []StageTrackerPerspective{
		{Name: "correctness", Verdict: "changes_requested", Summary: "do not discard the settled verdict"},
		{Name: "quality", Verdict: "approve"},
		{Name: "security", Verdict: "approve"},
	}
	for _, verdict := range []review.Verdict{"changes_requested", "requires_human"} {
		agg := &review.Aggregate{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Verdict: verdict}
		got := stageTrackerReviewActivity(pr, bundle, agg, review.DispatchState{}, nil, now)
		if !reflect.DeepEqual(got.Perspectives, want) || !strings.Contains(got.Caption, "correctness ✗: do not discard the settled verdict") || got.Active {
			t.Fatalf("settled review lost its current-head perspective details: %+v", got)
		}
		data, err := json.Marshal(got)
		if err != nil || !strings.Contains(string(data), `"perspectives":[{"name":"correctness","verdict":"changes_requested","summary":"do not discard the settled verdict"}`) {
			t.Fatalf("perspective wire fields missing: %s, %v", data, err)
		}
	}
	bundle.Verdicts[2].Verdict = "requires_human"
	agg := &review.Aggregate{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Verdict: "requires_human"}
	got := stageTrackerReviewActivity(pr, bundle, agg, review.DispatchState{}, nil, now)
	if got.State != "human_decision" || !strings.Contains(got.Caption, "correctness requires human: do not discard the settled verdict") || got.Perspectives[0].Verdict != "requires_human" {
		t.Fatalf("human perspective was not explained: %+v", got)
	}
	bundle.Verdicts[2].Verdict = "changes_requested"
	// Approval from another backend must not replace still-current dissent.
	bundle.Verdicts = append(bundle.Verdicts, evidence.Verdict{Perspective: "correctness", Backend: "z", Verdict: "approve", RecordedAt: observed.Add(time.Minute)})
	if got := trackerReviewPerspectives(bundle); !reflect.DeepEqual(got, want) {
		t.Fatalf("newer approval from another backend hid a finding: %+v", got)
	}
	for _, other := range []github.PullRequest{
		{Repo: pr.Repo, Number: pr.Number, HeadSHA: "new-head"},
		{Repo: "other/repo", Number: pr.Number, HeadSHA: pr.HeadSHA},
		{Repo: pr.Repo, Number: 8, HeadSHA: pr.HeadSHA},
	} {
		got := stageTrackerReviewActivity(other, bundle, nil, review.DispatchState{}, nil, now)
		if got.State != "unknown" || len(got.Perspectives) != 0 || strings.Contains(got.Caption, "correctness") {
			t.Fatalf("unrelated review details leaked: %+v", got)
		}
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
	if got.State != "running" || !got.Active || got.ChecksPassed != 13 || got.ChecksTotal != 19 || got.Caption != "CI running · 13/19 checks done" {
		t.Fatalf("13/19 with 6 pending must be an active running track: %+v", got)
	}
	full := stageTrackerCIActivity(build(19, 0), now)
	if full.State != "passing" || full.Active || full.ChecksPassed != 19 || full.ChecksTotal != 19 {
		t.Fatalf("19/19 is a complete green ring: %+v", full)
	}
	// Required set unknown: the head-wide count still reads motion.
	loose := build(13, 6)
	loose.CIActivity.RequiredKnown, loose.CIActivity.Required, loose.CIActivity.RequiredChecks = false, nil, nil
	if got := stageTrackerCIActivity(loose, now); got.State != "running" || !got.Active || got.Caption != "CI running · 13/16 checks done" {
		t.Fatalf("head-wide count with checks in progress must run: %+v", got)
	}
}
