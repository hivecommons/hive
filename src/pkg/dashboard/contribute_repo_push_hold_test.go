package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// A contributor whose fork of a repository is far behind upstream, with a
// token that lacks the `workflow` scope, cannot push any branch based on
// current upstream: GitHub rejects it. The hub kept handing that contributor
// tasks in the repositories they could not push to, ten minutes of agent time
// each, while their tasks in a repository with a fresh fork shipped. These
// tests pin the per-contributor, per-repository push hold that stops it.

// The three reasons below are the hub's own log lines for the wasted tasks,
// with the names generalised.
const (
	pushHoldReasonFailed = "agent reported an environment failure: environment failure: push to fork user/repo rejected for missing workflow OAuth scope while fork main (6c34b7f) lags upstream/main (19d0c85) across workflow commits and fork sync is disallowed by gh-wrapper.sh; no PR could be opened"
	pushHoldReasonIdle   = "agent reported complete without a pull request: local fix committed on fix/x (6b0a67d) but push to fork user/repo failed because GH_TOKEN lacks workflow scope and fork sync is disallowed by the gh wrapper"
	pushHoldReasonBehind = "agent reported complete without a pull request: environment push failure: push to fork user/repo rejected because OAuth token lacks workflow scope while fork is 281 commits behind upstream/main"
)

func TestContributorForkPushFailure_Classifier(t *testing.T) {
	const idle = "agent reported complete without a pull request: "
	cases := []struct {
		name   string
		reason string
		repo   string
		want   bool
	}{
		{"failed, environment", pushHoldReasonFailed, "myorg/repo", true},
		{"idle, push failed", pushHoldReasonIdle, "myorg/repo", true},
		{"idle, fork behind", pushHoldReasonBehind, "myorg/repo", true},
		{"blocked verdict of hive 11776", toolingBlockedReason, "myorg/repo", true},
		{"github refusal text", "git push rejected: refusing to allow an OAuth App to create or update workflow .github/workflows/ci.yml without workflow scope", "myorg/repo", true},
		{"wrapped over lines", "push to fork user/repo\nrejected: token lacks\nworkflow scope", "myorg/repo", true},
		// One clause saying the push to the fork was refused is enough
		// without the scope being named.
		{"refused clause, no scope", idle + "the push to my fork was rejected by GitHub", "myorg/repo", true},
		{"could not push clause", idle + "could not push fix/x to my fork", "myorg/repo", true},
		{"fork refused the push", idle + "my fork rejected the push", "myorg/repo", true},
		{"named fork, any owner, same repository, other case", "could not push to fork someone/Repo.git: rejected", "myorg/repo", true},
		// A git ref or "and/or" after the word fork names no fork.
		{"a ref is not a fork name", "push to fork origin/main rejected: token lacks workflow scope", "myorg/repo", true},
		{"upstream ref is not a fork name", "could not push to my fork: token lacks workflow scope and the fork upstream/main is behind", "myorg/repo", true},
		{"and/or is not a fork name", "the push to my fork was rejected, on the fork and/or upstream the token lacks workflow scope", "myorg/repo", true},

		{"empty", "", "myorg/repo", false},
		{"blocked on another repository", blockedReason, "myorg/repo", false},
		{"waiting on a release", "waiting on myorg/packages to publish a build with the recipes", "myorg/repo", false},
		{"ci failure", "CI failed after the push: the lint job is red and the unit tests time out", "myorg/repo", false},
		{"merge conflict", "rebase onto upstream/main hit a merge conflict in pkg/dashboard/api.go that could not be resolved", "myorg/repo", false},
		// The upstream repository refusing a direct push is the pull request
		// path doing its job, not a fork the contributor has to repair.
		{"upstream branch protection", "could not push to the upstream repository org/repo: branch protection requires a pull request", "myorg/repo", false},
		{"upstream branch protection, fork named", "push to org/repo main rejected by branch protection; the fork is in sync", "myorg/repo", false},
		// A stale fork is the cause, not the failure: without a refused push
		// the reason does not say a pull request was impossible.
		{"stale fork alone", "the fork's v6 is behind upstream and the gh wrapper refuses to sync it", "myorg/repo", false},
		{"other tooling failure", "git push failed with HTTP 403", "myorg/repo", false},
		{"the issue is about fork syncing", "investigated why fork sync is refused for contributor pull requests; the design needs a maintainer", "myorg/repo", false},
		// A failed push and the word fork in different clauses do not add
		// up: these contributors can push.
		{"chose not to push", idle + "tests failed locally so I did not push to the fork", "myorg/repo", false},
		{"nothing to push", idle + "could not reproduce; nothing to push. Fork is up to date.", "myorg/repo", false},
		{"pushed, then something else failed", idle + "implemented the fix and pushed fix/x to my fork, but gh pr create failed with 'No commits between v6 and fix/x'", "myorg/repo", false},
		{"ci failed after a push that worked", "CI failed after the push to my fork: the lint job is red", "myorg/repo", false},
		// The fork of another repository says nothing about this one.
		{"another repository's fork", "could not push to fork otheruser/otherrepo (a dependency repo I tried to patch): rejected", "myorg/repo", false},
		{"real reason, another repository's task", pushHoldReasonFailed, "myorg/elsewhere", false},
		// The workflow scope merely being mentioned is not a refused push.
		{"scope documented", idle + "already documented: the workflow scope requirement is in contributor-relay.md", "myorg/repo", false},
		{"scope present", idle + "nothing to change: the token already has the workflow scope and the fork is in sync", "myorg/repo", false},
		{"scope explained in docs", idle + "updated the docs to say gh auth refresh -s workflow adds the workflow scope; no code change needed", "myorg/repo", false},
		// Nor is a push that worked, beside something else that failed and
		// a mention of the scope.
		{"pushed, another step failed, scope mentioned", idle + "pushed the fix to my fork; the docs test failed because the workflow scope note was missing", "myorg/repo", false},
		{"scope docs fixed, pull request creation failed", idle + "fixed the workflow scope docs and pushed to my fork but the PR creation failed with a validation error", "myorg/repo", false},
		// "unable to push" somewhere else must not reach a later push that worked.
		{"a later push worked", idle + "unable to push directly to main so I pushed to my fork and opened the PR manually later", "myorg/repo", false},
	}
	for _, tc := range cases {
		if got := isContributorForkPushFailure(tc.reason, tc.repo); got != tc.want {
			t.Errorf("%s: isContributorForkPushFailure(%q, %q) = %v, want %v", tc.name, tc.reason, tc.repo, got, tc.want)
		}
	}
}

