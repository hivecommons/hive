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
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

const contributorHeldRunsTestContext = "Audit production dependencies"

// contributorHeldRunsTestFork is the fork a contributor PR's head lives in:
// GitHub approves held runs only for fork pull_request heads.
const contributorHeldRunsTestFork = "kyle/widget"

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
			// GitHub answers 403 "not from a fork pull request" for a
			// same-repo run, on every try: the lane never sends it.
			name:       "registry trusted same-repo head is not approved",
			registry:   registryTrusted,
			mutate:     noMarker,
			wantReason: wantReason,
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
			name:       "blocked label refuses",
			registry:   registryTrusted,
			mutate:     func(fx *contributorLaneFixture) { noMarker(fx); fx.labels = []string{"blocked"} },
			wantReason: autoMergeReasonLabelBlocked,
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
			name:     "approve server error posts no comment and sets no label",
			registry: registryTrusted,
			mutate: func(fx *contributorLaneFixture) {
				noMarker(fx)
				fx.headRepo = contributorHeldRunsTestFork
				fx.approveStatus = http.StatusInternalServerError
			},
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
	fx.headRepo = contributorHeldRunsTestFork
	fx.workflowRuns = []map[string]any{heldRun(11, contributorHeldRunsTestFork)}
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
	wantMarker := unauthorizedMarker(unauthorizedStepApproveWorkflowRuns)
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
			fx.headRepo = contributorHeldRunsTestFork
			fx.workflowRuns = []map[string]any{heldRun(11, contributorHeldRunsTestFork), heldRun(12, contributorHeldRunsTestFork)}
			fx.approveStatus = http.StatusForbidden
			if tc.labelled {
				fx.labels = []string{unauthorizedLabel}
				fx.prComments = []map[string]any{{
					"id":   5,
					"user": map[string]string{"login": testHiveAppBotLogin},
					"body": unauthorizedComment("sha7", unauthorizedStepApproveWorkflowRuns, unauthorizedPermissionActionsWrite, "do it", unauthorizedAttempts{Count: 1, Heads: []string{"sha7"}}),
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
	fx.headRepo = contributorHeldRunsTestFork
	fx.workflowRuns = []map[string]any{heldRun(11, contributorHeldRunsTestFork)}
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
	marker := unauthorizedMarker(unauthorizedStepApproveWorkflowRuns)
	if marker != "<!-- hive:unauthorized step=approve-workflow-runs -->" {
		t.Fatalf("marker = %q", marker)
	}
	action := approveWorkflowRunsHumanAction("acme", "widget", "CI / Mutation (advisory)", []int64{40459320512, 40459320537})
	first := unauthorizedAttempts{Count: 1, Heads: []string{"e3e9f5cabcdef1234567"}}
	body := unauthorizedComment("E3E9F5CABCDEF1234567", unauthorizedStepApproveWorkflowRuns, unauthorizedPermissionActionsWrite, action, first)
	for _, want := range []string{marker, "`actions: write`", "approve workflow runs", "e3e9f5cabcdef", "Approve and run", "40459320512, 40459320537", "CI / Mutation (advisory)", "Actions tab of acme/widget", first.record()} {
		if !strings.Contains(body, want) {
			t.Fatalf("comment lacks %q:\n%s", want, body)
		}
	}
	if !strings.HasPrefix(body, marker+"\n") {
		t.Fatalf("comment must start with the marker line:\n%s", body)
	}
	if strings.Contains(body, "Attempt ") {
		t.Fatalf("first attempt must not count itself:\n%s", body)
	}
	if got := parseUnauthorizedAttempts(body); !reflect.DeepEqual(got, first) {
		t.Fatalf("parse(first) = %+v, want %+v", got, first)
	}
	second := unauthorizedComment("ffff", unauthorizedStepApproveWorkflowRuns, unauthorizedPermissionActionsWrite, action, first.next("FFFF"))
	if !strings.Contains(second, "<sub>Attempt 2: the same block hit heads `e3e9f5cabcde`, `ffff`.</sub>") || !strings.Contains(second, "for head `ffff`") {
		t.Fatalf("second attempt:\n%s", second)
	}
	if got := unauthorizedReason(unauthorizedStepApproveWorkflowRuns); got != "unauthorized:approve-workflow-runs" {
		t.Fatalf("reason = %q", got)
	}
}

// A rulesets-only repository (projectbluefin/testsuite, bluefin-bling): the
// classic protection answer is "Branch not protected", the ruleset lists the
// required checks. The lane must read the ruleset, stop at
// required-check-missing and approve the registry-trusted author's held runs
// rather than sit at ci-unverified forever.
func TestContributorLaneApprovesHeldRunsOnRulesetsOnlyBranch(t *testing.T) {
	fx := validContributorFixture()
	fx.baseRef = "main"
	fx.body = "Part of #42\n\nSome change."
	fx.issueComments = nil
	fx.headRepo = "thatguywob/widget"
	fx.rulesetContexts = []string{"ci/build", contributorHeldRunsTestContext}
	fx.workflowRuns = []map[string]any{heldRun(11, "thatguywob/widget"), heldRun(12, "thatguywob/widget")}
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	policy := defaultContributorLanePolicy()
	policy.RegistryTiersSufficient = true
	policy.ApproveHeldRunsForRegistryTrusted = true
	clock := &contributorLaneClock{at: time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)}
	engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorRegistryTier: registryTiers(map[string]string{contributorLaneTestAuthor: "trusted"}), ContributorSoak: soakedTracker(clock)})

	_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
	if err != nil {
		t.Fatal(err)
	}
	if want := requiredCheckMissingPrefix + contributorHeldRunsTestContext; reason != want {
		t.Fatalf("reason = %q, want %q (ruleset required checks read)", reason, want)
	}
	if !slices.Equal(fx.approvedRuns, []int64{11, 12}) {
		t.Fatalf("approved runs = %v, want [11 12]", fx.approvedRuns)
	}
	if fx.merges != 0 {
		t.Fatalf("merges = %d, want 0", fx.merges)
	}
}

