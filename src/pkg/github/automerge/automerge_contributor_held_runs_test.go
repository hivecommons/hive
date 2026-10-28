package automerge

// Held workflow runs on registry-trusted contributor PRs (#14519). These pin
// that the contributor lane approves `action_required` runs once per head
// only for an author the registry path admitted, and never for the other
// admission paths, labelled PRs, CI-definition diffs or with the toggle off.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

const contributorHeldRunsTestContext = "Audit production dependencies"

func TestContributorLaneApprovesHeldRunsForRegistryTrusted(t *testing.T) {
	registryTrusted := registryTiers(map[string]string{contributorLaneTestAuthor: "trusted"})
	noMarker := func(fx *contributorLaneFixture) {
		fx.body = "Part of #42\n\nSome change."
		fx.issueComments = nil
	}
	cleanHistory := func(n int) []map[string]any {
		history := make([]map[string]any, 0, n)
		for i := 0; i < n; i++ {
			history = append(history, mergedHistoryPR(contributorLaneTestAuthor, "fix: clean change"))
		}
		return history
	}
	wantReason := requiredCheckMissingPrefix + contributorHeldRunsTestContext
	for _, tc := range []struct {
		name         string
		registry     ContributorRegistryTierFunc
		mutate       func(fx *contributorLaneFixture)
		policy       func(p *ContributorLanePolicy)
		wantReason   string
		wantApproved []int64
		wantComments int
	}{
		{
			name:         "registry trusted fork head approves once",
			registry:     registryTrusted,
			mutate:       func(fx *contributorLaneFixture) { noMarker(fx); fx.headRepo = "thatguywob/widget" },
			wantReason:   wantReason,
			wantApproved: []int64{11, 12},
			wantComments: 1,
		},
		{
			name:         "registry trusted same-repo head approves once",
			registry:     registryTrusted,
			mutate:       noMarker,
			wantReason:   wantReason,
			wantApproved: []int64{11, 12},
			wantComments: 1,
		},
		{
			name:     "auto-promoted author is not approved for",
			registry: registryTiers(nil),
			mutate: func(fx *contributorLaneFixture) {
				fx.issueComments[0]["body"] = contributorClaimMarkerTier(contributorLaneTestAuthor, "contributor", contributorLaneTestHive, " tier=contributor verified=true")
				fx.mergedHistory = cleanHistory(3)
			},
			policy:       func(p *ContributorLanePolicy) { p.AutoPromoteAfterMergedPR = 3 },
			wantReason:   wantReason,
			wantComments: 1, // the auto-promotion audit comment only
		},
		{
			name:       "claim marker author is not approved for",
			registry:   registryTiers(nil),
			wantReason: wantReason,
		},
		{
			name:       "sentinel label refuses",
			registry:   registryTrusted,
			mutate:     func(fx *contributorLaneFixture) { noMarker(fx); fx.labels = []string{"hive-sentinel"} },
			wantReason: contributorLaneReasonSentinelBlocked,
		},
		{
			name:       "exclude label refuses",
			registry:   registryTrusted,
			mutate:     func(fx *contributorLaneFixture) { noMarker(fx); fx.labels = []string{"blocked"} },
			wantReason: "excluded-label:blocked",
		},
		{
			name:     "workflow path refuses",
			registry: registryTrusted,
			mutate: func(fx *contributorLaneFixture) {
				noMarker(fx)
				fx.files = []map[string]any{{"filename": ".github/workflows/ci.yml", "status": "modified", "additions": 1, "deletions": 1}}
			},
			wantReason: contributorLaneReasonWorkflowPath,
		},
		{
			name:       "toggle off does nothing",
			registry:   registryTrusted,
			mutate:     noMarker,
			policy:     func(p *ContributorLanePolicy) { p.ApproveHeldRunsForRegistryTrusted = false },
			wantReason: wantReason,
		},
		{
			name:       "no held runs does nothing",
			registry:   registryTrusted,
			mutate:     func(fx *contributorLaneFixture) { noMarker(fx); fx.workflowRuns = []map[string]any{} },
			wantReason: wantReason,
		},
		{
			// A non-403 approve failure is logged and otherwise changes nothing
			// (the unauthorized path is TestContributorLaneUnauthorizedWhenApproveForbidden).
			name:         "approve server error posts no comment and sets no label",
			registry:     registryTrusted,
			mutate:       func(fx *contributorLaneFixture) { noMarker(fx); fx.approveStatus = http.StatusInternalServerError },
			wantReason:   wantReason,
			wantApproved: []int64{11, 12},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := validContributorFixture()
			fx.baseRef = "main"
			fx.requiredContexts = []string{"ci/build", contributorHeldRunsTestContext}
			headRepo := "acme/widget"
			if tc.mutate != nil {
				tc.mutate(&fx)
			}
			if fx.headRepo != "" {
				headRepo = fx.headRepo
			}
			if fx.workflowRuns == nil {
				fx.workflowRuns = []map[string]any{
					heldRun(11, headRepo),
					heldRun(12, headRepo),
					{"id": 13, "name": "lint", "status": "completed", "conclusion": "success", "head_repository": map[string]any{"full_name": headRepo}},
					heldRun(14, "mallory/widget"),
				}
			}
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := defaultContributorLanePolicy()
			policy.RegistryTiersSufficient = true
			policy.ApproveHeldRunsForRegistryTrusted = true
			if tc.policy != nil {
				tc.policy(&policy)
			}
			clock := &contributorLaneClock{at: time.Date(2026, 10, 27, 13, 0, 0, 0, time.UTC)}
			engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorRegistryTier: tc.registry, ContributorSoak: soakedTracker(clock)})

			for tick := 1; tick <= 2; tick++ {
				_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
				if err != nil {
					t.Fatalf("tick %d err = %v", tick, err)
				}
				if reason != tc.wantReason {
					t.Fatalf("tick %d reason = %q, want %q", tick, reason, tc.wantReason)
				}
			}
			if fx.merges != 0 {
				t.Fatalf("merges = %d, want 0 while a required check is missing", fx.merges)
			}
			if got, want := fx.approvedRuns, tc.wantApproved; len(got) != len(want) {
				t.Fatalf("approved runs = %v, want %v (once across two ticks)", got, want)
			} else {
				for i := range want {
					if got[i] != want[i] {
						t.Fatalf("approved runs = %v, want %v", got, want)
					}
				}
			}
			if fx.comments != tc.wantComments {
				t.Fatalf("comments = %d, want %d last=%q", fx.comments, tc.wantComments, fx.lastComment)
			}
			if len(fx.labelsAdded) != 0 || len(fx.labelsRemoved) != 0 {
				t.Fatalf("label writes added=%v removed=%v, want none", fx.labelsAdded, fx.labelsRemoved)
			}
			if len(tc.wantApproved) > 0 && tc.wantComments > 0 {
				wantMarker := contributorHeldRunsApprovedMarkerPrefix + " head=sha7 author=" + contributorLaneTestAuthor + " tier=trusted runs=11,12 -->"
				if !strings.HasPrefix(fx.lastComment, wantMarker) || !strings.Contains(fx.lastComment, "registry-trusted at tier `trusted`") {
					t.Fatalf("comment = %q, want marker %q", fx.lastComment, wantMarker)
				}
			}
		})
	}
}

