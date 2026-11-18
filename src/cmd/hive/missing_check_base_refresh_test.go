package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

type fakeMissingCheckRefresher struct {
	bases     map[string]github.StaleRedBaseState
	baseReads []string
	calls     []github.PullRequest
	checks    []string
	out       github.StaleRedRefreshOutcome
	err       error
}

func (f *fakeMissingCheckRefresher) StaleRedBaseStateFor(_ context.Context, repo, baseRef string) (github.StaleRedBaseState, bool, error) {
	f.baseReads = append(f.baseReads, repo+"@"+baseRef)
	st, ok := f.bases[repo+"@"+baseRef]
	return st, ok, nil
}

func (f *fakeMissingCheckRefresher) RefreshMissingCheckPR(_ context.Context, pr github.PullRequest, _ github.StaleRedBaseState, check string) (github.StaleRedRefreshOutcome, error) {
	f.calls = append(f.calls, pr)
	f.checks = append(f.checks, check)
	return f.out, f.err
}

// resetMissingCheckRefresh gives each test a fresh pass clock and memos.
func resetMissingCheckRefresh(t *testing.T) {
	t.Helper()
	prev := missingCheckRefresh
	missingCheckRefresh = &missingCheckRefreshState{}
	t.Cleanup(func() { missingCheckRefresh = prev })
}

// missingCheckPR is projectbluefin/testsuite#1245 in miniature: a
// contributor's fork PR (maintainers may edit), every check it has green,
// and `dashboard-build`, made required after it opened, never reported.
func missingCheckPR(number int, head string) github.PullRequest {
	return github.PullRequest{
		Repo:                "widgets",
		Number:              number,
		Author:              "contributor",
		HeadSHA:             head,
		HeadRef:             "feature",
		BaseRef:             "main",
		CIStatus:            "success",
		FromFork:            true,
		MaintainerCanModify: true,
		MergeableState:      "blocked",
		Protection: &github.ProtectionFacts{
			RequiredChecksKnown:   true,
			MissingRequiredChecks: []string{"dashboard-build"},
		},
		CIActivity: &github.PRCIActivity{Head: head, Checks: []github.PRActivityCheck{
			{Name: "pytest", State: "completed", Conclusion: "success", Head: head},
		}},
	}
}

// baseRunning names the checks with a run on the base tip.
func baseRunning(sha string, checks ...string) map[string]github.StaleRedBaseState {
	reported := map[string]bool{}
	for _, c := range checks {
		reported[c] = true
	}
	return map[string]github.StaleRedBaseState{"acme/widgets@main": {
		SHA: sha, Completed: map[string]bool{}, Red: map[string]bool{}, Reported: reported,
	}}
}

// The base tip runs the missing check: the hive merges the base in once for
// that (head, base tip) and audits it. A second pass on the same head and tip
// makes no call. After that one update, the same check missing again on a
// later head and base tip is not chased: the new head did not start it, so
// another merge would not either (a path filter, or #11877).
func TestRefreshMissingCheckBehindPRs_OncePerHeadAndBaseTip(t *testing.T) {
	resetMissingCheckRefresh(t)
	cfg := escalationTestConfig()
	actionable := actionableWith(missingCheckPR(1245, "head1"))
	f := &fakeMissingCheckRefresher{bases: baseRunning("base1", "dashboard-build"), out: github.StaleRedRefreshOutcome{Updated: true, Settled: true, BehindBy: 5}}
	var audited []string
	audit := func(repo string, number int, detail string) { audited = append(audited, detail) }
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

	got := refreshMissingCheckBehindPRs(context.Background(), cfg, f, actionable, audit, discardLogger(), now)
	if len(f.calls) != 1 || f.calls[0].Repo != "acme/widgets" || f.checks[0] != "dashboard-build" {
		t.Fatalf("refresh calls = %+v (checks %v), want one for acme/widgets#1245 on dashboard-build", f.calls, f.checks)
	}
	if !got["acme/widgets#1245"] {
		t.Fatalf("returned keys = %v, want acme/widgets#1245", got)
	}
	if len(audited) != 1 {
		t.Fatalf("audit lines = %v, want one", audited)
	}

	// Same head, same base tip, next pass: nothing.
	refreshMissingCheckBehindPRs(context.Background(), cfg, f, actionable, audit, discardLogger(), now.Add(staleRedRefreshInterval))
	if len(f.calls) != 1 {
		t.Fatalf("refresh calls = %d after a second pass on the same head and base tip, want 1", len(f.calls))
	}

	// The update landed, the new head still lacks the check, and the base
	// moved: no second update for the same check.
	f.bases = baseRunning("base2", "dashboard-build")
	refreshMissingCheckBehindPRs(context.Background(), cfg, f, actionableWith(missingCheckPR(1245, "head2")), audit, discardLogger(), now.Add(2*staleRedRefreshInterval))
	if len(f.calls) != 1 {
		t.Fatalf("refresh calls = %d for a check one update already failed to start, want 1", len(f.calls))
	}
}

