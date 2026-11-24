package main

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// srbRefresher is the stale-red refresher plus what the pass needs to ask a
// dependency bot to rebase. Its Renovate request ticks one box per PR; a new
// head from the bot comes with an unticked box (untick).
type srbRefresher struct {
	fakeStaleRedRefresher
	depBotFakeReader
	denied   bool
	noBox    bool
	ticked   map[int]bool
	asks     int
	edits    []string
	behind   int
	compares int
	tipAt    time.Time
	tipReads int
	// heads is each PR's head as the pass sees it (track); commits
	// overrides a PR's commit list, which is otherwise one bot commit at
	// that head.
	heads   map[int]string
	commits map[int][]github.PRCommit
}

func newSRBRefresher(bases map[string]github.StaleRedBaseState) *srbRefresher {
	return &srbRefresher{
		fakeStaleRedRefresher: fakeStaleRedRefresher{bases: bases, out: github.StaleRedRefreshOutcome{Updated: true, Settled: true, BehindBy: 1}},
		ticked:                map[int]bool{},
		behind:                2,
		heads:                 map[int]string{},
		commits:               map[int][]github.PRCommit{},
	}
}

// track remembers each PR's head for the commit list and returns the
// actionable set.
func (f *srbRefresher) track(prs ...github.PullRequest) *github.ActionableResult {
	for _, pr := range prs {
		f.heads[pr.Number] = pr.HeadSHA
	}
	return actionableWith(prs...)
}

func (f *srbRefresher) ListPRCommits(_ context.Context, _ string, number int) ([]github.PRCommit, error) {
	if c, ok := f.commits[number]; ok {
		return c, nil
	}
	return []github.PRCommit{{SHA: f.heads[number], Author: "renovate[bot]", ParentCount: 1}}, nil
}

func (f *srbRefresher) BotRebaseRequestAllowed(string, int) bool { return !f.denied }

func (f *srbRefresher) RequestRenovateRebase(_ context.Context, repo string, number int, head string) (github.RenovateRebaseCheckbox, error) {
	f.asks++
	switch {
	case f.noBox:
		return github.RenovateRebaseCheckboxAbsent, nil
	case f.ticked[number]:
		return github.RenovateRebaseCheckboxTicked, nil
	}
	f.ticked[number] = true
	f.edits = append(f.edits, repo+"#"+head)
	return github.RenovateRebaseCheckboxUnticked, nil
}

func (f *srbRefresher) untick(number int) { delete(f.ticked, number) }

func (f *srbRefresher) StaleRedBehindBy(context.Context, string, string, string) (int, error) {
	f.compares++
	return f.behind, nil
}

func (f *srbRefresher) CommitCommittedAt(context.Context, string, string) (time.Time, error) {
	f.tipReads++
	return f.tipAt, nil
}

var (
	_ staleRedRefresher  = (*srbRefresher)(nil)
	_ staleRedBotRebaser = (*srbRefresher)(nil)
	_ depBotRebasePolicy = (*srbRefresher)(nil)
	_ depBotCommitLister = (*srbRefresher)(nil)
)

// useSRBStore gives the pass a fresh dependency-bot ledger.
func useSRBStore(t *testing.T) *depBotConflictStore {
	t.Helper()
	store := loadDepBotConflictStore(filepath.Join(t.TempDir(), "dependency-bot-conflicts.json"))
	prev := staleRedDepBotStore
	staleRedDepBotStore = func() *depBotConflictStore { return store }
	t.Cleanup(func() { staleRedDepBotStore = prev })
	return store
}

func srbLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// srbBotPR is projectbluefin/actions#775 in miniature: a Renovate branch
// (opened by another App) red on pytest, CI settled, behind main.
func srbBotPR(number int, head string) github.PullRequest {
	pr := redPR("widgets", number, "mergeraptor[bot]", head)
	pr.FailingChecks = []string{"pytest"}
	pr.BaseRef = "main"
	pr.HeadRef = "renovate/github-actions-(non-major)"
	pr.State = "open"
	pr.MergeableState = "blocked"
	return pr
}

// prOnlyBase is a base tip that runs pytest green and never runs contract.
func prOnlyBase(sha string) map[string]github.StaleRedBaseState {
	return map[string]github.StaleRedBaseState{"acme/widgets@main": {
		SHA: sha, Completed: map[string]bool{"pytest": true}, Red: map[string]bool{}, Reported: map[string]bool{"pytest": true},
	}}
}

// srbPROnlyPR is projectbluefin/utah#1471 in miniature: a Renovate PR red
// on `contract`, a check that runs only on pull requests, which failed at
// ranAt.
func srbPROnlyPR(number int, head string, ranAt time.Time) github.PullRequest {
	pr := srbBotPR(number, head)
	pr.FailingChecks = []string{"contract"}
	pr.CIActivity = &github.PRCIActivity{Head: head, Checks: []github.PRActivityCheck{
		{Name: "contract", State: "completed", Conclusion: "failure", Since: &ranAt, Head: head},
		{Name: "pytest", State: "completed", Conclusion: "success", Since: &ranAt, Head: head},
	}}
	return pr
}

