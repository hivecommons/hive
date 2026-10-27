package automerge

// Held workflow runs on registry-trusted contributor PRs (#14519). These pin
// that the contributor lane approves `action_required` runs once per head
// only for an author the registry path admitted, and never for the other
// admission paths, labelled PRs, CI-definition diffs or with the toggle off.

import (
	"context"
	"net/http"
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
			name:         "approve forbidden logs and posts no comment",
			registry:     registryTrusted,
			mutate:       func(fx *contributorLaneFixture) { noMarker(fx); fx.approveStatus = http.StatusForbidden },
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