// A required check with a run on the head in any state is slow, not missing:
// the PR is left alone even though the protection facts still list it.
func TestRefreshMissingCheckBehindPRs_RunOnHeadLeftAlone(t *testing.T) {
	for _, state := range []string{"queued", "in_progress", "completed", "pending"} {
		t.Run(state, func(t *testing.T) {
			resetMissingCheckRefresh(t)
			pr := missingCheckPR(1, "h1")
			pr.CIActivity.Checks = append(pr.CIActivity.Checks, github.PRActivityCheck{Name: "dashboard-build", State: state, Head: "h1"})
			f := &fakeMissingCheckRefresher{bases: baseRunning("base1", "dashboard-build"), out: github.StaleRedRefreshOutcome{Updated: true, Settled: true}}
			refreshMissingCheckBehindPRs(context.Background(), escalationTestConfig(), f, actionableWith(pr), nil, discardLogger(), time.Now())
			if len(f.calls) != 0 || len(f.baseReads) != 0 {
				t.Fatalf("calls %d, base reads %v; want a PR whose check has a %s run left alone with no API call", len(f.calls), f.baseReads, state)
			}
		})
	}
}

// Without evidence that the check runs anywhere today the PR is not touched;
// a run on another open PR's head against the same base is evidence.
func TestRefreshMissingCheckBehindPRs_NeedsEvidenceTheCheckExists(t *testing.T) {
	resetMissingCheckRefresh(t)
	cfg := escalationTestConfig()
	f := &fakeMissingCheckRefresher{bases: baseRunning("base1", "pytest"), out: github.StaleRedRefreshOutcome{Updated: true, Settled: true}}
	refreshMissingCheckBehindPRs(context.Background(), cfg, f, actionableWith(missingCheckPR(1, "h1")), nil, discardLogger(), time.Now())
	if len(f.calls) != 0 {
		t.Fatalf("refreshed %d PRs with no evidence the check exists, want 0", len(f.calls))
	}

	resetMissingCheckRefresh(t)
	newer := missingCheckPR(2, "h2")
	newer.Protection = &github.ProtectionFacts{RequiredChecksKnown: true}
	newer.CIActivity.Checks = append(newer.CIActivity.Checks, github.PRActivityCheck{Name: "dashboard-build", State: "in_progress", Head: "h2"})
	refreshMissingCheckBehindPRs(context.Background(), cfg, f, actionableWith(missingCheckPR(1, "h1"), newer), nil, discardLogger(), time.Now())
	if len(f.calls) != 1 || f.calls[0].Number != 1 {
		t.Fatalf("refresh calls = %+v, want #1 refreshed on another open PR's run of the check", f.calls)
	}
}

