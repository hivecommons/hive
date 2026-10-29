package automerge

import (
	"context"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

func humanMergePR(title string, labels ...string) *gh.PullRequest {
	pr := &gh.PullRequest{
		Title: gh.Ptr(title),
		State: gh.Ptr("open"),
		User:  &gh.User{Login: gh.Ptr("alice")},
		Head:  &gh.PullRequestBranch{SHA: gh.Ptr("sha")},
	}
	for _, l := range labels {
		pr.Labels = append(pr.Labels, &gh.Label{Name: gh.Ptr(l)})
	}
	return pr
}

func TestRequiresHumanMergeDefaults(t *testing.T) {
	for _, tc := range []struct {
		name string
		pr   *gh.PullRequest
		want bool
	}{
		{name: "warning sign with VS16", pr: humanMergePR("\u26a0\ufe0f drop the v4 API"), want: true},
		{name: "bare warning sign", pr: humanMergePR("\u26a0 drop the v4 API"), want: true},
		{name: "leading whitespace", pr: humanMergePR("  \t\u26a0\ufe0f drop the v4 API"), want: true},
		{name: "sign inside the title does not count", pr: humanMergePR("fix: handle \u26a0\ufe0f in titles"), want: false},
		{name: "needs-owner-merge label", pr: humanMergePR("\u2728 add a thing", "needs-owner-merge"), want: true},
		{name: "label match is case-insensitive and trimmed", pr: humanMergePR("\u2728 add a thing", " Needs-Owner-Merge "), want: true},
		{name: "other labels", pr: humanMergePR("\u2728 add a thing", "hold", "needs-rebase"), want: false},
		{name: "plain feature", pr: humanMergePR("\u2728 add a thing"), want: false},
		{name: "nil PR", pr: nil, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RequiresHumanMerge(tc.pr); got != tc.want {
				t.Fatalf("RequiresHumanMerge = %v, want %v", got, tc.want)
			}
			engine := New(hgithub.NewClient("token", "acme", []string{"widget"}, nil, ""), Options{})
			if got := engine.RequiresHumanMerge(tc.pr); got != tc.want {
				t.Fatalf("engine.RequiresHumanMerge (default rule) = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRequiresHumanMergeConfiguredRule(t *testing.T) {
	rule := prstage.HumanMergeRule{TitlePrefixes: []string{"BREAKING:"}, Labels: []string{"owner-only"}}
	engine := New(hgithub.NewClient("token", "acme", []string{"widget"}, nil, ""), Options{HumanMergeRule: func() prstage.HumanMergeRule { return rule }})
	if !engine.RequiresHumanMerge(humanMergePR("BREAKING: new wire format")) {
		t.Fatal("configured prefix must match")
	}
	if !engine.RequiresHumanMerge(humanMergePR("feat", "Owner-Only")) {
		t.Fatal("configured label must match case-insensitively")
	}
	// The configured list replaces the defaults; an empty list switches
	// that half off.
	if engine.RequiresHumanMerge(humanMergePR("\u26a0\ufe0f default sign", "needs-owner-merge")) {
		t.Fatal("defaults must not apply once the rule is configured")
	}
	off := prstage.HumanMergeRule{TitlePrefixes: []string{}, Labels: []string{}}
	if ok, _ := off.RequiresHumanMerge("\u26a0\ufe0f x", []string{"needs-owner-merge"}); ok {
		t.Fatal("explicitly empty lists switch the rule off")
	}
	if ok, why := (prstage.HumanMergeRule{}).RequiresHumanMerge("x", []string{"needs-owner-merge"}); !ok || why != "label:needs-owner-merge" {
		t.Fatalf("zero rule = %v %q, want the defaults", ok, why)
	}
}

// Every lane prefilter reports the one shared reason for a PR the owner
// merges, so neither a lane nor the merge train ever admits it.
func TestHumanMergeRequiredSkipsEveryLanePrefilter(t *testing.T) {
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, "")
	client.SetAppBotLogin(testHiveAppBotLogin)
	engine := New(client, Options{
		TrustedAuthorizer: func(login, requireRole string) TrustedAuthorDecision {
			return TrustedAuthorDecision{Allowed: true, Role: requireRole}
		},
	})
	breaking := humanMergePR("\u26a0\ufe0f remove the v4 API")
	flagged := humanMergePR("\u2728 add a thing", "needs-owner-merge")
	for name, pr := range map[string]*gh.PullRequest{"breaking title": breaking, "owner label": flagged} {
		if got := engine.prefilterTrustedAuthorPR(pr, TrustedAuthorPolicy{RequireRole: "merger"}); got != autoMergeReasonHumanMergeRequired {
			t.Fatalf("%s: trusted-author prefilter = %q, want %q", name, got, autoMergeReasonHumanMergeRequired)
		}
		if got := engine.prefilterContributorLanePR(pr, ContributorLanePolicy{}); got != autoMergeReasonHumanMergeRequired {
			t.Fatalf("%s: contributor-lane prefilter = %q, want %q", name, got, autoMergeReasonHumanMergeRequired)
		}
		appPR := *pr
		appPR.User = &gh.User{Login: gh.Ptr(testHiveAppBotLogin)}
		if got := engine.prefilterSelfAuthoredPR(&appPR); got != autoMergeReasonHumanMergeRequired {
			t.Fatalf("%s: self-authored prefilter = %q, want %q", name, got, autoMergeReasonHumanMergeRequired)
		}
		issue := &gh.Issue{Title: pr.Title, PullRequestLinks: &gh.PullRequestLinks{URL: gh.Ptr("u")}, Labels: append([]*gh.Label{{Name: gh.Ptr("hive/automerge")}}, pr.Labels...)}
		if got := engine.prefilterQueuedIssue(issue, "hive/automerge"); got != autoMergeReasonHumanMergeRequired {
			t.Fatalf("%s: queued-issue prefilter = %q, want %q", name, got, autoMergeReasonHumanMergeRequired)
		}
		if mergeTrainCandidate(engine.prefilterTrustedAuthorPR(pr, TrustedAuthorPolicy{RequireRole: "merger"})) {
			t.Fatalf("%s: the merge train must not admit a human-merge PR", name)
		}
	}
	if got := engine.prefilterTrustedAuthorPR(humanMergePR("\u2728 add a thing"), TrustedAuthorPolicy{RequireRole: "merger"}); got != "" {
		t.Fatalf("a plain PR must stay eligible, got %q", got)
	}
}

// A green, approved ⚠️ PR by a trusted author is skipped by the trusted-author
// sweep with the human-merge reason and is never merged.
func TestSweepTrustedAuthorSkipsBreakingTitle(t *testing.T) {
	fx := trustedSweepFixture{title: "\u26a0\ufe0f drop the v4 API"}
	api := newTrustedSweepAPI(t, &fx)
	defer api.Close()
	engine := newTrustedSweepEngine(api.URL, TrustedAuthorPolicy{Enabled: true}, Options{})
	var audits []AutoMergeSweepEvent
	result, err := engine.SweepTrustedAuthorAutoMerges(context.Background(), AutoMergeSweepOptions{
		Audit: func(event AutoMergeSweepEvent) { audits = append(audits, event) },
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Seen != 1 || result.Skipped != 1 || len(result.Merged) != 0 || len(audits) != 0 || fx.merges != 0 {
		t.Fatalf("result = %+v audits=%d merges=%d, want the PR seen, skipped and unmerged", result, len(audits), fx.merges)
	}
	if got := engine.prefilterTrustedAuthorPR(humanMergePR(fx.title), TrustedAuthorPolicy{Enabled: true}); got != autoMergeReasonHumanMergeRequired {
		t.Fatalf("skip reason = %q, want %q", got, autoMergeReasonHumanMergeRequired)
	}
	// The same PR with a plain title merges: the title is the only difference.
	fx2 := trustedSweepFixture{title: "\u2728 drop nothing"}
	api2 := newTrustedSweepAPI(t, &fx2)
	defer api2.Close()
	engine2 := newTrustedSweepEngine(api2.URL, TrustedAuthorPolicy{Enabled: true}, Options{})
	result2, err := engine2.SweepTrustedAuthorAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil || len(result2.Merged) != 1 {
		t.Fatalf("plain title: result=%+v err=%v, want one merge", result2, err)
	}
}

// The merge train never groups, admits or fronts a human-merge PR: it is
// left out of the plan even when it overlaps every other member.
func TestPlanMergeTrainsExcludesHumanMergePR(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	api := &mergeTrainAPI{
		files: map[int][]string{20: {"a.go", "b.go"}, 21: {"a.go"}, 22: {"b.go"}},
		state: map[int]string{20: "clean", 21: "clean", 22: "behind"},
	}
	srv := newMergeTrainServer(t, api)
	defer srv.Close()
	c, _ := newMergeTrainEngine(t, srv.URL, DefaultMergeTrainPolicy, nil)
	breaking := mtPR(20, t0, "")
	breaking.Title = gh.Ptr("\u26a0\ufe0f breaking: rename everything")
	breaking.User = &gh.User{Login: gh.Ptr("alice")}
	prs := []*gh.PullRequest{breaking, mtPR(21, t0.Add(time.Minute), ""), mtPR(22, t0.Add(2*time.Minute), "")}
	prefilter := func(pr *gh.PullRequest) string {
		if c.RequiresHumanMerge(pr) {
			return autoMergeReasonHumanMergeRequired
		}
		return ""
	}
	plan := c.planMergeTrains(context.Background(), "trusted", "acme/widget", "acme", "widget", prs, prefilter)
	if plan.isFront(20) || plan.isSibling(20) {
		t.Fatalf("the ⚠️ PR must never be in the train: front=%v sibling=%v", plan.isFront(20), plan.isSibling(20))
	}
	// #21 and #22 share no file, so without #20 there is no train at all;
	// the breaking PR's files bridged them and it must not count.
	if plan.isFront(21) || plan.isSibling(21) || plan.isFront(22) || plan.isSibling(22) {
		t.Fatalf("no train expected without the excluded PR; groups %+v", plan.groups)
	}
}