// TestContributorForkPushFailure_Evidence: a failed push with workflow-scope
// evidence is the fault the hold exists for; a bare "could not push to my
// fork" is not something text alone can tell from a contributor who simply
// did not push, so it is the weaker kind.
func TestContributorForkPushFailure_Evidence(t *testing.T) {
	for _, reason := range []string{pushHoldReasonFailed, pushHoldReasonIdle, pushHoldReasonBehind, toolingBlockedReason,
		"git push rejected: refusing to allow an OAuth App to create or update workflow .github/workflows/ci.yml without workflow scope",
		"git push failed: refusing to allow a Personal Access Token to create or update workflow `.github/workflows/build.yml` without `workflow` scope",
		"could not push to my fork: token lacks workflow scope",
		"push to fork user/repo\nrejected: token lacks\nworkflow scope"} {
		if got := classifyContributorForkPushFailure(reason, "myorg/repo"); got != forkPushScopeBacked {
			t.Errorf("want scope-backed evidence, got %d for %q", got, reason)
		}
	}
	for _, reason := range []string{
		// The scope named before the refusal, not behind it, is the weaker kind.
		"the token lacks workflow scope so the push to my fork was rejected",
		"tests failed so I could not push to my fork yet",
		"I was unable to push to the fork because I ran out of time",
		"cannot push to the fork until the maintainer decides on the approach",
		`the issue says "push to my fork was rejected" but I could not reproduce it`,
		"push to my fork failed the pre-push hook because gofmt was not clean",
		"could not push to my fork of the packages repo: rejected",
	} {
		if got := classifyContributorForkPushFailure(reason, "myorg/repo"); got != forkPushClauseOnly {
			t.Errorf("want clause-only evidence (a strike, not a hold), got %d for %q", got, reason)
		}
	}
}

// pushHoldReasonNoScope says a push to the fork was refused and nothing
// about why.
const pushHoldReasonNoScope = "agent reported complete without a pull request: the push to my fork was rejected"

// TestRepoPushHold_NoScopeEvidenceTakesTwoReports: a refused push with no
// workflow-scope evidence books nothing the first time, and the hold the
// second time within 24 hours. A later second report, or one after a
// verified pull request, is a first report again. Scope-backed evidence
// books at once.
func TestRepoPushHold_NoScopeEvidenceTakesTwoReports(t *testing.T) {
	hub, s := covK2Hub(t)
	pushHoldRepos(s, []int{261}, nil)
	c := streakConn("ct-strike")
	id := contributorFlapIdentity(c)
	const repo = "myorg/repo"
	task := pushHoldTask(repo, 260)

	// The first report, through the entry point both handlers use.
	if hub.holdRepoIfForkPushFailed(c, task, pushHoldReasonNoScope) {
		t.Fatal("one report with no scope evidence must not book a hold")
	}
	if _, active := hub.contributorRepoPushHoldActive(id, repo, time.Now()); active {
		t.Fatal("one report with no scope evidence must leave the contributor unheld")
	}
	if got := hub.RepoPushHolds(time.Now()); len(got) != 0 {
		t.Fatalf("a strike is not a hold and must not be listed, got %+v", got)
	}
	if msg := hub.selectTask(c); msg == nil || msg.Type != "task_assign" || msg.Repo != repo {
		t.Fatalf("after one report the contributor must still be offered the repository, got %+v", msg)
	}
	// The second report inside the window books the first hold.
	if !hub.holdRepoIfForkPushFailed(c, task, pushHoldReasonNoScope) {
		t.Fatal("a second report within 24 hours must book the hold")
	}
	hold, active := hub.contributorRepoPushHoldActive(id, repo, time.Now())
	if !active || hold.Count != 1 || hold.Until.Sub(hold.HeldAt) != 6*time.Hour || !hold.StrikeAt.IsZero() {
		t.Fatalf("want a first hold of 6h with the strike spent, got active=%v %+v", active, hold)
	}

	// With an injected clock: a second report after the window is a first
	// report again.
	late := streakConn("ct-strike-late")
	t0 := time.Now()
	if _, outcome := hub.recordContributorRepoPushFailure(late, repo, pushHoldReasonNoScope, false, t0); outcome != repoPushStrike {
		t.Fatalf("first report: outcome %d, want a strike", outcome)
	}
	t1 := t0.Add(contributorRepoPushStrikeWindow)
	if _, outcome := hub.recordContributorRepoPushFailure(late, repo, pushHoldReasonNoScope, false, t1); outcome != repoPushStrike {
		t.Fatalf("a report 24 hours after the first: outcome %d, want a new first strike", outcome)
	}
	if rec, outcome := hub.recordContributorRepoPushFailure(late, repo, pushHoldReasonNoScope, false, t1.Add(23*time.Hour)); outcome != repoPushHoldBookedOnSecond || rec.Count != 1 {
		t.Fatalf("a report 23 hours after a strike: outcome %d %+v, want the hold", outcome, rec)
	}

	// A verified pull request in between clears the strike.
	fixed := streakConn("ct-strike-fixed")
	hub.recordContributorRepoPushFailure(fixed, repo, pushHoldReasonNoScope, false, t0)
	if !hub.clearContributorRepoPushHold(contributorFlapIdentity(fixed), repo) {
		t.Fatal("a verified pull request must clear a strike as it clears a hold")
	}
	if _, outcome := hub.recordContributorRepoPushFailure(fixed, repo, pushHoldReasonNoScope, false, t0.Add(time.Hour)); outcome != repoPushStrike {
		t.Fatalf("after a verified pull request the next report: outcome %d, want a first strike", outcome)
	}

	// Scope-backed evidence books at once, strike or no strike.
	scoped := streakConn("ct-strike-scoped")
	if !hub.holdRepoIfForkPushFailed(scoped, task, pushHoldReasonFailed) {
		t.Fatal("a failed push with workflow-scope evidence must book the hold at once")
	}
	// Each repository and each contributor has its own strike.
	if _, outcome := hub.recordContributorRepoPushFailure(fixed, "myorg/other", pushHoldReasonNoScope, false, t0.Add(2*time.Hour)); outcome != repoPushStrike {
		t.Fatalf("a strike in one repository must not count in another: outcome %d", outcome)
	}
}