// A head that moved under the request is not settled: the next pass judges
// it again. A settled refusal (a conflict) is not retried for the same pair
// and does not count as the one update per check.
func TestRefreshMissingCheckBehindPRs_Outcomes(t *testing.T) {
	resetMissingCheckRefresh(t)
	cfg := escalationTestConfig()
	actionable := actionableWith(missingCheckPR(1, "h1"))
	f := &fakeMissingCheckRefresher{bases: baseRunning("base1", "dashboard-build"), out: github.StaleRedRefreshOutcome{Skipped: "head-moved"}}
	now := time.Now()
	refreshMissingCheckBehindPRs(context.Background(), cfg, f, actionable, nil, discardLogger(), now)
	f.out = github.StaleRedRefreshOutcome{Settled: true, Skipped: "conflict"}
	refreshMissingCheckBehindPRs(context.Background(), cfg, f, actionable, nil, discardLogger(), now.Add(staleRedRefreshInterval))
	if len(f.calls) != 2 {
		t.Fatalf("refresh calls = %d, want a retry after head-moved", len(f.calls))
	}
	refreshMissingCheckBehindPRs(context.Background(), cfg, f, actionable, nil, discardLogger(), now.Add(2*staleRedRefreshInterval))
	if len(f.calls) != 2 {
		t.Fatalf("refresh calls = %d, want a settled conflict left alone on the same head and base tip", len(f.calls))
	}
	f.bases = baseRunning("base2", "dashboard-build")
	f.out = github.StaleRedRefreshOutcome{Updated: true, Settled: true}
	refreshMissingCheckBehindPRs(context.Background(), cfg, f, actionable, nil, discardLogger(), now.Add(3*staleRedRefreshInterval))
	if len(f.calls) != 3 {
		t.Fatalf("refresh calls = %d, want the new base tip judged again after a conflict", len(f.calls))
	}

	resetMissingCheckRefresh(t)
	f = &fakeMissingCheckRefresher{bases: baseRunning("base1", "dashboard-build"), err: errors.New("502")}
	refreshMissingCheckBehindPRs(context.Background(), cfg, f, actionable, nil, discardLogger(), now)
	f.err, f.out = nil, github.StaleRedRefreshOutcome{Updated: true, Settled: true}
	refreshMissingCheckBehindPRs(context.Background(), cfg, f, actionable, nil, discardLogger(), now.Add(staleRedRefreshInterval))
	if len(f.calls) != 2 {
		t.Fatalf("refresh calls = %d, want a retry after a failed pass", len(f.calls))
	}
}

// Updates share the stale-red refresh's per-repository cap.
func TestRefreshMissingCheckBehindPRs_PerRepoCap(t *testing.T) {
	resetMissingCheckRefresh(t)
	var prs []github.PullRequest
	for i := 1; i <= staleRedRefreshMaxPerRepo+3; i++ {
		prs = append(prs, missingCheckPR(i, "h"))
	}
	f := &fakeMissingCheckRefresher{bases: baseRunning("base1", "dashboard-build"), out: github.StaleRedRefreshOutcome{Updated: true, Settled: true, BehindBy: 1}}
	got := refreshMissingCheckBehindPRs(context.Background(), escalationTestConfig(), f, actionableWith(prs...), nil, discardLogger(), time.Now())
	if len(f.calls) != staleRedRefreshMaxPerRepo || len(got) != staleRedRefreshMaxPerRepo {
		t.Fatalf("updates = %d (keys %v), want the per-repo cap %d", len(f.calls), got, staleRedRefreshMaxPerRepo)
	}
}