// Part 1: every failing check is green on the base tip and the bot's branch
// is behind it. The pass ticks the rebase box once for that (head, base tip),
// never updates the branch, logs and audits it, and records the request.
// The same tip again asks nothing; a new tip on the same head asks again
// but keeps the first request's time; the bot's new head red on the same
// check is not asked about again.
func TestStaleRedBotRebase_OncePerHeadAndBaseTip(t *testing.T) {
	resetStaleRedRefresh(t)
	store := useSRBStore(t)
	cfg := escalationTestConfig()
	f := newSRBRefresher(greenPytestBase("base1"))
	logger, logs := srbLogger()
	var audited []string
	audit := func(repo string, number int, detail string) { audited = append(audited, detail) }
	now := time.Date(2026, 11, 23, 11, 0, 0, 0, time.UTC)

	got := refreshStaleRedBehindPRs(context.Background(), cfg, f, f.track(srbBotPR(775, "head1")), audit, logger, now)
	if len(f.calls) != 0 {
		t.Fatalf("update-branch calls = %+v, want none on a dependency bot's branch", f.calls)
	}
	if f.asks != 1 || len(f.edits) != 1 {
		t.Fatalf("rebase requests = %d (edits %v), want one", f.asks, f.edits)
	}
	if !got["acme/widgets#775"] {
		t.Fatalf("returned keys = %v, want acme/widgets#775", got)
	}
	if !strings.Contains(logs.String(), "stale-red refresh: asked dependency bot to rebase") {
		t.Fatalf("log = %q, want the request logged", logs.String())
	}
	if len(audited) != 1 || !strings.Contains(audited[0], "action=bot_rebase_request") {
		t.Fatalf("audit = %v, want one bot_rebase_request line", audited)
	}
	st, ok := store.rebaseState("acme/widgets#775")
	if !ok || st.HeadSHA != "head1" || st.BaseSHA != "base1" || st.Trigger != staleRedBotTriggerBaseClears ||
		st.Via != depBotViaRenovateCheckbox || !st.RequestedAt.Equal(now) {
		t.Fatalf("recorded request = %+v (found %v), want head1/base1 stale-red at %v", st, ok, now)
	}

	// Same head, same base tip: nothing.
	refreshStaleRedBehindPRs(context.Background(), cfg, f, f.track(srbBotPR(775, "head1")), audit, logger, now.Add(staleRedRefreshInterval))
	if f.asks != 1 || f.compares != 1 {
		t.Fatalf("asks = %d, compares = %d after a second pass on the same head and tip; want 1 and 1", f.asks, f.compares)
	}

	// The base moved, same head: asked again (the box is still ticked, so
	// nothing is written), and the first request's time is kept.
	f.bases = greenPytestBase("base2")
	refreshStaleRedBehindPRs(context.Background(), cfg, f, f.track(srbBotPR(775, "head1")), audit, logger, now.Add(2*staleRedRefreshInterval))
	if f.asks != 2 || len(f.edits) != 1 {
		t.Fatalf("asks = %d, edits = %d after the base moved; want 2 asks, 1 edit", f.asks, len(f.edits))
	}
	if st, _ := store.rebaseState("acme/widgets#775"); st.BaseSHA != "base2" || !st.RequestedAt.Equal(now) || st.Via != depBotViaRenovateCheckbox {
		t.Fatalf("record after a new tip = %+v, want base2 with the first request's time and mechanism", st)
	}

	// The bot pushed a new head that is red on the same check against a
	// newer base: not asked again (the bump breaks the check), see
	// TestStaleRedBotRebase_SameChecksAfterRebaseNotAskedAgain.
	f.untick(775)
	f.bases = greenPytestBase("base3")
	refreshStaleRedBehindPRs(context.Background(), cfg, f, f.track(srbBotPR(775, "head2")), audit, logger, now.Add(3*staleRedRefreshInterval))
	if f.asks != 2 || len(f.edits) != 1 {
		t.Fatalf("asks = %d, edits = %d after the bot's new head failed the same check; want 2 and 1", f.asks, len(f.edits))
	}
	if len(f.calls) != 0 {
		t.Fatalf("update-branch calls = %d, want none ever on a bot's branch", len(f.calls))
	}
}

// A bot's branch that is not behind the base tip is not asked about.
func TestStaleRedBotRebase_NotBehind(t *testing.T) {
	resetStaleRedRefresh(t)
	useSRBStore(t)
	f := newSRBRefresher(greenPytestBase("base1"))
	f.behind = 0
	got := refreshStaleRedBehindPRs(context.Background(), escalationTestConfig(), f, f.track(srbBotPR(775, "head1")), nil, discardLogger(), time.Now())
	if f.asks != 0 || len(f.calls) != 0 || len(got) != 0 {
		t.Fatalf("asks %d, updates %d, keys %v; want nothing for a current branch", f.asks, len(f.calls), got)
	}
}