// TestRepoPushHold_StrikeSurvivesRestart: the strike is kept in the same
// ledger entry, so the second report after a restart still books the hold; a
// strike older than its window is not carried over.
func TestRepoPushHold_StrikeSurvivesRestart(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	c := streakConn("ct-strike-restart")
	old := streakConn("ct-strike-old")
	now := time.Now()

	hub1, _ := covK2Hub(t)
	hub1.recordContributorRepoPushFailure(c, "myorg/repo", pushHoldReasonNoScope, false, now.Add(-time.Hour))
	hub1.recordContributorRepoPushFailure(old, "myorg/repo", pushHoldReasonNoScope, false, now.Add(-(contributorRepoPushStrikeWindow + time.Hour)))

	hub2, _ := covK2Hub(t)
	hub2.pushHoldMu.Lock()
	_, stale := hub2.repoPushHolds[contributorRepoPushHoldKey(contributorFlapIdentity(old), "myorg/repo")]
	hub2.pushHoldMu.Unlock()
	if stale {
		t.Fatal("a strike older than its window must not be carried over a restart")
	}
	if rec, outcome := hub2.recordContributorRepoPushFailure(c, "myorg/repo", pushHoldReasonNoScope, false, now); outcome != repoPushHoldBookedOnSecond || rec.Count != 1 {
		t.Fatalf("the second report after a restart: outcome %d %+v, want the hold", outcome, rec)
	}
}

func TestContributorRepoPushHoldDuration(t *testing.T) {
	for count, want := range map[int]time.Duration{
		1: 6 * time.Hour, 2: 24 * time.Hour, 3: 72 * time.Hour, 4: 72 * time.Hour, 40: 72 * time.Hour,
	} {
		if got := contributorRepoPushHoldDuration(count); got != want {
			t.Errorf("hold %d lasts %v, want %v", count, got, want)
		}
	}
}

// pushHoldRepos installs a status with two repositories: myorg/repo, the one
// the contributor cannot push to (the real reasons name the fork user/repo),
// and myorg/fresh, one they can.
func pushHoldRepos(s *Server, stale, fresh []int) {
	issues := func(repo string, numbers []int) []any {
		out := make([]any, 0, len(numbers))
		for _, n := range numbers {
			out = append(out, map[string]any{
				"number": float64(n),
				"title":  "push hold test issue",
				"url":    "https://github.com/" + repo + "/issues/" + itoa(n),
				"author": "someone",
			})
		}
		return out
	}
	s.statusMu.Lock()
	s.status = &StatusPayload{Repos: []FrontendRepo{
		{Name: "repo", Full: "myorg/repo", ActionableIssues: issues("myorg/repo", stale)},
		{Name: "fresh", Full: "myorg/fresh", ActionableIssues: issues("myorg/fresh", fresh)},
	}}
	s.statusMu.Unlock()
}

func pushHoldTask(repo string, number int) *WSTaskAssign {
	return &WSTaskAssign{TaskID: "ct-" + repo + "-" + itoa(number), Kind: "issue", Repo: repo, Number: number}
}