func TestMissingCheckRefreshSkip(t *testing.T) {
	cases := []struct {
		name string
		edit func(*github.PullRequest)
		want string
	}{
		{"candidate", func(*github.PullRequest) {}, ""},
		{"draft", func(p *github.PullRequest) { p.Draft = true }, "draft"},
		{"hold label", func(p *github.PullRequest) { p.Labels = append(p.Labels, "hold") }, "held"},
		{"do-not-merge", func(p *github.PullRequest) { p.Labels = append(p.Labels, "do-not-merge") }, "held"},
		{"parked on a decision", func(p *github.PullRequest) { p.Labels = append(p.Labels, github.PRNeedsDecisionStageLabel) }, "parked"},
		{"no protection facts", func(p *github.PullRequest) { p.Protection = nil }, "no-missing-required-check"},
		{"required set unknown", func(p *github.PullRequest) { p.Protection.RequiredChecksKnown = false }, "no-missing-required-check"},
		{"nothing missing", func(p *github.PullRequest) { p.Protection.MissingRequiredChecks = nil }, "no-missing-required-check"},
		{"red", func(p *github.PullRequest) { p.CIStatus = "failure"; p.FailingChecks = []string{"pytest"} }, "red"},
		{"required check red", func(p *github.PullRequest) { p.Protection.FailingRequiredChecks = []string{"lint"} }, "red"},
		{"CI pending", func(p *github.PullRequest) { p.CIStatus = "pending" }, "ci-not-settled"},
		{"waiting on workflow approval", func(p *github.PullRequest) {
			p.CIActivity.WorkflowApproval = &github.PRWorkflowApproval{}
		}, "workflow-approval-pending"},
		{"queued run for the check on the head", func(p *github.PullRequest) {
			p.CIActivity.Checks = append(p.CIActivity.Checks, github.PRActivityCheck{Name: "dashboard-build", State: "queued", Head: "h"})
		}, "check-reported"},
		{"a run on an older head does not count", func(p *github.PullRequest) {
			p.CIActivity.Checks = append(p.CIActivity.Checks, github.PRActivityCheck{Name: "dashboard-build", State: "completed", Head: "older"})
		}, ""},
		{"conflicting", func(p *github.PullRequest) { p.MergeableState = "dirty" }, "conflict"},
		{"fork without maintainer edits", func(p *github.PullRequest) { p.MaintainerCanModify = false }, "fork-not-editable"},
		{"renovate branch", func(p *github.PullRequest) { p.HeadRef = "renovate/actions"; p.Author = "app/mergeraptor" }, "dependency-bot-branch"},
		{"dependabot", func(p *github.PullRequest) { p.HeadRef = "dependabot/go_modules/x"; p.Author = "dependabot[bot]" }, "dependency-bot-branch"},
		{"returned to contributor", func(p *github.PullRequest) {
			p.Labels = append(p.Labels, stagelabels.ReturnedToContributor.Label())
		}, "returned-to-contributor"},
		{"adopted by the hive", func(p *github.PullRequest) { p.Labels = append(p.Labels, stagelabels.AdoptedByHive.Label()) }, "adopted-by-hive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr := missingCheckPR(1, "h")
			tc.edit(&pr)
			if got := missingCheckRefreshSkip(pr); got != tc.want {
				t.Fatalf("missingCheckRefreshSkip = %q, want %q", got, tc.want)
			}
		})
	}
}

// projectbluefin/server#686: a fork PR whose pull_request_target check (Prow)
// passed while GitHub holds every pull_request run for approval. The probe
// now runs on such a green head, and a held head is never base-refreshed:
// the update would only make a new head whose runs are held again.
func TestRefreshMissingCheckBehindPRs_HeldForkHeadLeftAlone(t *testing.T) {
	resetMissingCheckRefresh(t)
	pr := missingCheckPR(686, "h1")
	pr.CIActivity.Checks = []github.PRActivityCheck{{Name: "prow / prow", State: "completed", Conclusion: "success", Head: "h1"}}
	pr.CIActivity.WorkflowApproval = &github.PRWorkflowApproval{Pending: 4, RunIDs: []int64{1, 2, 3, 4}}
	f := &fakeMissingCheckRefresher{bases: baseRunning("base1", "dashboard-build"), out: github.StaleRedRefreshOutcome{Updated: true, Settled: true}}
	refreshMissingCheckBehindPRs(context.Background(), escalationTestConfig(), f, actionableWith(pr), nil, discardLogger(), time.Now())
	if len(f.calls) != 0 || len(f.baseReads) != 0 {
		t.Fatalf("calls %d, base reads %v; want a head with held runs left alone with no API call", len(f.calls), f.baseReads)
	}
}