// Dependabot refuses commands from apps: its branch keeps today's handling
// (skipped, not even a base read), and a non-bot PR is still updated.
func TestStaleRedBotRebase_DependabotAndHumanPRsUnchanged(t *testing.T) {
	resetStaleRedRefresh(t)
	useSRBStore(t)
	dependabot := srbBotPR(1, "dep1")
	dependabot.Author = "dependabot[bot]"
	dependabot.HeadRef = "dependabot/npm_and_yarn/lodash-4.17.21"
	f := newSRBRefresher(greenPytestBase("base1"))
	refreshStaleRedBehindPRs(context.Background(), escalationTestConfig(), f, actionableWith(dependabot), nil, discardLogger(), time.Now())
	if f.asks != 0 || len(f.calls) != 0 || len(f.baseReads) != 0 {
		t.Fatalf("Dependabot PR: asks %d, updates %d, base reads %v; want none", f.asks, len(f.calls), f.baseReads)
	}

	resetStaleRedRefresh(t)
	got := refreshStaleRedBehindPRs(context.Background(), escalationTestConfig(), f, actionableWith(staleRedPR(544, "head1")), nil, discardLogger(), time.Now())
	if len(f.calls) != 1 || f.asks != 0 || !got["acme/widgets#544"] {
		t.Fatalf("non-bot PR: updates %d, asks %d, keys %v; want one update-branch and no request", len(f.calls), f.asks, got)
	}
}

// The write-back gate would refuse bot_rebase_request: the pass decides
// that without an API call, before the base tip is read, so nothing is read,
// written or recorded, and it says so in the debug log.
func TestStaleRedBotRebase_GateRefuses(t *testing.T) {
	resetStaleRedRefresh(t)
	store := useSRBStore(t)
	f := newSRBRefresher(greenPytestBase("base1"))
	f.denied = true
	logger, logs := srbLogger()
	got := refreshStaleRedBehindPRs(context.Background(), escalationTestConfig(), f, f.track(srbBotPR(775, "head1")), nil, logger, time.Now())
	if f.asks != 0 || len(f.edits) != 0 || len(f.calls) != 0 || len(got) != 0 {
		t.Fatalf("asks %d, edits %v, updates %d, keys %v; want nothing written", f.asks, f.edits, len(f.calls), got)
	}
	if len(f.baseReads) != 0 || f.compares != 0 {
		t.Fatalf("base reads %v, compares %d; want no API call when the gate would refuse", f.baseReads, f.compares)
	}
	if !strings.Contains(logs.String(), "bot_rebase_request not allowed") {
		t.Fatalf("log = %q, want the refusal logged", logs.String())
	}
	if _, ok := store.rebaseState("acme/widgets#775"); ok {
		t.Fatal("a refused request was recorded")
	}
}

// Ticking the box regenerates the branch and drops every commit that is not
// the bot's: a branch carrying a person's fix commit is never asked about,
// nor one whose commit list does not end at the head.
func TestStaleRedBotRebase_NeverOverSomeoneElsesCommits(t *testing.T) {
	t.Run("a person's commit on the branch", func(t *testing.T) {
		resetStaleRedRefresh(t)
		store := useSRBStore(t)
		f := newSRBRefresher(greenPytestBase("base1"))
		f.commits[775] = []github.PRCommit{
			{SHA: "c1", Author: "renovate[bot]", ParentCount: 1},
			{SHA: "fix", Author: "alice", ParentCount: 1},
			{SHA: "head1", Author: "renovate[bot]", ParentCount: 1},
		}
		now := time.Now()
		refreshStaleRedBehindPRs(context.Background(), escalationTestConfig(), f, f.track(srbBotPR(775, "head1")), nil, discardLogger(), now)
		if f.asks != 0 {
			t.Fatalf("asks = %d, want none over a person's commit", f.asks)
		}
		if _, ok := store.rebaseState("acme/widgets#775"); ok {
			t.Fatal("a request was recorded")
		}
		refreshStaleRedBehindPRs(context.Background(), escalationTestConfig(), f, f.track(srbBotPR(775, "head1")), nil, discardLogger(), now.Add(staleRedRefreshInterval))
		if f.asks != 0 || f.compares != 1 {
			t.Fatalf("asks %d, compares %d on the next pass; want the pair settled", f.asks, f.compares)
		}
	})
	t.Run("commit list does not end at the head", func(t *testing.T) {
		resetStaleRedRefresh(t)
		useSRBStore(t)
		f := newSRBRefresher(greenPytestBase("base1"))
		f.commits[775] = []github.PRCommit{{SHA: "older", Author: "renovate[bot]", ParentCount: 1}}
		refreshStaleRedBehindPRs(context.Background(), escalationTestConfig(), f, f.track(srbBotPR(775, "head1")), nil, discardLogger(), time.Now())
		if f.asks != 0 {
			t.Fatalf("asks = %d, want none when the branch facts are unknown", f.asks)
		}
	})
}