// TestRepoPushHold_HeldRepoSkippedForThatContributorOnly: a classified failure
// books a six hour hold; the held contributor is offered the other
// repository's work, and another contributor is offered the held repository's
// issue.
func TestRepoPushHold_HeldRepoSkippedForThatContributorOnly(t *testing.T) {
	hub, s := covK2Hub(t)
	pushHoldRepos(s, []int{201}, []int{202})
	held := streakConn("ct-held")
	other := streakConn("ct-other")

	if hub.holdRepoIfForkPushFailed(held, pushHoldTask("myorg/repo", 200), blockedReason) {
		t.Fatal("a reason that is not a fork push failure must not book a hold")
	}
	if !hub.holdRepoIfForkPushFailed(held, pushHoldTask("myorg/repo", 200), pushHoldReasonFailed) {
		t.Fatal("a fork push failure must book a hold")
	}
	hold, active := hub.contributorRepoPushHoldActive(contributorFlapIdentity(held), "myorg/repo", time.Now())
	if !active || hold.Count != 1 || hold.Until.Sub(hold.HeldAt) != 6*time.Hour {
		t.Fatalf("want an active first hold of 6h, got active=%v hold=%+v", active, hold)
	}
	if _, active := hub.contributorRepoPushHoldActive(contributorFlapIdentity(held), "myorg/fresh", time.Now()); active {
		t.Fatal("the hold must cover the failing repository only")
	}

	msg := hub.selectTask(held)
	if msg == nil || msg.Type != "task_assign" || msg.Repo != "myorg/fresh" || msg.Number != 202 {
		t.Fatalf("the held contributor must be offered the other repository's work, got %+v", msg)
	}
	// The fork belongs to the account: the same contributor's relay under
	// another session label is held too, and cannot dodge the hold with it.
	laptop := streakConn("ct-held")
	laptop.session = "laptop"
	if msg := hub.selectTask(laptop); msg != nil && msg.Type == "task_assign" && msg.Repo == "myorg/repo" {
		t.Fatalf("another session of the held contributor must not be offered the held repository, got %+v", msg)
	}
	if hub.holdRepoIfForkPushFailed(laptop, pushHoldTask("myorg/repo", 203), pushHoldReasonIdle) {
		t.Fatal("a second session's failure inside the hold must not book another")
	}
	msg = hub.selectTask(other)
	if msg == nil || msg.Type != "task_assign" || msg.Repo != "myorg/repo" || msg.Number != 201 {
		t.Fatalf("another contributor must still be offered the held repository's issue, got %+v", msg)
	}
	if hub.isTaskInFailureCooldown("myorg/repo", 201) || hub.recentFailureCount("myorg/repo", 201) != 0 {
		t.Fatal("a push hold must not book a failure or a cooldown on the issues it withholds")
	}
}

// TestRepoPushHold_OnlyHeldWorkQueuedSaysWhy: when every queued repository is
// one the contributor is held out of, the answer keeps the no_matching_work
// wire reason (so an existing relay backs off as it does today) and carries
// the fix in its message.
func TestRepoPushHold_OnlyHeldWorkQueuedSaysWhy(t *testing.T) {
	hub, s := covK2Hub(t)
	pushHoldRepos(s, []int{211}, nil)
	held := streakConn("ct-held-only")
	hub.holdRepoIfForkPushFailed(held, pushHoldTask("myorg/repo", 210), pushHoldReasonIdle)

	msg := hub.selectTask(held)
	if msg == nil || msg.Type != "task_unavailable" || msg.Reason != taskUnavailableNoMatchingWork {
		t.Fatalf("want task_unavailable/%s, got %+v", taskUnavailableNoMatchingWork, msg)
	}
	for _, want := range []string{"myorg/repo", "Sync fork", "gh auth refresh -h github.com -s workflow"} {
		if !strings.Contains(msg.Message, want) {
			t.Errorf("the refusal message must name %q, got %q", want, msg.Message)
		}
	}
	// Without a hold the same answer carries no message, as before.
	if plain := hub.selectTask(streakConn("ct-nobody")); plain == nil || plain.Type != "task_assign" {
		t.Fatalf("a contributor with no hold must be assigned the issue, got %+v", plain)
	}
}

// TestRepoPushHold_EscalatesAndClears: the second and third failures after an
// expiry book 24h and 72h (the cap); a failure inside a standing hold changes
// nothing; a verified pull request clears the hold and its count.
func TestRepoPushHold_EscalatesAndClears(t *testing.T) {
	hub, _ := covK2Hub(t)
	c := streakConn("ct-escalate")
	id := contributorFlapIdentity(c)
	const repo = "myorg/repo"
	t0 := time.Now()

	first, booked := hub.bookContributorRepoPushHold(c, repo, pushHoldReasonFailed, t0)
	if !booked || first.Count != 1 || !first.Until.Equal(t0.Add(6*time.Hour)) {
		t.Fatalf("first hold: booked=%v %+v", booked, first)
	}
	again, booked := hub.bookContributorRepoPushHold(c, repo, pushHoldReasonIdle, t0.Add(time.Hour))
	if booked || again.Count != 1 || !again.Until.Equal(first.Until) {
		t.Fatalf("a failure inside a standing hold must not extend or escalate it: booked=%v %+v", booked, again)
	}
	if _, active := hub.contributorRepoPushHoldActive(id, repo, first.Until); active {
		t.Fatal("the hold must lapse at its expiry")
	}

	t1 := first.Until.Add(time.Minute)
	second, booked := hub.bookContributorRepoPushHold(c, repo, pushHoldReasonIdle, t1)
	if !booked || second.Count != 2 || !second.Until.Equal(t1.Add(24*time.Hour)) {
		t.Fatalf("second hold: booked=%v %+v", booked, second)
	}
	t2 := second.Until.Add(time.Minute)
	third, booked := hub.bookContributorRepoPushHold(c, repo, pushHoldReasonBehind, t2)
	if !booked || third.Count != 3 || !third.Until.Equal(t2.Add(72*time.Hour)) {
		t.Fatalf("third hold: booked=%v %+v", booked, third)
	}
	t3 := third.Until.Add(time.Minute)
	fourth, booked := hub.bookContributorRepoPushHold(c, repo, pushHoldReasonBehind, t3)
	if !booked || fourth.Count != 4 || !fourth.Until.Equal(t3.Add(72*time.Hour)) {
		t.Fatalf("the hold is capped at 72h: booked=%v %+v", booked, fourth)
	}

	// Another repository and another contributor each count on their own.
	if h, _ := hub.bookContributorRepoPushHold(c, "myorg/other", pushHoldReasonFailed, t3); h.Count != 1 {
		t.Fatalf("the count is per repository, got %+v", h)
	}
	if h, _ := hub.bookContributorRepoPushHold(streakConn("ct-someone"), repo, pushHoldReasonFailed, t3); h.Count != 1 {
		t.Fatalf("the count is per contributor, got %+v", h)
	}

	if !hub.clearContributorRepoPushHold(id, repo) {
		t.Fatal("a verified pull request must clear the standing hold")
	}
	if _, active := hub.contributorRepoPushHoldActive(id, repo, t3.Add(time.Minute)); active {
		t.Fatal("the hold must be gone after the clear")
	}
	if _, active := hub.contributorRepoPushHoldActive(id, "myorg/other", t3.Add(time.Minute)); !active {
		t.Fatal("clearing one repository must leave the other repository's hold")
	}
	fresh, booked := hub.bookContributorRepoPushHold(c, repo, pushHoldReasonFailed, t3.Add(time.Hour))
	if !booked || fresh.Count != 1 || fresh.Until.Sub(fresh.HeldAt) != 6*time.Hour {
		t.Fatalf("the clear must reset the count: booked=%v %+v", booked, fresh)
	}
}