// With no readable required checks a held head reports ci-unverified. The
// approver acts only when every pull_request run on the head is held; a head
// where some run already started is an ordinary evidence gap.
func TestContributorLaneApprovesHeldRunsWhenCIUnverifiedAndAllHeld(t *testing.T) {
	for _, tc := range []struct {
		name         string
		runs         []map[string]any
		registry     map[string]string
		wantApproved []int64
	}{
		{
			name:         "every run held approves",
			runs:         []map[string]any{heldRun(11, contributorHeldRunsTestFork), heldRun(12, contributorHeldRunsTestFork), heldRun(14, "mallory/widget")},
			registry:     map[string]string{contributorLaneTestAuthor: "trusted"},
			wantApproved: []int64{11, 12},
		},
		{
			name: "a run already in progress leaves it alone",
			runs: []map[string]any{
				heldRun(11, contributorHeldRunsTestFork),
				{"id": 13, "name": "lint", "event": "pull_request", "status": "in_progress", "head_repository": map[string]any{"full_name": contributorHeldRunsTestFork}},
			},
			registry: map[string]string{contributorLaneTestAuthor: "trusted"},
		},
		{
			name:     "claim marker author is not approved for",
			runs:     []map[string]any{heldRun(11, contributorHeldRunsTestFork)},
			registry: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := validContributorFixture()
			fx.baseRef = "main"
			if tc.registry != nil {
				fx.body = "Part of #42\n\nSome change."
				fx.issueComments = nil
			}
			fx.noCIEvidence = true
			fx.headRepo = contributorHeldRunsTestFork
			fx.workflowRuns = tc.runs
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := defaultContributorLanePolicy()
			policy.RegistryTiersSufficient = true
			policy.ApproveHeldRunsForRegistryTrusted = true
			clock := &contributorLaneClock{at: time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)}
			engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorRegistryTier: registryTiers(tc.registry), ContributorSoak: soakedTracker(clock)})

			for tick := 1; tick <= 2; tick++ {
				_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
				if err != nil {
					t.Fatalf("tick %d: %v", tick, err)
				}
				if reason != contributorHeldRunsCIUnverified {
					t.Fatalf("tick %d reason = %q, want %q", tick, reason, contributorHeldRunsCIUnverified)
				}
			}
			if !slices.Equal(fx.approvedRuns, tc.wantApproved) {
				t.Fatalf("approved runs = %v, want %v (once across two ticks)", fx.approvedRuns, tc.wantApproved)
			}
			if fx.merges != 0 {
				t.Fatalf("merges = %d, want 0", fx.merges)
			}
		})
	}
}