// A PR missing many required checks is updated once, not once per check:
// the one update answers every check missing at the time, so a later base
// tip does not buy another update for the next check on the list.
func TestRefreshMissingCheckBehindPRs_OncePerSetOfMissingChecks(t *testing.T) {
	resetMissingCheckRefresh(t)
	cfg := escalationTestConfig()
	missing := []string{"build", "docs", "lint", "unit"}
	pr := func(head string) github.PullRequest {
		p := missingCheckPR(686, head)
		p.Protection.MissingRequiredChecks = append([]string(nil), missing...)
		return p
	}
	f := &fakeMissingCheckRefresher{bases: baseRunning("base1", missing...), out: github.StaleRedRefreshOutcome{Updated: true, Settled: true}}
	now := time.Date(2026, 11, 18, 2, 9, 0, 0, time.UTC)
	for i, tip := range []string{"base1", "base2", "base3", "base4"} {
		f.bases = baseRunning(tip, missing...)
		refreshMissingCheckBehindPRs(context.Background(), cfg, f, actionableWith(pr("head"+tip)), nil, discardLogger(), now.Add(time.Duration(i)*staleRedRefreshInterval))
	}
	if len(f.calls) != 1 {
		t.Fatalf("refresh calls = %d over four base tips with %d missing checks, want 1", len(f.calls), len(missing))
	}
	// A check that becomes required later still gets its one update.
	f.bases = baseRunning("base5", append(missing, "e2e")...)
	next := pr("headbase5")
	next.Protection.MissingRequiredChecks = append(next.Protection.MissingRequiredChecks, "e2e")
	refreshMissingCheckBehindPRs(context.Background(), cfg, f, actionableWith(next), nil, discardLogger(), now.Add(5*staleRedRefreshInterval))
	if len(f.calls) != 2 || f.checks[1] != "e2e" {
		t.Fatalf("refresh calls = %d (checks %v), want a second update for the newly required e2e", len(f.calls), f.checks)
	}
}

// A missing check with no evidence that it runs anywhere did not justify the
// update, so it is not memoised with the ones that did: once its workflow
// lands on the base, the PR earns one update for it (#18945 review).
func TestRefreshMissingCheckBehindPRs_UnevidencedCheckKeepsItsUpdate(t *testing.T) {
	resetMissingCheckRefresh(t)
	cfg := escalationTestConfig()
	pr := func(head string) github.PullRequest {
		p := missingCheckPR(686, head)
		p.Protection.MissingRequiredChecks = []string{"build", "e2e"}
		return p
	}
	f := &fakeMissingCheckRefresher{bases: baseRunning("base1", "build"), out: github.StaleRedRefreshOutcome{Updated: true, Settled: true}}
	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	refreshMissingCheckBehindPRs(context.Background(), cfg, f, actionableWith(pr("h1")), nil, discardLogger(), now)
	if len(f.calls) != 1 || f.checks[0] != "build" {
		t.Fatalf("first pass: calls=%d checks=%v, want one update for build", len(f.calls), f.checks)
	}
	// e2e's workflow now runs on the base; build still has its update spent.
	f.bases = baseRunning("base2", "build", "e2e")
	refreshMissingCheckBehindPRs(context.Background(), cfg, f, actionableWith(pr("h2")), nil, discardLogger(), now.Add(staleRedRefreshInterval))
	if len(f.calls) != 2 || f.checks[1] != "e2e" {
		t.Fatalf("second pass: calls=%d checks=%v, want one more update for e2e", len(f.calls), f.checks)
	}
	f.bases = baseRunning("base3", "build", "e2e")
	refreshMissingCheckBehindPRs(context.Background(), cfg, f, actionableWith(pr("h3")), nil, discardLogger(), now.Add(2*staleRedRefreshInterval))
	if len(f.calls) != 2 {
		t.Fatalf("third pass: calls=%d, want no more updates", len(f.calls))
	}
}
