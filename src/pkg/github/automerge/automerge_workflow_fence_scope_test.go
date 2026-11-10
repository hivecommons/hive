package automerge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/intent"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// testReleaseWorkflow is the release-critical workflow the scope tests list
// in auto_merge.human_merge_paths, as knuckle's repos list build.yml.
const testReleaseWorkflow = ".github/workflows/build.yml"

func scopedFencePolicy() ContributorLanePolicy {
	policy := defaultContributorLanePolicy()
	policy.WorkflowFenceScope = config.ContributorLaneWorkflowFenceScopeHumanMergePaths
	return policy
}

// workflow_fence_scope: human_merge_paths fences only the workflow / action
// files the repository lists in human_merge_paths; every other rule stays
// as it was. On v6 tip the fence ignores the list, so every case that
// expects a merge (or tests-removed) instead stops at workflow-path.
func TestContributorLaneWorkflowFenceScope(t *testing.T) {
	file := func(name, status string) map[string]any {
		return map[string]any{"filename": name, "status": status, "additions": 2, "deletions": 1}
	}
	listed := humanPathsFor("acme/widget", testReleaseWorkflow)
	for _, tc := range []struct {
		name       string
		scope      string
		paths      func(string) []string
		files      []map[string]any
		wantReason string
		wantMerges int
	}{
		{
			name:       "scope all keeps the full fence on an unlisted workflow",
			scope:      config.ContributorLaneWorkflowFenceScopeAll,
			paths:      listed,
			files:      []map[string]any{file(".github/workflows/lint.yml", "modified")},
			wantReason: contributorLaneReasonWorkflowPath,
		},
		{
			name:       "unset scope keeps the full fence",
			paths:      listed,
			files:      []map[string]any{file(".github/workflows/lint.yml", "modified")},
			wantReason: contributorLaneReasonWorkflowPath,
		},
		{
			name:       "unknown scope keeps the full fence",
			scope:      "release_only",
			paths:      listed,
			files:      []map[string]any{file(".github/workflows/lint.yml", "modified")},
			wantReason: contributorLaneReasonWorkflowPath,
		},
		{
			name:       "scoped: unlisted workflow merges through review, CI and soak",
			scope:      config.ContributorLaneWorkflowFenceScopeHumanMergePaths,
			paths:      listed,
			files:      []map[string]any{file(".github/workflows/lint.yml", "modified")},
			wantMerges: 1,
		},
		{
			name:       "scoped: unlisted local action merges",
			scope:      config.ContributorLaneWorkflowFenceScopeHumanMergePaths,
			paths:      listed,
			files:      []map[string]any{file(".github/actions/setup/action.yml", "modified")},
			wantMerges: 1,
		},
		{
			name:       "scoped: listed workflow keeps the fence",
			scope:      config.ContributorLaneWorkflowFenceScopeHumanMergePaths,
			paths:      listed,
			files:      []map[string]any{file(".github/workflows/lint.yml", "modified"), file(testReleaseWorkflow, "modified")},
			wantReason: contributorLaneReasonWorkflowPath,
		},
		{
			name:       "scoped: repo without a human_merge_paths entry keeps the fence",
			scope:      config.ContributorLaneWorkflowFenceScopeHumanMergePaths,
			paths:      humanPathsFor("acme/other", testReleaseWorkflow),
			files:      []map[string]any{file(".github/workflows/lint.yml", "modified")},
			wantReason: contributorLaneReasonWorkflowPath,
		},
		{
			name:       "scoped: renamed workflow keeps the fence",
			scope:      config.ContributorLaneWorkflowFenceScopeHumanMergePaths,
			paths:      listed,
			files:      []map[string]any{file(".github/workflows/lint.yml", "renamed")},
			wantReason: contributorLaneReasonWorkflowPath,
		},
		{
			name:       "scoped: tests removed still wins",
			scope:      config.ContributorLaneWorkflowFenceScopeHumanMergePaths,
			paths:      listed,
			files:      []map[string]any{file(".github/workflows/lint.yml", "modified"), {"filename": "pkg/widget/widget_test.go", "status": "removed", "additions": 0, "deletions": 40}},
			wantReason: contributorLaneReasonTestsRemoved,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := validContributorFixture()
			fx.files = tc.files
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := defaultContributorLanePolicy()
			policy.WorkflowFenceScope = tc.scope
			clock := &contributorLaneClock{at: mustParseTime(t, "2026-10-27T13:00:00Z")}
			engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock), HumanMergePaths: tc.paths})
			_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
			if err != nil {
				t.Fatal(err)
			}
			if reason != tc.wantReason || fx.merges != tc.wantMerges {
				t.Fatalf("reason=%q merges=%d, want %q/%d (comments=%v)", reason, fx.merges, tc.wantReason, tc.wantMerges, fx.allComments)
			}
		})
	}
}