// TestRepoPushHold_CountForgottenAfterQuietPeriod: a hold that lapsed and saw
// no further failure for contributorRepoPushHoldMemory starts again at 6h.
func TestRepoPushHold_CountForgottenAfterQuietPeriod(t *testing.T) {
	hub, _ := covK2Hub(t)
	c := streakConn("ct-quiet")
	t0 := time.Now()
	first, _ := hub.bookContributorRepoPushHold(c, "myorg/repo", pushHoldReasonFailed, t0)
	second, _ := hub.bookContributorRepoPushHold(c, "myorg/repo", pushHoldReasonFailed, first.Until.Add(time.Minute))
	if second.Count != 2 {
		t.Fatalf("setup: want a second hold, got %+v", second)
	}
	later := second.Until.Add(contributorRepoPushHoldMemory + time.Minute)
	again, booked := hub.bookContributorRepoPushHold(c, "myorg/repo", pushHoldReasonFailed, later)
	if !booked || again.Count != 1 {
		t.Fatalf("a count older than the memory window must reset, got booked=%v %+v", booked, again)
	}
}

// TestRepoPushHold_ReasonIsBoundedAndRedacted: the stored reason is the
// relay's text on one line, token redacted and capped like the neighbouring
// ledgers.
func TestRepoPushHold_ReasonIsBoundedAndRedacted(t *testing.T) {
	hub, _ := covK2Hub(t)
	c := streakConn("ct-reason")
	long := "push to fork user/repo rejected:\n token lacks workflow scope " + strings.Repeat("x", noWorkReasonMaxLen*3)
	hold, _ := hub.bookContributorRepoPushHold(c, "myorg/repo", long, time.Now())
	if len(hold.Reason) > noWorkReasonMaxLen || strings.ContainsAny(hold.Reason, "\n\t") {
		t.Fatalf("stored reason must be one line of at most %d bytes, got %d: %q", noWorkReasonMaxLen, len(hold.Reason), hold.Reason)
	}
	if !strings.HasPrefix(hold.Reason, "push to fork user/repo rejected: token lacks workflow scope") {
		t.Fatalf("stored reason must keep the relay's words, got %q", hold.Reason)
	}
}

// TestRepoPushHold_SurvivesRestartAndExpires: the hold round-trips through the
// on-disk ledger beside the no-PR streaks, and lapses on the clock.
func TestRepoPushHold_SurvivesRestartAndExpires(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	c := streakConn("ct-restart")
	id := contributorFlapIdentity(c)
	now := time.Now()

	hub1, _ := covK2Hub(t)
	booked, _ := hub1.bookContributorRepoPushHold(c, "myorg/repo", pushHoldReasonFailed, now)
	// A hold that lapsed long ago is not carried over a restart.
	hub1.bookContributorRepoPushHold(c, "myorg/ancient", pushHoldReasonFailed, now.Add(-(contributorRepoPushHoldMemory + 7*time.Hour)))

	hub2, s2 := covK2Hub(t)
	hold, active := hub2.contributorRepoPushHoldActive(id, "myorg/repo", now.Add(time.Hour))
	if !active || hold.Count != 1 || hold.Reason != booked.Reason || !hold.Until.Equal(booked.Until) || hold.Username != "ct-restart" {
		t.Fatalf("the hold must survive a restart, got active=%v hold=%+v want %+v", active, hold, booked)
	}
	hub2.pushHoldMu.Lock()
	_, ancient := hub2.repoPushHolds[contributorRepoPushHoldKey(id, "myorg/ancient")]
	hub2.pushHoldMu.Unlock()
	if ancient {
		t.Fatal("a record past its memory window must be pruned on load")
	}

	pushHoldRepos(s2, []int{221}, nil)
	if msg := hub2.selectTask(c); msg == nil || msg.Type != "task_unavailable" {
		t.Fatalf("the restored hold must still withhold the repository, got %+v", msg)
	}
	if _, active := hub2.contributorRepoPushHoldActive(id, "myorg/repo", booked.Until.Add(time.Second)); active {
		t.Fatal("the hold must expire on the clock")
	}
	// Age the hold past its expiry: the repository is offered again.
	hub2.pushHoldMu.Lock()
	key := contributorRepoPushHoldKey(id, "myorg/repo")
	rec := hub2.repoPushHolds[key]
	rec.HeldAt = now.Add(-7 * time.Hour)
	rec.Until = now.Add(-time.Hour)
	hub2.repoPushHolds[key] = rec
	hub2.pushHoldMu.Unlock()
	if msg := hub2.selectTask(c); msg == nil || msg.Type != "task_assign" || msg.Repo != "myorg/repo" {
		t.Fatalf("an expired hold must offer the repository again, got %+v", msg)
	}
}

