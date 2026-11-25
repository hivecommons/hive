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

// projectbluefin/utah-packages#1258 (2026-11-21): the fork PR's only red
// check, Canary, refuses fork PRs and is green on main because main is not a
// fork. The pass read that as "the base fixed it" and merged main in at
// 11:55:47Z, which discarded the head a maintainer's Canary run would prove.
// A head red only on fork guards is not touched.
func TestRefreshStaleRedBehindPRs_SkipsForkGuardOnlyHead(t *testing.T) {
	resetStaleRedRefresh(t)
	cfg := escalationTestConfig()
	pr := staleRedPR(1258, "c40685e")
	pr.FailingChecks = []string{"Canary"}
	pr.CIFailureExcerpt = "Canary: a fork pull request cannot publish the canary image, and no maintainer-run Canary has passed on c40685e"
	f := &fakeStaleRedRefresher{bases: map[string]github.StaleRedBaseState{"acme/widgets@main": {
		SHA: "base1", Completed: map[string]bool{"Canary": true}, Red: map[string]bool{},
	}}, out: github.StaleRedRefreshOutcome{Updated: true, Settled: true, BehindBy: 2}}
	got := refreshStaleRedBehindPRs(context.Background(), cfg, f, actionableWith(pr), nil, discardLogger(), time.Now())
	if len(f.calls) != 0 || len(got) != 0 {
		t.Fatalf("refresh calls %d (keys %v), want none for a head red only on a fork guard", len(f.calls), got)
	}
}

// staleRedAdvisory is a non-required check that judges the PR's own diff.
const staleRedAdvisory = "Mutation check on changed lines (advisory)"

// requiredRun is a completed check run on the head with conclusion.
func requiredRun(name, conclusion string) github.PRActivityCheck {
	return github.PRActivityCheck{Name: name, State: "completed", Conclusion: conclusion}
}

// withRequiredChecks gives pr the facts of a base whose required set is
// known and reported on the head: the protection facts (the required checks
// concluded failure red) and the head's CI activity carrying required.
func withRequiredChecks(pr github.PullRequest, required ...github.PRActivityCheck) github.PullRequest {
	f := &github.ProtectionFacts{RequiredChecksKnown: true, RequiredChecksAllReported: true}
	a := &github.PRCIActivity{Head: pr.HeadSHA, RequiredKnown: true}
	for _, c := range required {
		a.Required = append(a.Required, c.Name)
		a.Checks = append(a.Checks, c)
		if c.Conclusion == "failure" {
			f.FailingRequiredChecks = append(f.FailingRequiredChecks, c.Name)
		}
	}
	pr.Protection, pr.CIActivity = f, a
	return pr
}

// greenOnBase is a base tip with every named check completed green.
func greenOnBase(sha string, names ...string) map[string]github.StaleRedBaseState {
	done := map[string]bool{}
	for _, n := range names {
		done[n] = true
	}
	return map[string]github.StaleRedBaseState{"acme/widgets@main": {
		SHA: sha, Completed: done, Red: map[string]bool{}, Reported: done,
	}}
}

// Danathar/tududi#658: every required check passed, only a non-required
// advisory check red, and that check green on the base tip. A base merge
// cannot clear a check that judges the PR's own diff, and the red does not
// block the merge, so the PR is not touched and the base is not even read.
func TestRefreshStaleRedBehindPRs_SkipsNonRequiredOnlyRed(t *testing.T) {
	resetStaleRedRefresh(t)
	cfg := escalationTestConfig()
	pr := withRequiredChecks(staleRedPR(658, "h1"), requiredRun("pytest", "success"), requiredRun("lint", "skipped"))
	pr.FailingChecks = []string{staleRedAdvisory}
	if got := staleRedRefreshSkip(pr); got != "only-non-required-checks-red" {
		t.Fatalf("staleRedRefreshSkip = %q, want only-non-required-checks-red", got)
	}
	f := &fakeStaleRedRefresher{bases: greenOnBase("base1", "pytest", "lint", staleRedAdvisory), out: github.StaleRedRefreshOutcome{Updated: true, Settled: true, BehindBy: 1}}
	got := refreshStaleRedBehindPRs(context.Background(), cfg, f, actionableWith(pr), nil, discardLogger(), time.Now())
	if len(f.calls) != 0 || len(got) != 0 || len(f.baseReads) != 0 {
		t.Fatalf("refresh calls %d (keys %v, base reads %v), want none for a head red only on a non-required check", len(f.calls), got, f.baseReads)
	}
}