func TestParseUnauthorizedAttempts(t *testing.T) {
	step := unauthorizedStepRerunSharedCI
	for _, tc := range []struct {
		name  string
		body  string
		want  unauthorizedAttempts
		match bool
	}{
		{
			name:  "current form with record",
			body:  unauthorizedMarker(step) + "\ntext\n" + unauthorizedAttempts{Count: 3, Heads: []string{"aaa", "bbb"}}.record() + "\n",
			want:  unauthorizedAttempts{Count: 3, Heads: []string{"aaa", "bbb"}},
			match: true,
		},
		{
			name:  "legacy per-head marker counts as one attempt on that head",
			body:  unauthorizedLegacyMarker("D61D907", step) + "\nThe hive's GitHub App lacks `actions: write`…",
			want:  unauthorizedAttempts{Count: 1, Heads: []string{"d61d907"}},
			match: true,
		},
		{
			name:  "current marker without record is one unknown attempt",
			body:  unauthorizedMarker(step) + "\ntext",
			want:  unauthorizedAttempts{Count: 1},
			match: true,
		},
		{
			name:  "malformed count falls back to one",
			body:  unauthorizedMarker(step) + "\n" + unauthorizedAttemptsPrefix + "x heads=aaa" + unauthorizedAttemptsSuffix,
			want:  unauthorizedAttempts{Count: 1, Heads: []string{"aaa"}},
			match: true,
		},
		{
			name:  "other step does not match",
			body:  unauthorizedMarker(unauthorizedStepApproveWorkflowRuns) + "\ntext",
			want:  unauthorizedAttempts{Count: 1},
			match: false,
		},
		{
			name:  "legacy other step does not match",
			body:  unauthorizedLegacyMarker("aaa", unauthorizedStepApproveWorkflowRuns) + "\ntext",
			want:  unauthorizedAttempts{Count: 1, Heads: []string{"aaa"}},
			match: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUnauthorizedCommentFor(step)(tc.body); got != tc.match {
				t.Fatalf("match = %v, want %v", got, tc.match)
			}
			if got := parseUnauthorizedAttempts(tc.body); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parse = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestUnauthorizedAttemptsNextCapsHeads(t *testing.T) {
	a := unauthorizedAttempts{Count: 1, Heads: []string{"h1"}}
	for i := 2; i <= 8; i++ {
		a = a.next(fmt.Sprintf("H%d", i))
	}
	if a.Count != 8 || !reflect.DeepEqual(a.Heads, []string{"h4", "h5", "h6", "h7", "h8"}) {
		t.Fatalf("after 8 attempts: %+v", a)
	}
	if !a.covers("H8") || a.covers("h1") {
		t.Fatalf("covers: %+v", a)
	}
}

// TestCommentUnauthorizedUpsertsPerStep pins hivecommons/hive#17066: the
// hive:unauthorized note is one comment per PR and step. The same head is
// a no-op, a new head edits the note with the attempt count, a legacy
// per-head note is adopted rather than joined, and a different step gets
// its own note.
func TestCommentUnauthorizedUpsertsPerStep(t *testing.T) {
	step := unauthorizedStepRerunSharedCI
	for _, tc := range []struct {
		name        string
		existing    []map[string]any
		heads       []string
		wantPosts   int
		wantEdits   int
		wantAttempt int
		wantLast    string
	}{
		{
			name:        "same head twice posts once",
			heads:       []string{"aaa", "aaa"},
			wantPosts:   1,
			wantEdits:   0,
			wantAttempt: 1,
		},
		{
			name:        "three heads post once and edit twice",
			heads:       []string{"aaa", "bbb", "ccc"},
			wantPosts:   1,
			wantEdits:   2,
			wantAttempt: 3,
			wantLast:    "<sub>Attempt 3: the same block hit heads `aaa`, `bbb`, `ccc`.</sub>",
		},
		{
			name: "legacy per-head note is adopted in place",
			existing: []map[string]any{{
				"id":   5,
				"user": map[string]string{"login": testHiveAppBotLogin},
				"body": unauthorizedLegacyMarker("aaa", step) + "\nold text",
			}},
			heads:       []string{"aaa", "bbb"},
			wantPosts:   0,
			wantEdits:   1,
			wantAttempt: 2,
			wantLast:    "<sub>Attempt 2: the same block hit heads `aaa`, `bbb`.</sub>",
		},
		{
			name: "a stranger's marker is not edited",
			existing: []map[string]any{{
				"id":   5,
				"user": map[string]string{"login": "mallory"},
				"body": unauthorizedMarker(step) + "\nforged",
			}},
			heads:       []string{"aaa"},
			wantPosts:   1,
			wantEdits:   0,
			wantAttempt: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := validContributorFixture()
			fx.prComments = tc.existing
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			clock := &contributorLaneClock{at: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
			engine := newContributorLaneEngine(api.URL, defaultContributorLanePolicy(), clock, Options{})
			for _, head := range tc.heads {
				if err := engine.commentUnauthorized(context.Background(), "acme", "widget", 7, head, step, unauthorizedPermissionActionsWrite, "do it"); err != nil {
					t.Fatalf("head %s: %v", head, err)
				}
			}
			if fx.comments != tc.wantPosts || len(fx.commentEdits) != tc.wantEdits {
				t.Fatalf("posts = %d edits = %d, want %d/%d\nedits: %q", fx.comments, len(fx.commentEdits), tc.wantPosts, tc.wantEdits, fx.commentEdits)
			}
			var notes []string
			for _, c := range fx.prComments {
				if body, _ := c["body"].(string); isUnauthorizedCommentFor(step)(body) && c["user"].(map[string]string)["login"] == testHiveAppBotLogin {
					notes = append(notes, body)
				}
			}
			if len(notes) != 1 {
				t.Fatalf("own notes for step = %d, want 1: %q", len(notes), notes)
			}
			if got := parseUnauthorizedAttempts(notes[0]); got.Count != tc.wantAttempt {
				t.Fatalf("attempts = %+v, want count %d", got, tc.wantAttempt)
			}
			if tc.wantLast != "" && !strings.Contains(notes[0], tc.wantLast) {
				t.Fatalf("note lacks %q:\n%s", tc.wantLast, notes[0])
			}
			if !strings.HasPrefix(notes[0], unauthorizedMarker(step)+"\n") {
				t.Fatalf("note not under the current marker:\n%s", notes[0])
			}
			// A different step is its own note.
			if err := engine.commentUnauthorized(context.Background(), "acme", "widget", 7, "aaa", unauthorizedStepApproveWorkflowRuns, unauthorizedPermissionActionsWrite, "click"); err != nil {
				t.Fatal(err)
			}
			if fx.comments != tc.wantPosts+1 {
				t.Fatalf("other step did not post its own note: posts = %d", fx.comments)
			}
		})
	}
}