// A bump that really breaks a check: the bot rebased after the request and
// its new head fails the same check again. No second request (the base
// moving again changes nothing), no hold on the new head, and the red-CI
// hand-off fires on its usual schedule.
func TestStaleRedBotRebase_SameChecksAfterRebaseNotAskedAgain(t *testing.T) {
	resetStaleRedRefresh(t)
	store := useSRBStore(t)
	newTestEscalationStore(t) // the sweep reads the transient re-run ledger
	cfg := escalationTestConfig()
	f := newSRBRefresher(greenPytestBase("base1"))
	t0 := time.Date(2026, 11, 23, 11, 0, 0, 0, time.UTC)

	refreshStaleRedBehindPRs(context.Background(), cfg, f, f.track(srbBotPR(775, "head1")), nil, discardLogger(), t0)
	if f.asks != 1 {
		t.Fatalf("asks = %d, want the first request", f.asks)
	}

	// The bot rebased: head2, still red on pytest; main moves twice.
	f.untick(775)
	for i, base := range []string{"base2", "base3"} {
		f.bases = greenPytestBase(base)
		refreshStaleRedBehindPRs(context.Background(), cfg, f, f.track(srbBotPR(775, "head2")), nil, discardLogger(), t0.Add(time.Duration(i+1)*staleRedRefreshInterval))
	}
	if f.asks != 1 {
		t.Fatalf("asks = %d after the bot's new head failed the same check, want 1", f.asks)
	}

	writer := &depBotFakeWriter{}
	sweep := func(at time.Time) {
		a := &github.ActionableResult{}
		a.PRs.Items = []github.PullRequest{srbBotPR(775, "head2")}
		sweepDependencyBotConflicts(context.Background(), store, "acme", 0, writer, &depBotFakeReader{}, a, func() time.Time { return at }, discardLogger())
	}
	seen := t0.Add(time.Hour)
	sweep(seen)
	sweep(seen.Add(depBotRedCIGrace - time.Minute))
	if len(writer.comments) != 0 {
		t.Fatalf("before the red-CI grace: comments %v, want none", writer.comments)
	}
	sweep(seen.Add(depBotRedCIGrace))
	if len(writer.comments) != 1 {
		t.Fatalf("comments = %v, want the red-CI hand-off on schedule", writer.comments)
	}
	if rec, _ := store.get("acme/widgets#775"); rec.Reason != depBotHandoffRedCI || rec.HeadSHA != "head2" {
		t.Fatalf("hand-off = %+v, want red-ci on head2", rec)
	}
}

// After a request, the bot's new head red on a different check, or red
// again after a green head, may be asked about once more.
func TestStaleRedBotRebase_NewFailureAskedAgain(t *testing.T) {
	cfg := escalationTestConfig()
	t0 := time.Date(2026, 11, 23, 11, 0, 0, 0, time.UTC)

	t.Run("different failing check", func(t *testing.T) {
		resetStaleRedRefresh(t)
		useSRBStore(t)
		f := newSRBRefresher(map[string]github.StaleRedBaseState{"acme/widgets@main": {
			SHA: "base1", Completed: map[string]bool{"pytest": true, "lint": true}, Red: map[string]bool{},
		}})
		refreshStaleRedBehindPRs(context.Background(), cfg, f, f.track(srbBotPR(775, "head1")), nil, discardLogger(), t0)
		f.untick(775)
		f.bases = map[string]github.StaleRedBaseState{"acme/widgets@main": {
			SHA: "base2", Completed: map[string]bool{"pytest": true, "lint": true}, Red: map[string]bool{},
		}}
		lint := srbBotPR(775, "head2")
		lint.FailingChecks = []string{"lint"}
		refreshStaleRedBehindPRs(context.Background(), cfg, f, f.track(lint), nil, discardLogger(), t0.Add(staleRedRefreshInterval))
		if f.asks != 2 {
			t.Fatalf("asks = %d, want a second request for a different failing check", f.asks)
		}
	})
	t.Run("green then red", func(t *testing.T) {
		resetStaleRedRefresh(t)
		useSRBStore(t)
		f := newSRBRefresher(greenPytestBase("base1"))
		refreshStaleRedBehindPRs(context.Background(), cfg, f, f.track(srbBotPR(775, "head1")), nil, discardLogger(), t0)
		f.untick(775)
		green := srbBotPR(775, "head2")
		green.CIStatus, green.FailingChecks = "success", nil
		refreshStaleRedBehindPRs(context.Background(), cfg, f, f.track(green), nil, discardLogger(), t0.Add(staleRedRefreshInterval))
		f.bases = greenPytestBase("base2")
		refreshStaleRedBehindPRs(context.Background(), cfg, f, f.track(srbBotPR(775, "head3")), nil, discardLogger(), t0.Add(2*staleRedRefreshInterval))
		if f.asks != 2 {
			t.Fatalf("asks = %d, want a request again after a green head", f.asks)
		}
	})
}

// The PR-only trigger is bounded the same way: the bot's new head red on the
// same PR-only check is not asked about again, however often the base moves.
func TestStaleRedBotRebase_PROnlySameCheckAfterRebaseNotAskedAgain(t *testing.T) {
	resetStaleRedRefresh(t)
	useSRBStore(t)
	cfg := escalationTestConfig()
	window := cfg.DependencyBotRebaseAfter()
	ranAt := time.Date(2026, 11, 22, 15, 13, 0, 0, time.UTC)
	f := newSRBRefresher(prOnlyBase("base1"))
	f.tipAt = ranAt.Add(time.Hour)
	now := ranAt.Add(window + time.Hour)
	refreshStaleRedBehindPRs(context.Background(), cfg, f, f.track(srbPROnlyPR(1471, "head1", ranAt)), nil, discardLogger(), now)

	// The bot rebased; contract failed again on head2, and main moved on.
	f.untick(1471)
	ranAgain := now.Add(10 * time.Minute)
	f.bases = prOnlyBase("base2")
	f.tipAt = ranAgain.Add(time.Hour)
	refreshStaleRedBehindPRs(context.Background(), cfg, f, f.track(srbPROnlyPR(1471, "head2", ranAgain)), nil, discardLogger(), ranAgain.Add(window+2*time.Hour))
	if f.asks != 1 {
		t.Fatalf("asks = %d, want one request for the PR-only check across the bot's heads", f.asks)
	}
}

