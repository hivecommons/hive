package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

type fakeStaleRedRefresher struct {
	bases     map[string]github.StaleRedBaseState
	baseErr   error
	baseReads []string
	calls     []github.PullRequest
	out       github.StaleRedRefreshOutcome
	err       error
}

func (f *fakeStaleRedRefresher) StaleRedBaseStateFor(_ context.Context, repo, baseRef string) (github.StaleRedBaseState, bool, error) {
	f.baseReads = append(f.baseReads, repo+"@"+baseRef)
	if f.baseErr != nil {
		return github.StaleRedBaseState{}, false, f.baseErr
	}
	st, ok := f.bases[repo+"@"+baseRef]
	return st, ok, nil
}

func (f *fakeStaleRedRefresher) RefreshStaleRedPR(_ context.Context, pr github.PullRequest, _ github.StaleRedBaseState) (github.StaleRedRefreshOutcome, error) {
	f.calls = append(f.calls, pr)
	return f.out, f.err
}

// resetStaleRedRefresh gives each test a fresh pass clock and memo.
func resetStaleRedRefresh(t *testing.T) {
	t.Helper()
	prev := staleRedRefresh
	staleRedRefresh = &staleRedRefreshState{}
	t.Cleanup(func() { staleRedRefresh = prev })
}

// staleRedPR is projectbluefin/actions#544 in miniature: a contributor's
// fork PR (maintainers may edit), red on pytest, CI settled.
func staleRedPR(number int, head string) github.PullRequest {
	pr := redPR("widgets", number, "contributor", head)
	pr.FailingChecks = []string{"pytest"}
	pr.BaseRef = "main"
	pr.FromFork = true
	pr.MaintainerCanModify = true
	pr.Labels = []string{"needs-human"}
	return pr
}

func greenPytestBase(sha string) map[string]github.StaleRedBaseState {
	return map[string]github.StaleRedBaseState{"acme/widgets@main": {
		SHA: sha, Completed: map[string]bool{"pytest": true}, Red: map[string]bool{},
	}}
}

// The base fixed what the PR's pytest tripped on: the hive merges the base
// in once for that (head, base tip), audits it, and tells the reaper to hold
// its fix kick. A second pass on the same head and tip makes no call; a new
// base tip is judged again.
func TestRefreshStaleRedBehindPRs_OncePerHeadAndBaseTip(t *testing.T) {
	resetStaleRedRefresh(t)
	cfg := escalationTestConfig()
	actionable := actionableWith(staleRedPR(544, "head1"))
	f := &fakeStaleRedRefresher{bases: greenPytestBase("base1"), out: github.StaleRedRefreshOutcome{Updated: true, Settled: true, BehindBy: 3}}
	var audited []string
	audit := func(repo string, number int, detail string) { audited = append(audited, detail) }
	now := time.Date(2026, 11, 10, 5, 5, 0, 0, time.UTC)

	got := refreshStaleRedBehindPRs(context.Background(), cfg, f, actionable, audit, discardLogger(), now)
	if len(f.calls) != 1 || f.calls[0].Repo != "acme/widgets" {
		t.Fatalf("refresh calls = %+v, want one for acme/widgets#544", f.calls)
	}
	if !got["acme/widgets#544"] {
		t.Fatalf("returned keys = %v, want acme/widgets#544 so the reaper skips it", got)
	}
	if len(audited) != 1 {
		t.Fatalf("audit lines = %v, want one", audited)
	}

	// Same head, same base tip, next pass: nothing.
	refreshStaleRedBehindPRs(context.Background(), cfg, f, actionable, audit, discardLogger(), now.Add(staleRedRefreshInterval))
	if len(f.calls) != 1 {
		t.Fatalf("refresh calls = %d after a second pass on the same head and base tip, want 1", len(f.calls))
	}

	// The base moved: the same head is judged against the new tip.
	f.bases = greenPytestBase("base2")
	refreshStaleRedBehindPRs(context.Background(), cfg, f, actionable, audit, discardLogger(), now.Add(2*staleRedRefreshInterval))
	if len(f.calls) != 2 {
		t.Fatalf("refresh calls = %d after the base moved, want 2", len(f.calls))
	}
}

// Passes are spaced: a cycle inside the interval reads nothing.
func TestRefreshStaleRedBehindPRs_Interval(t *testing.T) {
	resetStaleRedRefresh(t)
	cfg := escalationTestConfig()
	f := &fakeStaleRedRefresher{bases: greenPytestBase("base1"), out: github.StaleRedRefreshOutcome{Settled: true, Skipped: "current"}}
	now := time.Date(2026, 11, 10, 5, 5, 0, 0, time.UTC)
	refreshStaleRedBehindPRs(context.Background(), cfg, f, actionableWith(staleRedPR(1, "h1")), nil, discardLogger(), now)
	refreshStaleRedBehindPRs(context.Background(), cfg, f, actionableWith(staleRedPR(2, "h2")), nil, discardLogger(), now.Add(time.Minute))
	if len(f.baseReads) != 1 || len(f.calls) != 1 {
		t.Fatalf("base reads %v, calls %d; want the second cycle inside the interval to do nothing", f.baseReads, len(f.calls))
	}
}