func TestContributorLaneHeldRunsApprovalCommentIsIdempotent(t *testing.T) {
	fx := validContributorFixture()
	fx.baseRef = "main"
	fx.body = "Part of #42\n\nSome change."
	fx.issueComments = nil
	fx.requiredContexts = []string{"ci/build", contributorHeldRunsTestContext}
	fx.workflowRuns = []map[string]any{heldRun(11, "acme/widget")}
	fx.prComments = []map[string]any{{
		"id":   5,
		"user": map[string]string{"login": testHiveAppBotLogin},
		"body": contributorHeldRunsApprovedComment("sha7", contributorLaneTestAuthor, "trusted", []int64{11}),
	}}
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	policy := defaultContributorLanePolicy()
	policy.RegistryTiersSufficient = true
	policy.ApproveHeldRunsForRegistryTrusted = true
	clock := &contributorLaneClock{at: time.Date(2026, 10, 27, 13, 0, 0, 0, time.UTC)}
	engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorRegistryTier: registryTiers(map[string]string{contributorLaneTestAuthor: "merger"}), ContributorSoak: soakedTracker(clock)})

	_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
	if err != nil {
		t.Fatal(err)
	}
	if want := requiredCheckMissingPrefix + contributorHeldRunsTestContext; reason != want {
		t.Fatalf("reason = %q, want %q", reason, want)
	}
	if len(fx.approvedRuns) != 1 || fx.approvedRuns[0] != 11 {
		t.Fatalf("approved runs = %v, want [11]", fx.approvedRuns)
	}
	if fx.comments != 0 {
		t.Fatalf("comments = %d, want 0 when the head marker is already on the PR", fx.comments)
	}
}