// Bot requests count against the per-repository cap.
func TestStaleRedBotRebase_PerRepoCap(t *testing.T) {
	resetStaleRedRefresh(t)
	useSRBStore(t)
	var prs []github.PullRequest
	for i := 1; i <= staleRedRefreshMaxPerRepo+2; i++ {
		prs = append(prs, srbBotPR(i, "h"))
	}
	f := newSRBRefresher(greenPytestBase("base1"))
	refreshStaleRedBehindPRs(context.Background(), escalationTestConfig(), f, f.track(prs...), nil, discardLogger(), time.Now())
	if len(f.edits) != staleRedRefreshMaxPerRepo {
		t.Fatalf("requests = %d, want the per-repo cap %d", len(f.edits), staleRedRefreshMaxPerRepo)
	}
}

// Part 2: a check that runs only on pull requests failed more than a window
// ago, and the base tip was committed after that run. One request per head:
// a second base move on the same head asks nothing.
func TestStaleRedBotRebase_PROnlyCheckOncePerHead(t *testing.T) {
	resetStaleRedRefresh(t)
	store := useSRBStore(t)
	cfg := escalationTestConfig()
	window := cfg.DependencyBotRebaseAfter()
	ranAt := time.Date(2026, 11, 22, 15, 13, 0, 0, time.UTC)
	f := newSRBRefresher(prOnlyBase("base1"))
	f.tipAt = ranAt.Add(time.Hour)
	now := ranAt.Add(window + time.Hour)

	refreshStaleRedBehindPRs(context.Background(), cfg, f, f.track(srbPROnlyPR(1471, "head1", ranAt)), nil, discardLogger(), now)
	if len(f.edits) != 1 || len(f.calls) != 0 {
		t.Fatalf("edits %v, updates %d; want one request and no update", f.edits, len(f.calls))
	}
	st, _ := store.rebaseState("acme/widgets#1471")
	if st.Trigger != staleRedBotTriggerPROnly || len(st.Checks) != 1 || st.Checks[0] != "contract" {
		t.Fatalf("recorded request = %+v, want a pr-only-red request for contract", st)
	}

	// The base moves again; same head: no second request.
	f.untick(1471)
	f.bases = prOnlyBase("base2")
	f.tipAt = ranAt.Add(3 * time.Hour)
	refreshStaleRedBehindPRs(context.Background(), cfg, f, f.track(srbPROnlyPR(1471, "head1", ranAt)), nil, discardLogger(), now.Add(staleRedRefreshInterval))
	if f.asks != 1 {
		t.Fatalf("asks = %d after a second base move on the same head, want 1", f.asks)
	}
}

// Part 2 waits: no request while the failing run is younger than the window,
// or when the base tip predates the run (the base has not moved).
func TestStaleRedBotRebase_PROnlyCheckWaits(t *testing.T) {
	cfg := escalationTestConfig()
	window := cfg.DependencyBotRebaseAfter()
	ranAt := time.Date(2026, 11, 22, 15, 13, 0, 0, time.UTC)

	t.Run("run younger than the window", func(t *testing.T) {
		resetStaleRedRefresh(t)
		useSRBStore(t)
		f := newSRBRefresher(prOnlyBase("base1"))
		f.tipAt = ranAt.Add(time.Minute)
		refreshStaleRedBehindPRs(context.Background(), cfg, f, f.track(srbPROnlyPR(1471, "head1", ranAt)), nil, discardLogger(), ranAt.Add(window-time.Minute))
		if f.asks != 0 || f.tipReads != 0 {
			t.Fatalf("asks %d, tip reads %d; want nothing before the window", f.asks, f.tipReads)
		}
	})
	t.Run("base unchanged since the run", func(t *testing.T) {
		resetStaleRedRefresh(t)
		useSRBStore(t)
		f := newSRBRefresher(prOnlyBase("base1"))
		f.tipAt = ranAt.Add(-time.Hour)
		refreshStaleRedBehindPRs(context.Background(), cfg, f, f.track(srbPROnlyPR(1471, "head1", ranAt)), nil, discardLogger(), ranAt.Add(window+time.Hour))
		if f.asks != 0 {
			t.Fatalf("asks = %d, want none when the base tip predates the failing run", f.asks)
		}
	})
	t.Run("check still running on the base tip", func(t *testing.T) {
		resetStaleRedRefresh(t)
		useSRBStore(t)
		base := prOnlyBase("base1")
		st := base["acme/widgets@main"]
		st.Reported["contract"] = true
		base["acme/widgets@main"] = st
		f := newSRBRefresher(base)
		f.tipAt = ranAt.Add(time.Hour)
		refreshStaleRedBehindPRs(context.Background(), cfg, f, f.track(srbPROnlyPR(1471, "head1", ranAt)), nil, discardLogger(), ranAt.Add(window+time.Hour))
		if f.asks != 0 {
			t.Fatalf("asks = %d, want none while the base tip still runs the check", f.asks)
		}
	})
}

