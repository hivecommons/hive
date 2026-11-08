package automerge

// The contributor lane's held-run approvals across sweeps. Production runs
// SweepContributorLaneAutoMerges, which builds a new Engine every tick
// (New(transport, opts)), so these tests build one engine per tick on one
// shared transport, as the hive does. projectbluefin, 2026-11-08 00:40-01:44Z:
// dakota-iso#307 and fsdk-containers#508 each had 3 held fork runs and the
// App lacks `actions: write`; every sweep re-POSTed all 6 and got 403.

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// contributorLaneTickEngines returns a constructor for one engine per
// sweep tick, all sharing one transport and one set of trackers.
func contributorLaneTickEngines(apiURL string, policy ContributorLanePolicy, clock *contributorLaneClock, opts Options) func() *Engine {
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, apiURL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	opts.ContributorLanePolicy = func() ContributorLanePolicy { return policy }
	opts.BasePreparation = NewBasePreparationStore("")
	opts.ContributorLaneVerdict = approvedVerdict
	if opts.ContributorSoak == nil {
		opts.ContributorSoak = soakedTracker(clock)
	}
	opts.HeadFirstSeen = NewHeadFirstSeenTracker()
	opts.StageStore = stagelabels.NewStore("", nil)
	opts.Now = clock.now
	opts.SentinelLabel = func() string { return "hive-sentinel" }
	return func() *Engine { return New(client, opts) }
}

func contributorHeldRunsLanePolicy() ContributorLanePolicy {
	policy := defaultContributorLanePolicy()
	policy.RegistryTiersSufficient = true
	policy.ApproveHeldRunsForRegistryTrusted = true
	return policy
}

func contributorHeldRunsForkFixture() contributorLaneFixture {
	fx := validContributorFixture()
	fx.baseRef = "main"
	fx.body = "Part of #42\n\nSome change."
	fx.issueComments = nil
	fx.requiredContexts = []string{"ci/build", contributorHeldRunsTestContext}
	fx.headRepo = contributorHeldRunsTestFork
	fx.workflowRuns = []map[string]any{heldRun(11, contributorHeldRunsTestFork), heldRun(12, contributorHeldRunsTestFork)}
	return fx
}

// A refused approval is not re-POSTed by the next sweep's engine; the PR
// keeps reporting the blocker, with one note and one label.
func TestContributorLaneRefusedHeldRunsNotRetriedAcrossSweeps(t *testing.T) {
	fx := contributorHeldRunsForkFixture()
	fx.approveStatus = http.StatusForbidden
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	policy := contributorHeldRunsLanePolicy()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 8, 1, 0, 0, 0, time.UTC)}
	tick := contributorLaneTickEngines(api.URL, policy, clock, Options{ContributorRegistryTier: registryTiers(map[string]string{contributorLaneTestAuthor: "trusted"})})

	wantReason := unauthorizedReason(unauthorizedStepApproveWorkflowRuns)
	for i := 1; i <= 4; i++ {
		_, reason, err := tick().trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
		if err != nil {
			t.Fatalf("sweep %d err = %v", i, err)
		}
		if reason != wantReason {
			t.Fatalf("sweep %d reason = %q, want %q", i, reason, wantReason)
		}
	}
	if !slices.Equal(fx.approvedRuns, []int64{11, 12}) {
		t.Fatalf("approve POSTs = %v, want [11 12] once across 4 sweeps", fx.approvedRuns)
	}
	if fx.comments != 1 || len(fx.labelsAdded) != 1 {
		t.Fatalf("comments = %d labels added = %v, want one unauthorized note and one label", fx.comments, fx.labelsAdded)
	}
	if !slices.Contains(fx.labels, unauthorizedLabel) || len(fx.labelsRemoved) != 0 {
		t.Fatalf("labels = %v removed = %v, want %q kept while the runs are held", fx.labels, fx.labelsRemoved, unauthorizedLabel)
	}
}