// TestRepoPushHold_RecordsAreBounded: the ledger never grows past its limit;
// the records whose holds ended longest ago go first.
func TestRepoPushHold_RecordsAreBounded(t *testing.T) {
	hub, _ := covK2Hub(t)
	hub.persistTaskLedgers = false
	now := time.Now()
	c := streakConn("ct-bound")
	for i := 0; i < contributorRepoPushHoldLimit+25; i++ {
		// Later repositories are booked later, so the earliest are the oldest.
		hub.bookContributorRepoPushHold(c, "myorg/repo-"+itoa(i), pushHoldReasonFailed, now.Add(time.Duration(i)*time.Second))
	}
	hub.pushHoldMu.Lock()
	n := len(hub.repoPushHolds)
	_, oldest := hub.repoPushHolds[contributorRepoPushHoldKey(contributorFlapIdentity(c), "myorg/repo-0")]
	_, newest := hub.repoPushHolds[contributorRepoPushHoldKey(contributorFlapIdentity(c), "myorg/repo-"+itoa(contributorRepoPushHoldLimit+24))]
	hub.pushHoldMu.Unlock()
	if n != contributorRepoPushHoldLimit || oldest || !newest {
		t.Fatalf("want %d records with the oldest dropped, got %d (oldest kept=%v, newest kept=%v)", contributorRepoPushHoldLimit, n, oldest, newest)
	}
}

// TestRepoPushHold_NoticeSentOncePerHold: booking a hold tells the relay once,
// in a notice frame every relay already prints, what happened and how to fix
// it; a second failure inside the same hold sends nothing.
func TestRepoPushHold_NoticeSentOncePerHold(t *testing.T) {
	hub, _ := covK2Hub(t)
	server, client := wsPipe(t)
	c := streakConn("ct-notice")
	c.ws = server

	if !hub.holdRepoIfForkPushFailed(c, pushHoldTask("myorg/repo", 231), pushHoldReasonFailed) {
		t.Fatal("setup: the failure must book a hold")
	}
	if hub.holdRepoIfForkPushFailed(c, pushHoldTask("myorg/repo", 232), pushHoldReasonIdle) {
		t.Fatal("a second failure inside the hold must not book another")
	}
	// A frame sent after both calls: if the second call had sent a notice it
	// would arrive before this one.
	if err := c.send(WSMessage{Type: "ping", Seq: 99}); err != nil {
		t.Fatalf("send marker: %v", err)
	}

	notice := readWire(t, client)
	if notice["type"] != "notice" {
		t.Fatalf("want a notice frame, got %v", notice)
	}
	text, _ := notice["message"].(string)
	hold, _ := hub.contributorRepoPushHoldActive(contributorFlapIdentity(c), "myorg/repo", time.Now())
	for _, want := range []string{
		"myorg/repo",
		"Sync fork",
		"gh auth refresh -h github.com -s workflow",
		hold.Until.UTC().Format(time.RFC3339),
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the notice must name %q, got %q", want, text)
		}
	}
	if strings.ContainsAny(text, "@\u2013\u2014") {
		t.Errorf("the notice must carry no mention and no long dash, got %q", text)
	}
	if next := readWire(t, client); next["type"] != "ping" {
		t.Fatalf("one notice per hold: the frame after it must be the marker, got %v", next)
	}
}