// Part 3: a head that neither moves nor goes green one window after the
// stale-red request reaches a person through the dependency-bot sweep's
// hand-off (one comment plus needs-human); the label comes off once the bot
// pushes a new head. Before the window the request holds the sweep's other
// hand-offs.
func TestStaleRedBotRebase_UnansweredRequestHandsOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dependency-bot-conflicts.json")
	newTestEscalationStore(t) // the sweep reads the transient re-run ledger
	store := loadDepBotConflictStore(path)
	asked := time.Date(2026, 11, 23, 11, 0, 0, 0, time.UTC)
	store.markStaleRedRebaseRequested("acme/widgets#775", "head1", "base1", staleRedBotTriggerBaseClears, depBotViaRenovateCheckbox, []string{"pytest"}, asked)
	writer := &depBotFakeWriter{}
	reader := &depBotFakeReader{}
	pass := func(at time.Time, pr github.PullRequest) {
		a := &github.ActionableResult{}
		a.PRs.Items = []github.PullRequest{pr}
		sweepDependencyBotConflicts(context.Background(), store, "acme", 0, writer, reader, a, func() time.Time { return at }, discardLogger())
	}

	pass(asked.Add(depBotRebaseWindow-time.Minute), srbBotPR(775, "head1"))
	if len(writer.comments) != 0 || len(writer.adds) != 0 {
		t.Fatalf("inside the window: comments %v, labels %v; want none", writer.comments, writer.adds)
	}

	pass(asked.Add(depBotRebaseWindow+time.Minute), srbBotPR(775, "head1"))
	if len(writer.comments) != 1 || len(writer.adds) != 1 {
		t.Fatalf("after the window: comments %v, labels %v; want one hand-off", writer.comments, writer.adds)
	}
	if c := writer.comments[0]; !strings.Contains(c, "`pytest`, which passes on `main` now") || !strings.Contains(c, "Hive ticked the rebase/retry box 4 hours ago") {
		t.Fatalf("hand-off comment = %q", c)
	}
	if strings.ContainsAny(writer.comments[0], "—–") {
		t.Fatalf("hand-off comment has an em or en dash: %q", writer.comments[0])
	}
	if rec, _ := store.get("acme/widgets#775"); rec.Reason != depBotHandoffStaleRed {
		t.Fatalf("hand-off reason = %q, want %q", rec.Reason, depBotHandoffStaleRed)
	}

	// The same head on the next pass: nothing more.
	labelled := srbBotPR(775, "head1")
	labelled.Labels = []string{"needs-human"}
	pass(asked.Add(depBotRebaseWindow+10*time.Minute), labelled)
	if len(writer.comments) != 1 || len(writer.removes) != 0 {
		t.Fatalf("same head again: comments %v, removes %v", writer.comments, writer.removes)
	}

	// The bot rebased: a new head; the label this sweep added comes off.
	moved := srbBotPR(775, "head2")
	moved.Labels = []string{"needs-human"}
	pass(asked.Add(depBotRebaseWindow+20*time.Minute), moved)
	if len(writer.removes) != 1 {
		t.Fatalf("after a new head: removes %v, want needs-human taken off", writer.removes)
	}
}

// A head that went green is not handed off for the stale-red request; the
// regular stranded check decides for it.
func TestStaleRedBotRebase_GreenHeadNotHandedOff(t *testing.T) {
	store := loadDepBotConflictStore("")
	asked := time.Date(2026, 11, 23, 11, 0, 0, 0, time.UTC)
	store.markStaleRedRebaseRequested("acme/widgets#775", "head1", "base1", staleRedBotTriggerBaseClears, depBotViaRenovateCheckbox, []string{"pytest"}, asked)
	c := depBotRebaseCheck{store: store, reader: &depBotFakeReader{}, window: depBotRebaseWindow, now: asked.Add(depBotRebaseWindow + time.Minute), logger: discardLogger()}
	green := srbBotPR(775, "head1")
	green.CIStatus = "success"
	if got, ok := c.staleRedStep("acme/widgets#775", green); ok || got != depBotStrandedNone {
		t.Fatalf("staleRedStep = %v, %v; want none, left to the regular stranded check, for a head that went green", got, ok)
	}
	if got, ok := c.staleRedStep("acme/widgets#775", srbBotPR(775, "head1")); !ok || got != depBotStrandedStaleRedHandoff {
		t.Fatalf("staleRedStep = %v, %v; want the stale-red hand-off for a red head", got, ok)
	}
	if _, ok := c.staleRedStep("acme/widgets#775", srbBotPR(775, "head2")); ok {
		t.Fatal("staleRedStep claimed a head it never asked about")
	}
}

// srbRedCIHandoff records, in the sweep's ledger, a hand-off for reason on
// head with the needs-human label added by the sweep (labelApplied), and
// returns the bot PR labelled needs-human as the next enumeration shows it.
func srbRedCIHandoff(store *depBotConflictStore, number int, head, reason string, labelApplied bool, at time.Time) github.PullRequest {
	key := "acme/widgets#" + strconv.Itoa(number)
	store.markCommented(key, head, reason, at)
	store.setLabelApplied(key, labelApplied)
	pr := srbBotPR(number, head)
	pr.Labels = []string{"needs-human"}
	return pr
}