// A refused head is tried again when the App gains `actions: write`, and
// any new head gets its own pass; a fork pull_request run on a new head is
// approved once GitHub accepts it.
func TestContributorLaneRefusedHeldRunsRetryOnGrantOrNewHead(t *testing.T) {
	fx := contributorHeldRunsForkFixture()
	fx.approveStatus = http.StatusForbidden
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	policy := contributorHeldRunsLanePolicy()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 8, 1, 0, 0, 0, time.UTC)}
	grant := "read"
	tick := contributorLaneTickEngines(api.URL, policy, clock, Options{
		ContributorRegistryTier: registryTiers(map[string]string{contributorLaneTestAuthor: "trusted"}),
		InstallationPermission: func(_ context.Context, name string) (string, bool) {
			if name != "actions" {
				t.Fatalf("permission read = %q, want actions", name)
			}
			return grant, true
		},
	})
	pr := &gh.PullRequest{
		Number: gh.Ptr(7),
		User:   &gh.User{Login: gh.Ptr(contributorLaneTestAuthor)},
		Head:   &gh.PullRequestBranch{Repo: &gh.Repository{FullName: gh.Ptr(contributorHeldRunsTestFork)}},
	}
	claim := contributorClaimMatch{RegistryTier: "trusted"}
	missing := requiredCheckMissingPrefix + contributorHeldRunsTestContext
	unauthorized := unauthorizedReason(unauthorizedStepApproveWorkflowRuns)
	pass := func(step, head string, wantPOSTs int, wantReason string) []int64 {
		t.Helper()
		approved, reason := tick().approveHeldRunsForRegistryTrusted(context.Background(), "widget", "acme", "widget", pr, 7, head, claim, policy, nil, missing)
		if reason != wantReason {
			t.Fatalf("%s: reason = %q, want %q", step, reason, wantReason)
		}
		if len(fx.approvedRuns) != wantPOSTs {
			t.Fatalf("%s: approve POSTs = %v, want %d in total", step, fx.approvedRuns, wantPOSTs)
		}
		return approved
	}

	pass("first refusal", "sha7", 2, unauthorized)
	pass("same head, same grant", "sha7", 2, unauthorized)
	grant = "write"
	pass("grant gained", "sha7", 4, unauthorized)
	pass("refused again holding write", "sha7", 4, unauthorized)
	pass("new head", "sha8", 6, unauthorized)
	pass("new head, no retry", "sha8", 6, unauthorized)

	fx.approveStatus = 0
	if approved := pass("another head GitHub accepts", "sha9", 8, missing); !slices.Equal(approved, []int64{11, 12}) {
		t.Fatalf("approved = %v, want [11 12]", approved)
	}
	pass("accepted head, no second pass", "sha9", 8, missing)
}

// Only runs GitHub's approve endpoint accepts are sent: a fork
// pull_request run or one the Actions bot queued. A pull_request_review
// run (projectbluefin/dakota-iso run 42341060495) or a pull_request_target
// run on the same fork head is never approved, even when the listing
// returns it.
func TestContributorLaneApprovesOnlyForkPullRequestHeldRuns(t *testing.T) {
	fx := contributorHeldRunsForkFixture()
	withEvent := func(run map[string]any, event string) map[string]any {
		run["event"] = event
		return run
	}
	actionsBot := heldRun(24, contributorHeldRunsTestFork)
	actionsBot["event"] = "workflow_run"
	actionsBot["triggering_actor"] = map[string]any{"login": githubActionsBotLogin}
	fx.workflowRuns = []map[string]any{
		heldRun(11, contributorHeldRunsTestFork),
		withEvent(heldRun(21, contributorHeldRunsTestFork), "pull_request_review"),
		withEvent(heldRun(22, contributorHeldRunsTestFork), "pull_request_target"),
		actionsBot,
	}
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	policy := contributorHeldRunsLanePolicy()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 8, 1, 0, 0, 0, time.UTC)}
	tick := contributorLaneTickEngines(api.URL, policy, clock, Options{ContributorRegistryTier: registryTiers(map[string]string{contributorLaneTestAuthor: "trusted"})})

	for i := 1; i <= 2; i++ {
		if _, _, err := tick().trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy); err != nil {
			t.Fatalf("sweep %d err = %v", i, err)
		}
	}
	if !slices.Equal(fx.approvedRuns, []int64{11, 24}) {
		t.Fatalf("approve POSTs = %v, want [11 24]: the fork pull_request run and the Actions-bot run, once", fx.approvedRuns)
	}
	if !strings.Contains(fx.lastComment, "runs=11,24") {
		t.Fatalf("audit comment = %q, want it to name runs 11 and 24", fx.lastComment)
	}
}

func TestApprovableHeldRun(t *testing.T) {
	run := func(event, headRepo, actor string) *gh.WorkflowRun {
		r := &gh.WorkflowRun{ID: gh.Ptr(int64(1)), Event: gh.Ptr(event), HeadRepository: &gh.Repository{FullName: gh.Ptr(headRepo)}}
		if actor != "" {
			r.TriggeringActor = &gh.User{Login: gh.Ptr(actor)}
		}
		return r
	}
	for _, tc := range []struct {
		name string
		run  *gh.WorkflowRun
		want bool
	}{
		{"fork pull_request", run("pull_request", "kyle/widget", "kyle"), true},
		{"fork pull_request, owner case differs", run("pull_request", "Kyle/Widget", ""), true},
		{"same-repo pull_request", run("pull_request", "Acme/Widget", "kyle"), false},
		{"pull_request_review on the base (dakota-iso run 42341060495)", run("pull_request_review", "acme/widget", "hivecommons-hive[bot]"), false},
		{"pull_request_review on the fork", run("pull_request_review", "kyle/widget", "kyle"), false},
		{"pull_request_target", run("pull_request_target", "kyle/widget", "kyle"), false},
		{"queued by the Actions bot", run("push", "acme/widget", githubActionsBotLogin), true},
		{"no head repository", run("pull_request", "", "kyle"), false},
		{"nil", nil, false},
		{"no id", &gh.WorkflowRun{Event: gh.Ptr("pull_request"), HeadRepository: &gh.Repository{FullName: gh.Ptr("kyle/widget")}}, false},
	} {
		if got := approvableHeldRun(tc.run, "acme/widget"); got != tc.want {
			t.Errorf("%s: approvableHeldRun = %v, want %v", tc.name, got, tc.want)
		}
	}
}