func TestHeldWorkflowRunsForHeadMatchesForkHead(t *testing.T) {
	run := func(id int64, conclusion, headRepo string) *gh.WorkflowRun {
		r := &gh.WorkflowRun{ID: gh.Ptr(id), Conclusion: gh.Ptr(conclusion)}
		if headRepo != "" {
			r.HeadRepository = &gh.Repository{FullName: gh.Ptr(headRepo)}
		}
		return r
	}
	runs := []*gh.WorkflowRun{
		run(1, "action_required", "thatguywob/widget"),
		run(2, "action_required", "acme/widget"),
		run(3, "action_required", ""),
		run(4, "success", "thatguywob/widget"),
		nil,
	}
	held := heldWorkflowRunsForHead(runs, "ThatGuyWob/Widget")
	if len(held) != 1 || held[0].GetID() != 1 {
		t.Fatalf("held = %v, want only run 1", held)
	}
	if got := heldWorkflowRunsForHead(nil, "acme/widget"); got != nil {
		t.Fatalf("held for nil runs = %v, want nil", got)
	}
}

func TestContributorLaneHeadRepoFallsBackToBase(t *testing.T) {
	if got := contributorLaneHeadRepo(&gh.PullRequest{}, "acme", "widget"); got != "acme/widget" {
		t.Fatalf("head repo = %q, want base fallback", got)
	}
	pr := &gh.PullRequest{Head: &gh.PullRequestBranch{Repo: &gh.Repository{FullName: gh.Ptr("fork/widget")}}}
	if got := contributorLaneHeadRepo(pr, "acme", "widget"); got != "fork/widget" {
		t.Fatalf("head repo = %q, want fork", got)
	}
}