// The dependency-bot sweep's own red-CI hand-off parked the bot PR with
// needs-human after 4 hours red on head1 (projectbluefin/fsdk-containers#570
// in miniature). Once the base tip clears the failing check, the stale-red
// pass asks the bot to rebase once, with the label still on, and not again
// at the same base tip, also after a restart forgets the pass's memo.
func TestStaleRedBotRebase_OwnRedCIHandoffAskedOnceBaseClears(t *testing.T) {
	resetStaleRedRefresh(t)
	store := useSRBStore(t)
	newTestEscalationStore(t) // the sweep reads the transient re-run ledger
	cfg := escalationTestConfig()
	seen := time.Date(2026, 11, 22, 7, 0, 0, 0, time.UTC)

	// The real sweep hands the red head off: one comment, needs-human.
	writer := &depBotFakeWriter{}
	sweep := func(at time.Time) {
		a := &github.ActionableResult{}
		a.PRs.Items = []github.PullRequest{srbBotPR(570, "head1")}
		sweepDependencyBotConflicts(context.Background(), store, "acme", 0, writer, &depBotFakeReader{}, a, func() time.Time { return at }, discardLogger())
	}
	sweep(seen)
	sweep(seen.Add(depBotRedCIGrace))
	if len(writer.comments) != 1 || len(writer.adds) != 1 {
		t.Fatalf("comments %v, labels %v; want the red-CI hand-off", writer.comments, writer.adds)
	}
	labelled := srbBotPR(570, "head1")
	labelled.Labels = []string{"needs-human"}
	if !staleRedOwnRedCIHandoff(store, "acme/widgets#570", labelled) {
		rec, _ := store.get("acme/widgets#570")
		t.Fatalf("ledger record %+v does not prove the sweep's own red-CI label on head1", rec)
	}

	f := newSRBRefresher(greenPytestBase("base1"))
	now := seen.Add(depBotRedCIGrace + time.Hour)
	got := refreshStaleRedBehindPRs(context.Background(), cfg, f, f.track(labelled), nil, discardLogger(), now)
	if f.asks != 1 || len(f.edits) != 1 || len(f.calls) != 0 || !got["acme/widgets#570"] {
		t.Fatalf("asks %d, edits %v, updates %d, keys %v; want one request and no update", f.asks, f.edits, len(f.calls), got)
	}
	if st, ok := store.rebaseState("acme/widgets#570"); !ok || st.HeadSHA != "head1" || st.BaseSHA != "base1" || st.Trigger != staleRedBotTriggerBaseClears {
		t.Fatalf("recorded request = %+v (found %v), want head1/base1 stale-red", st, ok)
	}

	// Same head, same base tip: nothing, from the pass's memo ...
	refreshStaleRedBehindPRs(context.Background(), cfg, f, f.track(labelled), nil, discardLogger(), now.Add(staleRedRefreshInterval))
	// ... and from the ledger once the memo is gone (a restart).
	resetStaleRedRefresh(t)
	refreshStaleRedBehindPRs(context.Background(), cfg, f, f.track(labelled), nil, discardLogger(), now.Add(2*staleRedRefreshInterval))
	if f.asks != 1 || len(f.edits) != 1 {
		t.Fatalf("asks %d, edits %v at the same head and base tip; want still one", f.asks, f.edits)
	}

	// The bot rebased; head2 is red on the same check and the label stays
	// (it comes off only on green). The record names head1: not asked.
	f.untick(570)
	f.bases = greenPytestBase("base2")
	moved := srbBotPR(570, "head2")
	moved.Labels = []string{"needs-human"}
	refreshStaleRedBehindPRs(context.Background(), cfg, f, f.track(moved), nil, discardLogger(), now.Add(3*staleRedRefreshInterval))
	if f.asks != 1 {
		t.Fatalf("asks = %d after the bot's new head, want 1", f.asks)
	}
}

// Every other needs-human still blocks the request, with no API call: a
// person's (no record, or a red-CI record whose label the sweep never
// added), another hand-off's, a red-CI record for an older head, and the
// sweep's own red-CI label next to a hold.
func TestStaleRedBotRebase_OtherNeedsHumanStillBlocks(t *testing.T) {
	at := time.Date(2026, 11, 22, 7, 0, 0, 0, time.UTC)
	cases := map[string]func(store *depBotConflictStore) github.PullRequest{
		"added by a person": func(*depBotConflictStore) github.PullRequest {
			pr := srbBotPR(570, "head1")
			pr.Labels = []string{"needs-human"}
			return pr
		},
		"red-ci record, label not the sweep's": func(store *depBotConflictStore) github.PullRequest {
			return srbRedCIHandoff(store, 570, "head1", depBotHandoffRedCI, false, at)
		},
		"conflict hand-off": func(store *depBotConflictStore) github.PullRequest {
			return srbRedCIHandoff(store, 570, "head1", depBotHandoffConflict, true, at)
		},
		"check-held hand-off": func(store *depBotConflictStore) github.PullRequest {
			return srbRedCIHandoff(store, 570, "head1", depBotHandoffHeld, true, at)
		},
		"stranded hand-off": func(store *depBotConflictStore) github.PullRequest {
			return srbRedCIHandoff(store, 570, "head1", depBotHandoffStranded, true, at)
		},
		"stale-red hand-off": func(store *depBotConflictStore) github.PullRequest {
			return srbRedCIHandoff(store, 570, "head1", depBotHandoffStaleRed, true, at)
		},
		"red-ci record for an older head": func(store *depBotConflictStore) github.PullRequest {
			pr := srbRedCIHandoff(store, 570, "head0", depBotHandoffRedCI, true, at)
			pr.HeadSHA = "head1"
			return pr
		},
		"own red-ci label plus a hold": func(store *depBotConflictStore) github.PullRequest {
			pr := srbRedCIHandoff(store, 570, "head1", depBotHandoffRedCI, true, at)
			pr.Labels = append(pr.Labels, "hold")
			return pr
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			resetStaleRedRefresh(t)
			store := useSRBStore(t)
			pr := mk(store)
			f := newSRBRefresher(greenPytestBase("base1"))
			got := refreshStaleRedBehindPRs(context.Background(), escalationTestConfig(), f, f.track(pr), nil, discardLogger(), at.Add(5*time.Hour))
			if f.asks != 0 || len(f.calls) != 0 || len(got) != 0 {
				t.Fatalf("asks %d, updates %d, keys %v; want nothing", f.asks, len(f.calls), got)
			}
			if len(f.baseReads) != 0 || f.compares != 0 {
				t.Fatalf("base reads %v, compares %d; want no API call", f.baseReads, f.compares)
			}
		})
	}
}