// A workflow edit the scope waives still needs the App's workflows: write to
// merge. A refusal for want of it marks the head hive:unauthorized and stops
// at workflow-path, as for an owner-released fence, and neither a second
// tick nor a restart that sees the marker tries the merge again. On v6 tip
// the head never reaches the merge at all (no merge attempt on tick one).
func TestContributorLaneWorkflowFenceScopeMergeUnauthorized(t *testing.T) {
	for _, tc := range []struct {
		name       string
		restart    bool
		wantMerges int
	}{
		{name: "first refusal marks the head", wantMerges: 1},
		{name: "restart sees the marker and does not retry", restart: true, wantMerges: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := validContributorFixture()
			fx.files = []map[string]any{{"filename": ".github/workflows/lint.yml", "status": "modified", "additions": 1, "deletions": 1}}
			fx.mergeStatus = http.StatusForbidden
			fx.mergeMessage = "Resource not accessible by integration"
			if tc.restart {
				fx.labels = []string{unauthorizedLabel}
				fx.prComments = []map[string]any{{
					"id":   5,
					"user": map[string]string{"login": testHiveAppBotLogin},
					"body": unauthorizedComment("sha7", unauthorizedStepMergeWorkflowPath, unauthorizedPermissionWorkflowsWrite, "click", unauthorizedAttempts{Count: 1, Heads: []string{"sha7"}}),
				}}
			}
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := scopedFencePolicy()
			clock := &contributorLaneClock{at: mustParseTime(t, "2026-10-27T13:00:00Z")}
			engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock), HumanMergePaths: humanPathsFor("acme/widget", testReleaseWorkflow)})
			for tick := 1; tick <= 2; tick++ {
				_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
				if err != nil {
					t.Fatalf("tick %d err = %v", tick, err)
				}
				if reason != contributorLaneReasonWorkflowPath {
					t.Fatalf("tick %d reason = %q, want %q", tick, reason, contributorLaneReasonWorkflowPath)
				}
			}
			if fx.merges != tc.wantMerges {
				t.Fatalf("merge attempts = %d, want %d across two ticks", fx.merges, tc.wantMerges)
			}
			if step := engine.unauthorizedStep("acme", "widget", 7, "sha7"); step != unauthorizedStepMergeWorkflowPath {
				t.Fatalf("unauthorizedStep = %q, want %q", step, unauthorizedStepMergeWorkflowPath)
			}
			if !engine.workflowFenceManual("acme", "widget", 7, "sha7") {
				t.Fatal("workflowFenceManual = false after an unauthorized merge, want true")
			}
		})
	}
}

// The held-run guard is an execution guard, not the merge fence: a registry-
// trusted fork PR whose workflow edit the scope lets merge still never has
// its held Actions runs approved by the hive. Scoping that guard with the
// merge fence would approve runs 11 and 12 here.
func TestContributorLaneWorkflowFenceScopeKeepsHeldRunsRefused(t *testing.T) {
	fx := validContributorFixture()
	fx.body = "Part of #42\n\nSome change."
	fx.issueComments = nil
	fx.baseRef = "main"
	fx.requiredContexts = []string{"ci/build", contributorHeldRunsTestContext}
	fx.headRepo = "thatguywob/widget"
	fx.files = []map[string]any{{"filename": ".github/workflows/lint.yml", "status": "modified", "additions": 1, "deletions": 1}}
	fx.workflowRuns = []map[string]any{heldRun(11, fx.headRepo), heldRun(12, fx.headRepo)}
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	policy := scopedFencePolicy()
	policy.RegistryTiersSufficient = true
	policy.ApproveHeldRunsForRegistryTrusted = true
	clock := &contributorLaneClock{at: mustParseTime(t, "2026-10-27T13:00:00Z")}
	engine := newContributorLaneEngine(api.URL, policy, clock, Options{
		ContributorRegistryTier: registryTiers(map[string]string{contributorLaneTestAuthor: "trusted"}),
		ContributorSoak:         soakedTracker(clock),
		HumanMergePaths:         humanPathsFor("acme/widget", testReleaseWorkflow),
	})
	_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
	if err != nil {
		t.Fatal(err)
	}
	if want := requiredCheckMissingPrefix + contributorHeldRunsTestContext; reason != want {
		t.Fatalf("reason = %q, want %q (the scoped fence lets the head reach CI)", reason, want)
	}
	if len(fx.approvedRuns) != 0 || fx.merges != 0 {
		t.Fatalf("approved runs = %v merges = %d, want none: held runs stay a person's call for any workflow edit", fx.approvedRuns, fx.merges)
	}
}