// TestContributorLaneUnauthorizedWhenApproveForbidden pins the
// hive:unauthorized signal (hivecommons/hive#14589): a 403 "Resource not
// accessible by integration" on held-run approval marks the lane
// unauthorized once per head — label + one marker comment + reason
// `unauthorized:approve-workflow-runs` — repeats without a duplicate on the
// next tick, and clears label and state in the tick the required check
// reports.
func TestContributorLaneUnauthorizedWhenApproveForbidden(t *testing.T) {
	wantReason := unauthorizedReasonPrefix + unauthorizedStepApproveWorkflowRuns
	wantMarker := unauthorizedMarker("sha7", unauthorizedStepApproveWorkflowRuns)
	for _, tc := range []struct {
		name string
		// labelled pre-seeds hive:unauthorized and the marker comment on
		// the PR, as a restarted hive sees them.
		labelled bool
	}{
		{name: "fresh head marks once then clears"},
		{name: "restart sees label and marker and re-posts nothing", labelled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := validContributorFixture()
			fx.baseRef = "main"
			fx.body = "Part of #42\n\nSome change."
			fx.issueComments = nil
			fx.requiredContexts = []string{"ci/build", contributorHeldRunsTestContext}
			fx.workflowRuns = []map[string]any{heldRun(11, "acme/widget"), heldRun(12, "acme/widget")}
			fx.approveStatus = http.StatusForbidden
			if tc.labelled {
				fx.labels = []string{unauthorizedLabel}
				fx.prComments = []map[string]any{{
					"id":   5,
					"user": map[string]string{"login": testHiveAppBotLogin},
					"body": unauthorizedComment("sha7", unauthorizedStepApproveWorkflowRuns, unauthorizedPermissionActionsWrite, "do it"),
				}}
			}
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := defaultContributorLanePolicy()
			policy.RegistryTiersSufficient = true
			policy.ApproveHeldRunsForRegistryTrusted = true
			clock := &contributorLaneClock{at: time.Date(2026, 10, 27, 13, 0, 0, 0, time.UTC)}
			engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorRegistryTier: registryTiers(map[string]string{contributorLaneTestAuthor: "trusted"}), ContributorSoak: soakedTracker(clock)})

			for tick := 1; tick <= 2; tick++ {
				_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
				if err != nil {
					t.Fatalf("tick %d err = %v", tick, err)
				}
				if reason != wantReason {
					t.Fatalf("tick %d reason = %q, want %q", tick, reason, wantReason)
				}
			}
			if got := fx.approvedRuns; len(got) != 2 || got[0] != 11 || got[1] != 12 {
				t.Fatalf("approve attempts = %v, want [11 12] once across two ticks", got)
			}
			wantAdds, wantComments := 1, 1
			if tc.labelled {
				wantAdds, wantComments = 0, 0
			}
			if len(fx.labelsAdded) != wantAdds || (wantAdds == 1 && fx.labelsAdded[0] != unauthorizedLabel) {
				t.Fatalf("labels added = %v, want %d × %q", fx.labelsAdded, wantAdds, unauthorizedLabel)
			}
			if fx.labelsCreated > 1 {
				t.Fatalf("label definition created %d times, want at most once", fx.labelsCreated)
			}
			if fx.comments != wantComments {
				t.Fatalf("comments = %d, want %d last=%q", fx.comments, wantComments, fx.lastComment)
			}
			if wantComments == 1 {
				if !strings.HasPrefix(fx.lastComment, wantMarker) || !strings.Contains(fx.lastComment, "`"+unauthorizedPermissionActionsWrite+"`") ||
					!strings.Contains(fx.lastComment, "Approve and run") || !strings.Contains(fx.lastComment, "11, 12") || !strings.Contains(fx.lastComment, contributorHeldRunsTestContext) {
					t.Fatalf("comment = %q, want marker %q naming the permission, the button, the runs and the missing check", fx.lastComment, wantMarker)
				}
			}
			if !slices.Contains(fx.labels, unauthorizedLabel) || len(fx.labelsRemoved) != 0 {
				t.Fatalf("labels = %v removed = %v, want %q still on while the check is missing", fx.labels, fx.labelsRemoved, unauthorizedLabel)
			}
			if step := engine.unauthorizedStep("acme", "widget", 7, "sha7"); step != unauthorizedStepApproveWorkflowRuns {
				t.Fatalf("unauthorizedStep = %q, want %q", step, unauthorizedStepApproveWorkflowRuns)
			}

			// The maintainer clicked Approve and run: the held check reports,
			// the required set is satisfied, and the same tick clears the signal.
			fx.requiredContexts = []string{"ci/build"}
			_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
			if err != nil {
				t.Fatalf("clear tick err = %v", err)
			}
			if strings.HasPrefix(reason, unauthorizedReasonPrefix) {
				t.Fatalf("clear tick reason = %q, want the lane past the unauthorized step", reason)
			}
			if len(fx.labelsRemoved) != 1 || fx.labelsRemoved[0] != unauthorizedLabel || slices.Contains(fx.labels, unauthorizedLabel) {
				t.Fatalf("labels removed = %v labels = %v, want %q removed once", fx.labelsRemoved, fx.labels, unauthorizedLabel)
			}
			if step := engine.unauthorizedStep("acme", "widget", 7, "sha7"); step != "" {
				t.Fatalf("unauthorizedStep after clear = %q, want empty", step)
			}
			// The lane goes on (here: to the merge note); clearing posts no
			// note of its own and never a second unauthorized one.
			for _, body := range fx.allComments {
				if strings.HasPrefix(body, unauthorizedMarkerPrefix) && !strings.HasPrefix(body, wantMarker) {
					t.Fatalf("unexpected unauthorized note after clear: %q", body)
				}
			}
			if n := strings.Count(strings.Join(fx.allComments, "\n"), wantMarker); n != wantComments {
				t.Fatalf("unauthorized notes posted = %d, want %d", n, wantComments)
			}
		})
	}
}