// pushHoldWire registers a contributor, connects it over a real socket and
// returns the connection with the registration reply.
func pushHoldWire(t *testing.T, s *Server, ts *httptest.Server, username string) (*websocket.Conn, map[string]string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(`{"github_username":"`+username+`"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var reg map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &reg); err != nil {
		t.Fatalf("register response: %v", err)
	}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	readMsg(t, conn) // challenge
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: reg["registration_token"], CLIBackend: "claude"})
	readMsg(t, conn) // auth_ok
	return conn, reg
}

// TestRepoPushHold_FailedPathWireShape drives the real task_failed handler
// with the reason from the hub's log: the hold is booked, the relay is told,
// and the next ready is not handed the same repository's other issue.
func TestRepoPushHold_FailedPathWireShape(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	conn, reg := pushHoldWire(t, s, ts, "stale-fork-user")
	pushHoldRepos(s, []int{301, 302}, nil)

	conn.WriteJSON(WSMessage{Type: "ready", Seq: 2})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" || assign.Repo != "myorg/repo" {
		t.Fatalf("expected a task in myorg/repo, got %+v", assign)
	}
	conn.WriteJSON(WSMessage{Type: "task_failed", TaskID: assign.TaskID, Reason: pushHoldReasonFailed, FailureKind: "environment"})
	notice := readMsg(t, conn)
	if notice.Type != "notice" || !strings.Contains(notice.Message, "myorg/repo") || !strings.Contains(notice.Message, "gh auth refresh -h github.com -s workflow") {
		t.Fatalf("the failure must be answered with the push hold notice, got %+v", notice)
	}
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 3})
	next := readMsg(t, conn)
	if next.Type != "task_unavailable" || next.Reason != taskUnavailableNoMatchingWork || !strings.Contains(next.Message, "myorg/repo") {
		t.Fatalf("the repository's other issue must be withheld from this contributor, got %+v", next)
	}

	hold, active := s.contributeHub.contributorRepoPushHoldActive(reg["contributor_id"], "myorg/repo", time.Now())
	if !active || hold.Count != 1 || hold.Until.Sub(hold.HeldAt) != 6*time.Hour || hold.Username != "stale-fork-user" {
		t.Fatalf("want a first hold of 6h for the contributor, got active=%v %+v", active, hold)
	}
}

// TestRepoPushHold_IdlePathWireShape: the same failure reported as a
// completion without a pull request books the hold too.
func TestRepoPushHold_IdlePathWireShape(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	conn, reg := pushHoldWire(t, s, ts, "idle-fork-user")
	pushHoldRepos(s, []int{311}, []int{312})

	conn.WriteJSON(WSMessage{Type: "ready", Seq: 2})
	assign := readMsg(t, conn)
	if assign.Type != "task_assign" || assign.Repo != "myorg/repo" {
		t.Fatalf("expected a task in myorg/repo, got %+v", assign)
	}
	conn.WriteJSON(WSMessage{
		Type: "task_complete", TaskID: assign.TaskID, Result: "completed",
		CompletionSignal: completionSignalVerdict, VerdictReason: pushHoldReasonIdle,
	})
	notice := readMsg(t, conn)
	if notice.Type != "notice" || !strings.Contains(notice.Message, "Sync fork") {
		t.Fatalf("the idle completion must be answered with the push hold notice, got %+v", notice)
	}
	// The other repository's work still arrives.
	conn.WriteJSON(WSMessage{Type: "ready", Seq: 3})
	next := readMsg(t, conn)
	if next.Type != "task_assign" || next.Repo != "myorg/fresh" || next.Number != 312 {
		t.Fatalf("the held contributor must still be offered the other repository, got %+v", next)
	}
	if _, active := s.contributeHub.contributorRepoPushHoldActive(reg["contributor_id"], "myorg/repo", time.Now()); !active {
		t.Fatal("the idle completion must book the hold")
	}
}

// TestRepoPushHold_VerifiedPRClearsWireShape: a completion whose pull request
// the hub verified, in the held repository, clears the hold and its count.
func TestRepoPushHold_VerifiedPRClearsWireShape(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	s.deps = verifyDepsFor(t, "acme", "widgets", "fixed-fork-user")
	conn, reg := pushHoldWire(t, s, ts, "fixed-fork-user")
	hub := s.contributeHub

	hub.mu.RLock()
	var c *ContributorConnection
	for _, candidate := range hub.connections {
		c = candidate
	}
	hub.mu.RUnlock()
	if c == nil {
		t.Fatal("no live contributor connection")
	}
	// Two lapsed holds on record, and a third in another repository.
	t0 := time.Now().Add(-48 * time.Hour)
	first, _ := hub.bookContributorRepoPushHold(c, "acme/widgets", pushHoldReasonFailed, t0)
	hub.bookContributorRepoPushHold(c, "acme/widgets", pushHoldReasonFailed, first.Until.Add(time.Minute))
	hub.bookContributorRepoPushHold(c, "acme/gadgets", pushHoldReasonFailed, time.Now())

	c.mu.Lock()
	c.currentTask = &WSTaskAssign{TaskID: "probe-1", Kind: "issue", Repo: "acme/widgets", Number: 1}
	c.mu.Unlock()
	if err := conn.WriteJSON(WSMessage{Type: "task_complete", TaskID: "probe-1", Result: "done", PRURL: "https://github.com/acme/widgets/pull/7"}); err != nil {
		t.Fatalf("write task_complete: %v", err)
	}
	waitFor(t, func() bool {
		p := findContributor(reg["contributor_id"])
		return p != nil && p.TasksWithPR == 1
	}, "the verified completion to be recorded")

	hub.pushHoldMu.Lock()
	_, kept := hub.repoPushHolds[contributorRepoPushHoldKey(contributorFlapIdentity(c), "acme/widgets")]
	_, other := hub.repoPushHolds[contributorRepoPushHoldKey(contributorFlapIdentity(c), "acme/gadgets")]
	hub.pushHoldMu.Unlock()
	if kept {
		t.Fatal("a verified pull request in the repository must clear its hold record")
	}
	if !other {
		t.Fatal("a verified pull request must not clear another repository's hold")
	}
	if again, _ := hub.bookContributorRepoPushHold(c, "acme/widgets", pushHoldReasonFailed, time.Now()); again.Count != 1 {
		t.Fatalf("the count must restart after a verified pull request, got %+v", again)
	}
}

// TestRepoPushHold_HeldOutOfEveryRepoDoesNotCover: a relay whose every queued
// repository is held is answered no_matching_work, so after the usual window
// the governor stops counting it as covering its lane.
func TestRepoPushHold_HeldOutOfEveryRepoDoesNotCover(t *testing.T) {
	hub, s, relay, sess, read := noWorkRelay(t)
	oneActionableIssue(s)
	hub.holdRepoIfForkPushFailed(relay, pushHoldTask("myorg/repo1", 100), toolingBlockedReason)
	if f := read(); f["type"] != "notice" {
		t.Fatalf("setup: booking the hold must send the notice, got %v", f)
	}

	f := sendReady(t, sess, read)
	if f["type"] != "task_unavailable" || f["reason"] != taskUnavailableNoMatchingWork {
		t.Fatalf("a relay held out of every queued repository must be answered %s, got %v", taskUnavailableNoMatchingWork, f)
	}
	since := idleSince(relay)
	if since.IsZero() {
		t.Fatal("the answer must start an idle run on the connection")
	}
	if got := coverageAt(hub, relay, since.Add(coverageNoWorkWindow)); got != nil {
		t.Fatalf("a relay held out of every queued repository counted as cover: RoleCoverage = %+v, want nil", got)
	}
}

// TestRepoPushHold_ContributorsListCarriesActiveHolds: GET /api/contributors
// (the Contributors card and the admin MCP's contributors_list) lists each
// contributor's active holds, and only those.
func TestRepoPushHold_ContributorsListCarriesActiveHolds(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	heldID, _ := registerTestUser(t, s, "list-held")
	registerTestUser(t, s, "list-clear")
	hub := s.contributeHub
	now := time.Now()

	held := &ContributorConnection{profile: &ContributorProfile{GitHubUsername: "list-held", ContributorID: heldID}}
	laptop := &ContributorConnection{profile: held.profile, session: "laptop"}
	hub.bookContributorRepoPushHold(held, "myorg/repo", pushHoldReasonFailed, now)
	hub.bookContributorRepoPushHold(laptop, "myorg/also-stale", pushHoldReasonBehind, now)
	hub.bookContributorRepoPushHold(held, "myorg/lapsed", pushHoldReasonFailed, now.Add(-7*time.Hour))
	// The two sessions share the account's holds: the second session's
	// failure in the same repository books nothing new.
	if _, booked := hub.bookContributorRepoPushHold(laptop, "myorg/repo", pushHoldReasonIdle, now.Add(time.Minute)); booked {
		t.Fatal("a second session must share the contributor's hold, not book its own")
	}

	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/contributors", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Contributors []ContributorProfile `json:"contributors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byLogin := map[string]ContributorProfile{}
	for _, p := range resp.Contributors {
		byLogin[p.GitHubUsername] = p
	}
	if got := byLogin["list-clear"].RepoPushHolds; len(got) != 0 {
		t.Fatalf("a contributor with no hold must list none, got %+v", got)
	}
	got := byLogin["list-held"].RepoPushHolds
	if len(got) != 2 || got[0].Repo != "myorg/also-stale" || got[1].Repo != "myorg/repo" {
		t.Fatalf("want the two active holds sorted by repository, got %+v", got)
	}
	stale := got[1]
	if stale.Count != 1 || stale.Reason == "" || stale.HeldAt != now.UTC().Format(time.RFC3339) || stale.Until != now.Add(6*time.Hour).UTC().Format(time.RFC3339) {
		t.Fatalf("the row must carry repo, reason, until and count, got %+v", stale)
	}
	if strings.Contains(w.Body.String(), "myorg/lapsed") {
		t.Fatal("a lapsed hold must not be listed")
	}
	// The transient field is never written to the profile on disk.
	p, err := loadContributorProfile("list-held")
	if err != nil {
		t.Fatalf("load list-held: %v", err)
	}
	if len(p.RepoPushHolds) != 0 {
		t.Fatalf("holds must not be persisted on the profile, got %+v", p.RepoPushHolds)
	}
}

// TestContributorPushHoldsCard: the Contributors card renders the active holds
// from repo_push_holds, escaped, and nothing when there are none.
func TestContributorPushHoldsCard(t *testing.T) {
	html := indexHTML(t)
	render := jsFunctionBody(t, html, "function renderContributors(contributors)")
	if !strings.Contains(render, "${contributorPushHolds(c)}") {
		t.Fatal("contributor cards do not render the push holds")
	}
	if !strings.Contains(html, ".contrib-push-hold {") {
		t.Fatal("the push hold row has no style")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH: push hold rendering was NOT executed")
	}
	js := jsFunctionBody(t, html, "function esc(s)") + "\n" + jsFunctionBody(t, html, "function contributorPushHolds(c)") + `
function assert(ok, message) { if (!ok) throw new Error(message); }
function formatDashboardDateTime(value) { return 'T(' + value + ')'; }
for (const c of [{}, {repo_push_holds: null}, {repo_push_holds: []}, {repo_push_holds: 'x'}]) {
  assert(contributorPushHolds(c) === '', 'no holds should render nothing');
}
const holds = [
  {repo: 'org/stale', reason: 'push to fork rejected', count: 2, held_at: '2026-11-27T10:00:00Z', until: '2026-11-28T10:00:00Z'},
  {repo: 'org/other', reason: 'token lacks workflow scope', count: 1, held_at: '2026-11-27T11:00:00Z', until: '2026-11-27T17:00:00Z'},
];
const before = JSON.stringify(holds);
const out = contributorPushHolds({repo_push_holds: holds});
assert((out.match(/class="contrib-push-hold"/g) || []).length === 2, out);
assert(out.includes('Push hold: org/stale until T(2026-11-28T10:00:00Z)'), out);
assert(out.includes('hold 2'), out);
assert(out.includes('push to fork rejected'), out);
assert(out.includes('Push hold: org/other until T(2026-11-27T17:00:00Z)</div>'), out);
assert(out.includes('gh auth refresh -h github.com -s workflow'), 'the row must say how the contributor fixes it: ' + out);
assert(JSON.stringify(holds) === before, 'render mutated source records');
const unsafe = contributorPushHolds({repo_push_holds: [{repo: '"<img>', reason: '" onmouseover="evil', until: '<script>', count: '<b>'}]});
assert(!unsafe.includes('<img>') && !unsafe.includes('<script>') && !unsafe.includes('<b>') && !unsafe.includes('" onmouseover="'), unsafe);
`
	if out, err := exec.Command(node, "-e", js).CombinedOutput(); err != nil {
		t.Fatalf("push hold rendering failed: %v\n%s", err, out)
	}
}