// The helpers on their own: the merge fence is scoped, the execution check
// is not.
func TestContributorLaneMergeFenceHelpers(t *testing.T) {
	e := New(nil, Options{HumanMergePaths: humanPathsFor("acme/widget", testReleaseWorkflow)})
	unlisted := []intent.ChangedFile{{Filename: ".github/workflows/lint.yml", Status: "modified"}}
	listedFiles := []intent.ChangedFile{{Filename: testReleaseWorkflow, Status: "modified"}}
	scoped := scopedFencePolicy()
	all := defaultContributorLanePolicy()
	if got := e.contributorLaneMergeFence(all, "acme", "widget", 7, "sha7", unlisted); got != contributorLaneReasonWorkflowPath {
		t.Fatalf("scope all, unlisted = %q, want workflow-path", got)
	}
	if got := e.contributorLaneMergeFence(scoped, "acme", "widget", 7, "sha7", unlisted); got != "" {
		t.Fatalf("scoped, unlisted = %q, want no fence", got)
	}
	if got := e.contributorLaneMergeFence(scoped, "acme", "widget", 7, "sha7", listedFiles); got != contributorLaneReasonWorkflowPath {
		t.Fatalf("scoped, listed = %q, want workflow-path", got)
	}
	if got := e.contributorLaneMergeFence(scoped, "acme", "gadget", 7, "sha7", unlisted); got != contributorLaneReasonWorkflowPath {
		t.Fatalf("scoped, repo without paths = %q, want workflow-path", got)
	}
	if !contributorLaneTouchesWorkflowFiles(unlisted) || contributorLaneTouchesWorkflowFiles([]intent.ChangedFile{{Filename: "src/x.go"}}) {
		t.Fatal("contributorLaneTouchesWorkflowFiles must report every workflow edit, whatever the scope")
	}
	if e.workflowFenceScopeKey(scoped, "acme", "widget", 7, "sha7") == e.workflowFenceScopeKey(all, "acme", "widget", 7, "sha7") {
		t.Fatal("the stage cache key must change with the scope")
	}
}

// scopedStageFence turns on the scoped fence with build.yml listed for
// acme/widget, on the stage engine the 085 notes come from.
func scopedStageFence(release bool) func(*Engine) {
	return func(e *Engine) {
		e.contributorLanePolicy = func() ContributorLanePolicy {
			return ContributorLanePolicy{Enabled: true, RequireTiers: map[string]bool{"trusted": true}, OwnerApprovalReleasesWorkflowFence: release, WorkflowFenceScope: config.ContributorLaneWorkflowFenceScopeHumanMergePaths}
		}
		e.SetHumanMergePaths(humanPathsFor("acme/widget", testReleaseWorkflow))
	}
}