// A required check red on the head and green on the base tip is refreshed
// as before, also with a non-required check red beside it that the base
// tip clears too.
func TestRefreshStaleRedBehindPRs_RequiredRedStillRefreshed(t *testing.T) {
	resetStaleRedRefresh(t)
	cfg := escalationTestConfig()
	requiredOnly := withRequiredChecks(staleRedPR(1, "h1"), requiredRun("pytest", "failure"))
	withAdvisory := withRequiredChecks(staleRedPR(2, "h2"), requiredRun("pytest", "failure"))
	withAdvisory.FailingChecks = []string{"pytest", staleRedAdvisory}
	f := &fakeStaleRedRefresher{bases: greenOnBase("base1", "pytest", staleRedAdvisory), out: github.StaleRedRefreshOutcome{Updated: true, Settled: true, BehindBy: 1}}
	got := refreshStaleRedBehindPRs(context.Background(), cfg, f, actionableWith(requiredOnly, withAdvisory), nil, discardLogger(), time.Now())
	if len(f.calls) != 2 || !got["acme/widgets#1"] || !got["acme/widgets#2"] {
		t.Fatalf("refresh calls %d (keys %v), want both PRs with a red required check refreshed", len(f.calls), got)
	}
}

// A required check that did not pass is not "only non-required checks
// red", even though the CI rollup lists only failure and action_required as
// failing: a timed-out, cancelled or not-started required run, a required
// commit status still pending, or a run the activity cannot attribute. Such
// a head beside a base-fixable advisory red is refreshed as today (which
// also re-runs the required check).
func TestRefreshStaleRedBehindPRs_RequiredNotPassedRefreshedAsToday(t *testing.T) {
	cases := []struct {
		name     string
		required github.PRActivityCheck
	}{
		{"timed out", requiredRun("pytest", "timed_out")},
		{"cancelled", requiredRun("pytest", "cancelled")},
		{"startup failure", requiredRun("pytest", "startup_failure")},
		{"stale", requiredRun("pytest", "stale")},
		{"pending commit status", github.PRActivityCheck{Name: "pytest", State: "pending", Kind: "commit_status"}},
		{"producer not attributable", github.PRActivityCheck{Name: "pytest", State: "unknown"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetStaleRedRefresh(t)
			cfg := escalationTestConfig()
			pr := withRequiredChecks(staleRedPR(658, "h1"), tc.required)
			pr.FailingChecks = []string{staleRedAdvisory}
			if got := staleRedRefreshSkip(pr); got != "" {
				t.Fatalf("staleRedRefreshSkip = %q, want a candidate as today", got)
			}
			f := &fakeStaleRedRefresher{bases: greenOnBase("base1", "pytest", staleRedAdvisory), out: github.StaleRedRefreshOutcome{Updated: true, Settled: true, BehindBy: 1}}
			got := refreshStaleRedBehindPRs(context.Background(), cfg, f, actionableWith(pr), nil, discardLogger(), time.Now())
			if len(f.calls) != 1 || !got["acme/widgets#658"] {
				t.Fatalf("refresh calls %d (keys %v), want today's refresh", len(f.calls), got)
			}
		})
	}
}

// A required check run still in progress beside an advisory red is skipped
// as ci-running, exactly as today.
func TestStaleRedRefreshSkip_PendingRequiredRunUnchanged(t *testing.T) {
	pr := withRequiredChecks(staleRedPR(658, "h1"), github.PRActivityCheck{Name: "pytest", State: "in_progress"})
	pr.FailingChecks = []string{staleRedAdvisory}
	pr.CIChecksRunning = true
	if got := staleRedRefreshSkip(pr); got != "ci-running" {
		t.Fatalf("staleRedRefreshSkip = %q, want ci-running as today", got)
	}
}

// The base-tip proof still covers every failing check, required or not: a
// required check green on the base tip beside a non-required check the base
// never runs is not refreshed, as before. The new skip never makes the
// refresh fire where it did not.
func TestRefreshStaleRedBehindPRs_NonRequiredCheckStillJudgedOnBase(t *testing.T) {
	resetStaleRedRefresh(t)
	cfg := escalationTestConfig()
	pr := withRequiredChecks(staleRedPR(1, "h1"), requiredRun("pytest", "failure"))
	pr.FailingChecks = []string{"pytest", staleRedAdvisory}
	f := &fakeStaleRedRefresher{bases: greenOnBase("base1", "pytest"), out: github.StaleRedRefreshOutcome{Updated: true, Settled: true, BehindBy: 1}}
	got := refreshStaleRedBehindPRs(context.Background(), cfg, f, actionableWith(pr), nil, discardLogger(), time.Now())
	if len(f.calls) != 0 || len(got) != 0 {
		t.Fatalf("refresh calls %d (keys %v), want none while a failing check has no run on the base tip", len(f.calls), got)
	}
}