// The sweep's own red-CI label is no barrier only when the base tip clears
// every failing check: not when the check is red on the base too, and not
// for a PR-only check (which an unlabelled PR would be asked about).
func TestStaleRedBotRebase_OwnRedCIHandoffWaitsForBaseToClear(t *testing.T) {
	cfg := escalationTestConfig()
	window := cfg.DependencyBotRebaseAfter()
	at := time.Date(2026, 11, 22, 7, 0, 0, 0, time.UTC)

	t.Run("red on the base tip", func(t *testing.T) {
		resetStaleRedRefresh(t)
		store := useSRBStore(t)
		pr := srbRedCIHandoff(store, 570, "head1", depBotHandoffRedCI, true, at)
		f := newSRBRefresher(map[string]github.StaleRedBaseState{"acme/widgets@main": {
			SHA: "base1", Completed: map[string]bool{"pytest": true}, Red: map[string]bool{"pytest": true},
		}})
		refreshStaleRedBehindPRs(context.Background(), cfg, f, f.track(pr), nil, discardLogger(), at.Add(5*time.Hour))
		if f.asks != 0 || f.compares != 0 {
			t.Fatalf("asks %d, compares %d; want nothing while the base is red too", f.asks, f.compares)
		}
	})
	t.Run("check still running on the base tip", func(t *testing.T) {
		resetStaleRedRefresh(t)
		store := useSRBStore(t)
		pr := srbRedCIHandoff(store, 570, "head1", depBotHandoffRedCI, true, at)
		f := newSRBRefresher(map[string]github.StaleRedBaseState{"acme/widgets@main": {
			SHA: "base1", Completed: map[string]bool{}, Red: map[string]bool{}, Reported: map[string]bool{"pytest": true},
		}})
		refreshStaleRedBehindPRs(context.Background(), cfg, f, f.track(pr), nil, discardLogger(), at.Add(5*time.Hour))
		if f.asks != 0 || f.compares != 0 {
			t.Fatalf("asks %d, compares %d; want nothing before the base tip clears", f.asks, f.compares)
		}
	})
	t.Run("pr-only check", func(t *testing.T) {
		ranAt := at.Add(-time.Hour)
		now := ranAt.Add(window + time.Hour)

		resetStaleRedRefresh(t)
		store := useSRBStore(t)
		srbRedCIHandoff(store, 1471, "head1", depBotHandoffRedCI, true, at)
		pr := srbPROnlyPR(1471, "head1", ranAt)
		pr.Labels = []string{"needs-human"}
		f := newSRBRefresher(prOnlyBase("base1"))
		f.tipAt = ranAt.Add(time.Hour)
		refreshStaleRedBehindPRs(context.Background(), cfg, f, f.track(pr), nil, discardLogger(), now)
		if f.asks != 0 || f.tipReads != 0 {
			t.Fatalf("asks %d, tip reads %d; want nothing for a PR-only check under the hand-off", f.asks, f.tipReads)
		}

		// The same PR without the label is asked about (#19526 unchanged).
		resetStaleRedRefresh(t)
		useSRBStore(t)
		f = newSRBRefresher(prOnlyBase("base1"))
		f.tipAt = ranAt.Add(time.Hour)
		refreshStaleRedBehindPRs(context.Background(), cfg, f, f.track(srbPROnlyPR(1471, "head1", ranAt)), nil, discardLogger(), now)
		if f.asks != 1 {
			t.Fatalf("asks = %d without the label, want 1", f.asks)
		}
	})
}

// Dependabot is never asked, also when the sweep's own red-CI hand-off
// parked it: no base read, no request.
func TestStaleRedBotRebase_DependabotOwnRedCIHandoffUnchanged(t *testing.T) {
	resetStaleRedRefresh(t)
	store := useSRBStore(t)
	at := time.Date(2026, 11, 22, 7, 0, 0, 0, time.UTC)
	pr := srbRedCIHandoff(store, 1, "dep1", depBotHandoffRedCI, true, at)
	pr.Author = "dependabot[bot]"
	pr.HeadRef = "dependabot/npm_and_yarn/lodash-4.17.21"
	f := newSRBRefresher(greenPytestBase("base1"))
	refreshStaleRedBehindPRs(context.Background(), escalationTestConfig(), f, f.track(pr), nil, discardLogger(), at.Add(5*time.Hour))
	if f.asks != 0 || len(f.calls) != 0 || len(f.baseReads) != 0 {
		t.Fatalf("Dependabot PR: asks %d, updates %d, base reads %v; want none", f.asks, len(f.calls), f.baseReads)
	}
}