// Only a base that has completed the failing check green clears a PR: a check
// still red there, or one the base never ran (a PR-only check), is not the
// base's to fix, and the PR is not touched.
func TestRefreshStaleRedBehindPRs_BaseMustClearEveryFailingCheck(t *testing.T) {
	resetStaleRedRefresh(t)
	cfg := escalationTestConfig()
	redOnBase := staleRedPR(1, "h1")
	redOnBase.FailingChecks = []string{"pytest", "build"}
	prOnly := staleRedPR(2, "h2")
	prOnly.FailingChecks = []string{"pytest", "Check consumer validation evidence"}
	f := &fakeStaleRedRefresher{bases: map[string]github.StaleRedBaseState{"acme/widgets@main": {
		SHA: "base1", Completed: map[string]bool{"pytest": true, "build": true}, Red: map[string]bool{"build": true},
	}}}
	refreshStaleRedBehindPRs(context.Background(), cfg, f, actionableWith(redOnBase, prOnly), nil, discardLogger(), time.Now())
	if len(f.calls) != 0 {
		t.Fatalf("refreshed %d PRs the base does not clear, want 0", len(f.calls))
	}
	if len(f.baseReads) != 1 {
		t.Fatalf("base reads = %v, want one per repo and base per pass", f.baseReads)
	}
}

// Updates are capped per repository per pass; the rest wait for a later pass.
func TestRefreshStaleRedBehindPRs_PerRepoCap(t *testing.T) {
	resetStaleRedRefresh(t)
	cfg := escalationTestConfig()
	var prs []github.PullRequest
	for i := 1; i <= staleRedRefreshMaxPerRepo+3; i++ {
		prs = append(prs, staleRedPR(i, "h"))
	}
	f := &fakeStaleRedRefresher{bases: greenPytestBase("base1"), out: github.StaleRedRefreshOutcome{Updated: true, Settled: true, BehindBy: 1}}
	got := refreshStaleRedBehindPRs(context.Background(), cfg, f, actionableWith(prs...), nil, discardLogger(), time.Now())
	if len(f.calls) != staleRedRefreshMaxPerRepo || len(got) != staleRedRefreshMaxPerRepo {
		t.Fatalf("updates = %d (keys %v), want the per-repo cap %d", len(f.calls), got, staleRedRefreshMaxPerRepo)
	}
}

// A transient failure is not settled: the next pass tries the same pair again.
func TestRefreshStaleRedBehindPRs_ErrorRetriesNextPass(t *testing.T) {
	resetStaleRedRefresh(t)
	cfg := escalationTestConfig()
	actionable := actionableWith(staleRedPR(544, "head1"))
	f := &fakeStaleRedRefresher{bases: greenPytestBase("base1"), err: errors.New("502")}
	now := time.Now()
	refreshStaleRedBehindPRs(context.Background(), cfg, f, actionable, nil, discardLogger(), now)
	f.err = nil
	f.out = github.StaleRedRefreshOutcome{Updated: true, Settled: true}
	refreshStaleRedBehindPRs(context.Background(), cfg, f, actionable, nil, discardLogger(), now.Add(staleRedRefreshInterval))
	if len(f.calls) != 2 {
		t.Fatalf("refresh calls = %d, want a retry after the failed pass", len(f.calls))
	}
}

func TestStaleRedRefreshSkip(t *testing.T) {
	base := staleRedPR(1, "h")
	cases := []struct {
		name string
		edit func(*github.PullRequest)
		want string
	}{
		{"candidate", func(*github.PullRequest) {}, ""},
		{"draft", func(p *github.PullRequest) { p.Draft = true }, "draft"},
		{"hold label", func(p *github.PullRequest) { p.Labels = append(p.Labels, "hold") }, "held"},
		{"do-not-merge", func(p *github.PullRequest) { p.Labels = append(p.Labels, "do-not-merge") }, "held"},
		{"green", func(p *github.PullRequest) { p.CIStatus = "success" }, "not-red"},
		{"red with no named check", func(p *github.PullRequest) { p.FailingChecks = nil }, "not-red"},
		{"CI still running", func(p *github.PullRequest) { p.CIChecksRunning = true }, "ci-running"},
		{"open shared-CI incident", func(p *github.PullRequest) { p.SharedCIIncident = 12 }, "shared-ci-incident-open"},
		{"red on the base too", func(p *github.PullRequest) { p.BaseRedChecks = []string{"pytest"} }, "base-red"},
		{"no base ref", func(p *github.PullRequest) { p.BaseRef = "" }, "head-or-base-unknown"},
		{"fork without maintainer edits", func(p *github.PullRequest) { p.MaintainerCanModify = false }, "fork-not-editable"},
		{"dependency bot branch", func(p *github.PullRequest) { p.HeadRef = "renovate/actions"; p.Author = "app/mergeraptor" }, "dependency-bot-branch"},
		{"returned to contributor", func(p *github.PullRequest) {
			p.Labels = append(p.Labels, stagelabels.ReturnedToContributor.Label())
		}, "returned-to-contributor"},
		{"adopted by the hive", func(p *github.PullRequest) { p.Labels = append(p.Labels, stagelabels.AdoptedByHive.Label()) }, "adopted-by-hive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr := base
			pr.Labels = append([]string(nil), base.Labels...)
			pr.FailingChecks = append([]string(nil), base.FailingChecks...)
			tc.edit(&pr)
			if got := staleRedRefreshSkip(pr); got != tc.want {
				t.Fatalf("staleRedRefreshSkip = %q, want %q", got, tc.want)
			}
		})
	}
}