// The stage engine's 085 note names the fence the lane applies. Behind a
// sentinel alert (which the lane reaches before its diff fence) an unlisted
// workflow edit is not called a manual merge or an approval-gated fence,
// and a listed one still is. The sentinel is a gate riding the head's real
// stage (hivecommons/hive#17327): with no fence the requires_human verdict's
// concrete remediation is the author's to make (071) and no 085 note is
// posted, so nothing can call it a manual merge.
func TestWorkflowFenceScopeStageNoteAgreesWithLane(t *testing.T) {
	t.Run("unlisted workflow", func(t *testing.T) {
		_, closeAPI, sweep := manualMergeSweepTo(t, stagelabels.NeedsAuthor, scopedStageFence(false), []string{".github/workflows/lint.yml"}, requiresHumanVerdict,
			contributorLaneReasonSentinelBlocked, "role/trusted", "author/clanker", "stage/050-review-pending", "sentinel-alert")
		defer closeAPI()
		if comments := sweep(); len(comments) != 0 {
			t.Fatalf("a workflow edit outside the fence scope must not be parked at 085: %q", comments)
		}
	})
	t.Run("listed workflow", func(t *testing.T) {
		_, closeAPI, sweep := manualMergeSweepWith(t, scopedStageFence(false), []string{testReleaseWorkflow}, requiresHumanVerdict,
			contributorLaneReasonSentinelBlocked, "role/trusted", "author/clanker", "stage/050-review-pending", "sentinel-alert")
		defer closeAPI()
		comments := sweep()
		if len(comments) != 1 || !strings.Contains(comments[0], "edits CI workflow / action files") ||
			!strings.Contains(comments[0], "a maintainer must review and merge this PR manually") {
			t.Fatalf("085 comment = %q", comments)
		}
	})
}

// `/hive approve` reads the same scoped fence as the lane: an unlisted
// workflow edit gets the ordinary acknowledgement (the lane merges it
// after review), a listed one is still a person's merge, and scope all
// keeps today's owner-approval wording. On v6 tip the unlisted case gets
// the owner-approval wording instead.
func TestPRApproveReplyWorkflowFenceScope(t *testing.T) {
	for _, tc := range []struct {
		name, file, scope, want string
	}{
		{name: "scoped unlisted workflow", file: ".github/workflows/lint.yml", scope: config.ContributorLaneWorkflowFenceScopeHumanMergePaths, want: "`/hive approve` acknowledged: "},
		{name: "scoped listed workflow", file: testReleaseWorkflow, scope: config.ContributorLaneWorkflowFenceScopeHumanMergePaths, want: "requires a person to merge manually"},
		{name: "scope all unlisted workflow", file: ".github/workflows/lint.yml", scope: config.ContributorLaneWorkflowFenceScopeAll, want: "merges only on a hive owner's explicit approval"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("feedback must not mutate: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusForbidden)
					return
				}
				switch r.URL.Path {
				case "/repos/acme/widget/pulls/7":
					json.NewEncoder(w).Encode(&gh.PullRequest{Number: gh.Ptr(7), State: gh.Ptr("open"), User: &gh.User{Login: gh.Ptr("contributor")}, Head: &gh.PullRequestBranch{SHA: gh.Ptr("sha7")}})
				case "/repos/acme/widget/pulls/7/files":
					json.NewEncoder(w).Encode([]*gh.CommitFile{{Filename: gh.Ptr(tc.file), Status: gh.Ptr("modified")}})
				default:
					t.Errorf("unexpected read: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer api.Close()
			client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, api.URL)
			client.SetAppBotLogin(testHiveAppBotLogin)
			e := New(client, Options{
				MergerAuthorizer:    func(login string) bool { return login == "owner" },
				TrustedAuthorPolicy: func() TrustedAuthorPolicy { return TrustedAuthorPolicy{} },
				TrustedAuthorizer:   func(string, string) TrustedAuthorDecision { return TrustedAuthorDecision{} },
				ContributorLanePolicy: func() ContributorLanePolicy {
					return ContributorLanePolicy{Enabled: true, OwnerApprovalReleasesWorkflowFence: true, WorkflowFenceScope: tc.scope}
				},
				ContributorLaneVerdict: func(string, int, string) ContributorLaneVerdict {
					return ContributorLaneVerdict{Configured: true, Verdict: "requires_human"}
				},
				HumanMergePaths:      humanPathsFor("acme/widget", testReleaseWorkflow),
				RepoAutoMergeEnabled: func(string) bool { return true },
			})
			reply, err := e.PRApproveReply(context.Background(), "acme/widget", 7, &gh.IssueComment{Body: gh.Ptr("/hive approve"), User: &gh.User{Login: gh.Ptr("owner"), Type: gh.Ptr("User")}})
			if err != nil || !strings.Contains(reply, tc.want) {
				t.Fatalf("reply=%q err=%v, want %q", reply, err, tc.want)
			}
		})
	}
}