// When the required set is unknown, empty or not fully reported on the
// head, or the head's CI activity is missing or for another head, any
// failing check counts as before: a head red only on the advisory check,
// green on the base tip, is refreshed.
func TestRefreshStaleRedBehindPRs_UnknownRequiredSetUnchanged(t *testing.T) {
	passed := withRequiredChecks(staleRedPR(658, "h1"), requiredRun("pytest", "success"))
	otherHead := *passed.CIActivity
	otherHead.Head = "h0"
	cases := []struct {
		name       string
		protection *github.ProtectionFacts
		activity   *github.PRCIActivity
	}{
		{"no protection facts", nil, passed.CIActivity},
		{"required set unknown", &github.ProtectionFacts{}, passed.CIActivity},
		{"required set not fully reported", &github.ProtectionFacts{RequiredChecksKnown: true}, passed.CIActivity},
		{"no CI activity", passed.Protection, nil},
		{"CI activity for another head", passed.Protection, &otherHead},
		{"CI activity with no required set", passed.Protection, &github.PRCIActivity{Head: "h1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetStaleRedRefresh(t)
			cfg := escalationTestConfig()
			pr := staleRedPR(658, "h1")
			pr.FailingChecks = []string{staleRedAdvisory}
			pr.Protection, pr.CIActivity = tc.protection, tc.activity
			if got := staleRedRefreshSkip(pr); got != "" {
				t.Fatalf("staleRedRefreshSkip = %q, want a candidate", got)
			}
			f := &fakeStaleRedRefresher{bases: greenOnBase("base1", staleRedAdvisory), out: github.StaleRedRefreshOutcome{Updated: true, Settled: true, BehindBy: 1}}
			got := refreshStaleRedBehindPRs(context.Background(), cfg, f, actionableWith(pr), nil, discardLogger(), time.Now())
			if len(f.calls) != 1 || !got["acme/widgets#658"] {
				t.Fatalf("refresh calls %d (keys %v), want today's refresh", len(f.calls), got)
			}
		})
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
		// Danathar/tududi#658: every required check passed, only an
		// advisory check red.
		{"red only on non-required checks", func(p *github.PullRequest) {
			*p = withRequiredChecks(*p, requiredRun("build", "success"))
		}, "only-non-required-checks-red"},
		{"a required check red", func(p *github.PullRequest) { *p = withRequiredChecks(*p, requiredRun("pytest", "failure")) }, ""},
		{"a required check timed out", func(p *github.PullRequest) { *p = withRequiredChecks(*p, requiredRun("build", "timed_out")) }, ""},
		{"required set unknown", func(p *github.PullRequest) { p.Protection = &github.ProtectionFacts{} }, ""},
		// A dependency bot's branch keeps today's route to its rebase
		// request (stale_red_bot_rebase.go).
		{"dependency bot red only on non-required checks", func(p *github.PullRequest) {
			*p = withRequiredChecks(*p, requiredRun("build", "success"))
			p.HeadRef, p.Author = "renovate/actions", "app/mergeraptor"
		}, "dependency-bot-branch"},
		// projectbluefin/utah-packages#1258: Canary refuses fork PRs, and
		// it is green on the base only because the base is not a fork.
		{"red only on a fork guard", func(p *github.PullRequest) {
			p.FailingChecks = []string{"Canary"}
			p.CIFailureExcerpt = "Canary: a fork pull request cannot publish the canary image"
		}, "fork-guard"},
		// A fork guard beside another failure is not what the PR waits on.
		{"fork guard and another failure", func(p *github.PullRequest) {
			p.FailingChecks = []string{"Canary", "pytest"}
			p.CIFailureExcerpt = "Canary: a fork pull request cannot publish the canary image\npytest: FAILED test_widget"
		}, ""},
		{"no base ref", func(p *github.PullRequest) { p.BaseRef = "" }, "head-or-base-unknown"},
		{"fork without maintainer edits", func(p *github.PullRequest) { p.MaintainerCanModify = false }, "fork-not-editable"},
		{"dependency bot branch", func(p *github.PullRequest) { p.HeadRef = "renovate/actions"; p.Author = "app/mergeraptor" }, "dependency-bot-branch"},
		// The base cleared the red, so a 072 PR is a candidate
		// (projectbluefin/actions#765).
		{"returned to contributor", func(p *github.PullRequest) {
			p.Labels = append(p.Labels, stagelabels.ReturnedToContributor.Label())
		}, ""},
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