// hivecommons/hive#14668: once the head conflicts, the held runs are no
// longer the blocker (the rebase push brings new ones), so the lane reports
// `conflicting` and takes the stale hive:unauthorized mark off in that tick.
func TestContributorLaneConflictClearsUnauthorized(t *testing.T) {
	fx := validContributorFixture()
	fx.baseRef = "main"
	fx.body = "Part of #42\n\nSome change."
	fx.issueComments = nil
	fx.requiredContexts = []string{"ci/build", contributorHeldRunsTestContext}
	fx.workflowRuns = []map[string]any{heldRun(11, "acme/widget")}
	fx.approveStatus = http.StatusForbidden
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	policy := defaultContributorLanePolicy()
	policy.RegistryTiersSufficient = true
	policy.ApproveHeldRunsForRegistryTrusted = true
	clock := &contributorLaneClock{at: time.Date(2026, 10, 27, 13, 0, 0, 0, time.UTC)}
	engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorRegistryTier: registryTiers(map[string]string{contributorLaneTestAuthor: "trusted"}), ContributorSoak: soakedTracker(clock)})

	if _, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy); err != nil || !strings.HasPrefix(reason, unauthorizedReasonPrefix) {
		t.Fatalf("mark tick reason = %q err = %v, want %s…", reason, err, unauthorizedReasonPrefix)
	}
	if !slices.Contains(fx.labels, unauthorizedLabel) {
		t.Fatalf("labels = %v, want %q after the forbidden approval", fx.labels, unauthorizedLabel)
	}

	fx.mergeableState = "dirty"
	_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
	if err != nil || reason != "conflicting" {
		t.Fatalf("conflict tick reason = %q err = %v, want conflicting", reason, err)
	}
	if len(fx.labelsRemoved) != 1 || fx.labelsRemoved[0] != unauthorizedLabel || slices.Contains(fx.labels, unauthorizedLabel) {
		t.Fatalf("labels removed = %v labels = %v, want %q removed once the head conflicts", fx.labelsRemoved, fx.labels, unauthorizedLabel)
	}
	if step := engine.unauthorizedStep("acme", "widget", 7, "sha7"); step != "" {
		t.Fatalf("unauthorizedStep after conflict = %q, want empty", step)
	}
}

func TestIsResourceNotAccessible(t *testing.T) {
	mk := func(status int, msg string) error {
		return &gh.ErrorResponse{Response: &http.Response{StatusCode: status, Request: &http.Request{Method: "POST", URL: &url.URL{Path: "/x"}}}, Message: msg}
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil},
		{name: "plain error", err: errors.New("Resource not accessible by integration")},
		{name: "403 integration", err: mk(http.StatusForbidden, "Resource not accessible by integration"), want: true},
		{name: "403 integration wrapped", err: fmt.Errorf("approve: %w", mk(http.StatusForbidden, "resource not accessible by integration")), want: true},
		{name: "403 rate limit", err: mk(http.StatusForbidden, "API rate limit exceeded")},
		{name: "404", err: mk(http.StatusNotFound, "Resource not accessible by integration")},
		{name: "500", err: mk(http.StatusInternalServerError, "Resource not accessible by integration")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isResourceNotAccessible(tc.err); got != tc.want {
				t.Fatalf("isResourceNotAccessible = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUnauthorizedCommentAndMarker(t *testing.T) {
	marker := unauthorizedMarker("E3E9F5CABCDEF1234567", unauthorizedStepApproveWorkflowRuns)
	if marker != "<!-- hive:unauthorized head=e3e9f5cabcdef1234567 step=approve-workflow-runs -->" {
		t.Fatalf("marker = %q", marker)
	}
	action := approveWorkflowRunsHumanAction("acme", "widget", "CI / Mutation (advisory)", []int64{40459320512, 40459320537})
	body := unauthorizedComment("e3e9f5cabcdef1234567", unauthorizedStepApproveWorkflowRuns, unauthorizedPermissionActionsWrite, action)
	for _, want := range []string{marker, "`actions: write`", "approve workflow runs", "e3e9f5cabcdef", "Approve and run", "40459320512, 40459320537", "CI / Mutation (advisory)", "Actions tab of acme/widget"} {
		if !strings.Contains(body, want) {
			t.Fatalf("comment lacks %q:\n%s", want, body)
		}
	}
	if !strings.HasPrefix(body, marker+"\n") {
		t.Fatalf("comment must start with the marker line:\n%s", body)
	}
	if got := unauthorizedReason(unauthorizedStepApproveWorkflowRuns); got != "unauthorized:approve-workflow-runs" {
		t.Fatalf("reason = %q", got)
	}
}