// project.repo_policies[].workflow_fence_scope overrides the hive-wide
// scope for one repository, either way, and a repository without an override
// keeps the hive-wide value. The lane resolves it through the same config
// resolver the hive wires in (config.RepoWorkflowFenceScope). On v6 tip the
// policy has no per-repo resolver, so the "override loosens" case stops at
// workflow-path and the "override tightens" case merges.
func TestContributorLaneWorkflowFenceScopeRepoOverride(t *testing.T) {
	lintEdit := []map[string]any{{"filename": ".github/workflows/lint.yml", "status": "modified", "additions": 2, "deletions": 1}}
	for _, tc := range []struct {
		name       string
		hiveWide   string
		policies   []config.RepoPolicy
		paths      func(string) []string
		wantReason string
		wantMerges int
	}{
		{
			name:       "override loosens: hive-wide all, widget human_merge_paths",
			hiveWide:   config.ContributorLaneWorkflowFenceScopeAll,
			policies:   []config.RepoPolicy{{Repo: "widget", WorkflowFenceScope: config.ContributorLaneWorkflowFenceScopeHumanMergePaths}},
			paths:      humanPathsFor("acme/widget", testReleaseWorkflow),
			wantMerges: 1,
		},
		{
			name:       "override tightens: hive-wide human_merge_paths, widget all",
			hiveWide:   config.ContributorLaneWorkflowFenceScopeHumanMergePaths,
			policies:   []config.RepoPolicy{{Repo: "acme/widget", WorkflowFenceScope: config.ContributorLaneWorkflowFenceScopeAll}},
			paths:      humanPathsFor("acme/widget", testReleaseWorkflow),
			wantReason: contributorLaneReasonWorkflowPath,
		},
		{
			name:       "another repo's override leaves widget on the hive-wide scope",
			hiveWide:   config.ContributorLaneWorkflowFenceScopeAll,
			policies:   []config.RepoPolicy{{Repo: "other", WorkflowFenceScope: config.ContributorLaneWorkflowFenceScopeHumanMergePaths}},
			paths:      humanPathsFor("acme/widget", testReleaseWorkflow),
			wantReason: contributorLaneReasonWorkflowPath,
		},
		{
			name:       "unknown override falls back to the hive-wide scope",
			hiveWide:   config.ContributorLaneWorkflowFenceScopeAll,
			policies:   []config.RepoPolicy{{Repo: "widget", WorkflowFenceScope: "release_only"}},
			paths:      humanPathsFor("acme/widget", testReleaseWorkflow),
			wantReason: contributorLaneReasonWorkflowPath,
		},
		{
			name:       "override to human_merge_paths keeps the full fence without human_merge_paths",
			hiveWide:   config.ContributorLaneWorkflowFenceScopeAll,
			policies:   []config.RepoPolicy{{Repo: "widget", WorkflowFenceScope: config.ContributorLaneWorkflowFenceScopeHumanMergePaths}},
			paths:      humanPathsFor("acme/other", testReleaseWorkflow),
			wantReason: contributorLaneReasonWorkflowPath,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Project.Org = "acme"
			cfg.Project.RepoPolicies = tc.policies
			cfg.AutoMerge.ContributorLane.WorkflowFenceScope = tc.hiveWide
			fx := validContributorFixture()
			fx.files = lintEdit
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := defaultContributorLanePolicy()
			policy.WorkflowFenceScope = cfg.AutoMerge.ContributorLane.EffectiveWorkflowFenceScope()
			policy.WorkflowFenceScopeForRepo = func(repo string) string {
				scope, _ := cfg.RepoWorkflowFenceScope(repo)
				return scope
			}
			clock := &contributorLaneClock{at: mustParseTime(t, "2026-10-27T13:00:00Z")}
			engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock), HumanMergePaths: tc.paths})
			_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
			if err != nil {
				t.Fatal(err)
			}
			if reason != tc.wantReason || fx.merges != tc.wantMerges {
				t.Fatalf("reason=%q merges=%d, want %q/%d (comments=%v)", reason, fx.merges, tc.wantReason, tc.wantMerges, fx.allComments)
			}
		})
	}
}
