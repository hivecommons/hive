package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A contributor whose provider plan hit its weekly limit kept being handed
// issues. Each task failed in two seconds, booked the ten-minute failure
// cooldown on the issue and counted toward its quarantine, and the same
// contributor took the same issue again ten minutes later. These tests pin
// the hub-side provider-quota hold: the contributor is held out of all work
// until the quota is back, and the issue is not penalised.

// quotaReasonReal is the hub's own log line for those failures. The
// separator is U+00B7.
const quotaReasonReal = "headless CLI exited with error: code 1: You've hit your weekly limit · resets Dec 1, 3pm (UTC)"

// quotaReasonNoReset states no reset time the hub can read.
const quotaReasonNoReset = "headless CLI exited with error: code 1: You've hit your weekly limit"

// quotaReasonResetting is the real reason with a reset time `in` from now,
// on the hour, and that time. Like the CLI, it states the year only when the
// reset falls in another calendar year.
func quotaReasonResetting(in time.Duration) (string, time.Time) {
	now := time.Now().UTC()
	reset := now.Add(in).Truncate(time.Hour)
	layout := "Jan 2, 3pm"
	if reset.Year() != now.Year() {
		layout = "Jan 2, 2006, 3pm"
	}
	return "headless CLI exited with error: code 1: You've hit your weekly limit · resets " + reset.Format(layout) + " (UTC)", reset
}

// ageReleaseCooldown moves the issue's failure stamp past the short cooldown,
// the state a live hub is in ten minutes after the task was let go.
func ageReleaseCooldown(hub *ContributeWSHub, repo string, number int) {
	hub.completedMu.Lock()
	hub.failedTasks[noPRKey(repo, number)] = time.Now().Add(-(failedTaskCooldownMinutes + 1) * time.Minute)
	hub.completedMu.Unlock()
}

func TestProviderQuotaExhaustion_Classifier(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		want   bool
	}{
		{"claude weekly limit, headless", quotaReasonReal, true},
		{"claude weekly limit, curly apostrophe", "headless CLI exited with error: code 1: You\u2019ve hit your weekly limit · resets 3pm (UTC)", true},
		{"claude two-word limit", "headless CLI exited with error: code 1: You've hit your Opus weekly limit · resets 3:30pm (UTC)", true},
		{"at the start of the reason", "You've hit your session limit · resets 3pm (UTC)", true},
		{"no limit name", "headless CLI exited with error: code 1: You've hit your limit · resets 3pm (America/New_York)", true},
		{"5-hour limit", "You've hit your 5-hour limit · resets 7pm (UTC)", true},
		{"org spend limit", "You've hit your org's monthly spend limit", true},
		{"monthly spend limit", "headless CLI exited with error: code 1: You've hit your monthly spend limit", true},
		{"usage credit limit", "You've hit your usage credit limit · resets 3pm (UTC)", true},
		{"channel usage limit, curly apostrophes", "You\u2019ve hit your channel\u2019s monthly usage limit", true},
		{"out of usage credits", "headless CLI exited with error: code 1: You're out of usage credits", true},
		{"codex headless", "headless CLI exited with error: code 1: ERROR: You've hit your usage limit. Upgrade to Pro or try again at 3:15 PM.", true},
		{"codex banner glyph", "■ You've hit your usage limit. To get more access now, send a request to your admin.", true},
		{"budget exceeded under API Error chrome", `headless CLI exited with error: code 1: API Error: 400 {"error":{"message":"Budget has been exceeded! Current cost: 10.2, Max budget: 10.0","type":"budget_exceeded"}}`, true},
		{"agy banner", "⚠ Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 38m29s.", true},
		{"listed wording at the start", "Individual quota reached. Please upgrade your subscription to increase your limits.", true},
		{"relay that parked itself", "provider quota exhausted for agy \u2014 not a fault of this host; the relay is standing down for 38m and will ask for work again after that (⚠ Individual quota reached. Resets in 38m29s.)", true},
		{"wrapped over lines", "headless CLI exited with error: code 1:\nYou've hit your\nweekly limit", true},

		{"empty", "", false},
		// The same words inside a sentence an agent wrote are not a CLI
		// diagnostic.
		{"agent prose quoting the banner", "agent reported an environment failure: the CLI printed You've hit your weekly limit so nothing ran", false},
		{"agent prose about quota", "the issue asks why individual quota reached is shown twice; no change needed", false},
		{"listed wording mid-sentence without chrome", "headless CLI exited with error: code 1: the tool said budget has been exceeded while testing the billing page", false},
		// Authorization is not quota: it does not expire.
		{"authorization refusal", `headless CLI exited with error: code 1: API Error: 403 {"error":{"message":"team not allowed to access model"}}`, false},
		{"not allowed to access model", "API Error: 400 key not allowed to access model claude-opus; budget_exceeded", false},
		{"ambiguous relay summary", "agent stopped on an API failure a retry cannot clear (authorization or quota)", false},
		{"rate limit error", `headless CLI exited with error: code 1: API Error: 429 {"error":{"type":"rate_limit_error"}}`, false},
		{"overloaded", "headless CLI exited with error: code 1: API Error: 529 Overloaded", false},
		{"cli usage error", "headless CLI exited with error: code 1: Unrecognized flag: --auto in command opencode run", false},
		{"three words before limit", "You've hit your very own personal limit", false},
		// Other limits are not the account's quota.
		{"rate limit", "headless CLI exited with error: code 1: You've hit your rate limit", false},
		{"context window limit", "You've hit your context window limit", false},
		{"max-turns limit", "You've hit your max-turns limit", false},
		{"three names", "You've hit your Opus weekly session limit", false},
		{"watchdog", "no observed progress for 30min \u2014 the agent CLI is not visibly working", false},
		{"timeout", "headless task exceeded 30min and was killed", false},
		{"task failure", "tests failed", false},
		{"fork push failure", pushHoldReasonFailed, false},
	}
	for _, tc := range cases {
		if got := isProviderQuotaExhaustion(tc.reason); got != tc.want {
			t.Errorf("%s: isProviderQuotaExhaustion(%q) = %v, want %v", tc.name, tc.reason, got, tc.want)
		}
	}
}

func TestParseProviderQuotaReset(t *testing.T) {
	utc := func(y int, m time.Month, d, h, mi int) time.Time {
		return time.Date(y, m, d, h, mi, 0, 0, time.UTC)
	}
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("the zone database must be available to the hub: %v", err)
	}
	now := utc(2026, time.November, 28, 16, 20)
	cases := []struct {
		name   string
		reason string
		now    time.Time
		want   time.Time
		ok     bool
	}{
		// The three shapes Claude Code prints.
		{"hour", "You've hit your session limit · resets 7pm (UTC)", now, utc(2026, time.November, 28, 19, 0), true},
		{"date and hour", quotaReasonReal, now, utc(2026, time.December, 1, 15, 0), true},
		{"date with the year, across the year boundary", "You've hit your weekly limit · resets Jan 2, 2027, 9am (UTC)", utc(2026, time.December, 30, 22, 0), utc(2027, time.January, 2, 9, 0), true},
		{"date without the year rolls into next year", "resets Jan 2, 9am (UTC)", utc(2026, time.December, 30, 22, 0), utc(2027, time.January, 2, 9, 0), true},
		{"a past stated year", "resets Jan 2, 2026, 9am (UTC)", utc(2026, time.December, 30, 22, 0), time.Time{}, false},
		{"a stated year too far ahead", "resets Jan 2, 2028, 9am (UTC)", utc(2026, time.December, 30, 22, 0), time.Time{}, false},

		// The zone is the CLI's own.
		{"IANA zone", "You've hit your limit · resets 3pm (America/New_York)", now, utc(2026, time.November, 28, 20, 0), true},
		{"IANA zone, date", "resets Dec 1, 9:30am (America/New_York)", now, utc(2026, time.December, 1, 14, 30), true},
		{"IANA zone, the zone's own day", "resets 1am (America/New_York)", utc(2026, time.November, 29, 3, 0), time.Date(2026, time.November, 29, 1, 0, 0, 0, newYork), true},
		{"unknown zone", "resets 3pm (PST)", now, time.Time{}, false},
		{"not a zone", "resets 3pm (Mars/Olympus)", now, time.Time{}, false},
		{"the hub's own zone is not the CLI's", "resets 3pm (Local)", now, time.Time{}, false},
		{"no zone", "You've hit your usage limit. Try again at 3:15 PM.", now, time.Time{}, false},

		// Clock only: the next occurrence, unless it has only just passed.
		{"hour well past today is tomorrow", "resets 3pm (UTC)", now, utc(2026, time.November, 29, 15, 0), true},
		{"hour and minutes", "resets 3:30pm (UTC)", now, utc(2026, time.November, 29, 15, 30), true},
		{"exactly now counts", "resets 4:20pm (UTC)", now, now, true},
		{"midnight", "resets 12am (UTC)", now, utc(2026, time.November, 29, 0, 0), true},
		{"noon", "resets 12pm (UTC)", utc(2026, time.November, 28, 9, 0), utc(2026, time.November, 28, 12, 0), true},
		{"just passed is not tomorrow", "resets 4:15pm (UTC)", now, time.Time{}, false},
		{"passed ten minutes ago", "resets 4:10pm (UTC)", now, time.Time{}, false},
		{"passed eleven minutes ago is tomorrow", "resets 4:09pm (UTC)", now, utc(2026, time.November, 29, 16, 9), true},
		{"just passed, across midnight", "resets 11:55pm (UTC)", utc(2026, time.November, 29, 0, 2), time.Time{}, false},
		{"just passed, across the zone's midnight", "resets 11:55pm (America/New_York)", utc(2026, time.November, 29, 5, 2), time.Time{}, false},
		{"date just passed", "resets Nov 28, 4:15pm (UTC)", now, time.Time{}, false},

		{"full month name, capitals", "RESETS December 1, 3PM (UTC)", now, utc(2026, time.December, 1, 15, 0), true},
		{"now in another zone", "resets 7pm (UTC)", now.In(newYork), utc(2026, time.November, 28, 19, 0), true},
		{"relative", "Individual quota reached. Resets in 4h42m28s.", now, now.Add(4*time.Hour + 42*time.Minute + 28*time.Second), true},
		{"relative, days", "resets in 2d 3h", now, now.Add(51 * time.Hour), true},
		{"relative, a month is not a minute", "Resets in 1 month", now, time.Time{}, false},
		{"relative, words are not units", "resets in 30 minutes", now, time.Time{}, false},
		{"relative over eight days", "resets in 9d", now, time.Time{}, false},
		{"relative zero", "Resets in 0s", now, time.Time{}, false},

		{"none stated", quotaReasonNoReset, now, time.Time{}, false},
		{"more than eight days away", "resets Dec 7, 5pm (UTC)", now, time.Time{}, false},
		{"exactly eight days away", "resets Dec 6, 4:20pm (UTC)", now, utc(2026, time.December, 6, 16, 20), true},
		{"not a month", "resets Foo 1, 3pm (UTC)", now, time.Time{}, false},
		{"not a day", "resets Feb 30, 3pm (UTC)", utc(2026, time.February, 25, 0, 0), time.Time{}, false},
		{"not an hour", "resets 13pm (UTC)", now, time.Time{}, false},
		{"not a minute", "resets 3:75pm (UTC)", now, time.Time{}, false},
	}
	for _, tc := range cases {
		got, ok := parseProviderQuotaReset(tc.reason, tc.now)
		if ok != tc.ok || (ok && !got.Equal(tc.want)) {
			t.Errorf("%s: parseProviderQuotaReset(%q) = (%v, %v), want (%v, %v)", tc.name, tc.reason, got, ok, tc.want, tc.ok)
		}
	}
}

// failAssignedTask reports the task c holds as failed with reason, through
// the real task_failed handler.
func failAssignedTask(t *testing.T, hub *ContributeWSHub, c *ContributorConnection, reason string) {
	t.Helper()
	c.mu.Lock()
	task := c.currentTask
	c.mu.Unlock()
	if task == nil {
		t.Fatal("setup: the connection holds no task to fail")
	}
	(&wsSession{h: hub, contributor: c}).handleTaskFailed(WSMessage{Type: "task_failed", TaskID: task.TaskID, Reason: reason})
}

// assignedIssue asserts selectTask hands c the given issue.
func assignedIssue(t *testing.T, hub *ContributeWSHub, c *ContributorConnection, repo string, number int) {
	t.Helper()
	msg := hub.selectTask(c)
	if msg == nil || msg.Type != "task_assign" || msg.Repo != repo || msg.Number != number {
		t.Fatalf("want %s#%d assigned to %s, got %+v", repo, number, identityOf(c), msg)
	}
}

func failureStreakOf(hub *ContributeWSHub, identity string) (contributorFailureStreak, bool) {
	hub.failureStreakMu.Lock()
	defer hub.failureStreakMu.Unlock()
	rec, ok := hub.contributorFailureStreaks[identity]
	return rec, ok
}

// TestQuotaHold_HoldsContributorNotIssue: a quota failure holds the
// contributor out of every repository until the stated reset. The issue gets
// the ten-minute release cooldown and nothing else: no failure count, no
// reclaim cooldown. Once that window has passed it is offered to another
// contributor.
func TestQuotaHold_HoldsContributorNotIssue(t *testing.T) {
	hub, s := covK2Hub(t)
	pushHoldRepos(s, []int{401}, []int{402})
	out := streakConn("ct-quota")
	reason, reset := quotaReasonResetting(26 * time.Hour)

	assignedIssue(t, hub, out, "myorg/repo", 401)
	failAssignedTask(t, hub, out, reason)

	hold, held := hub.contributorQuotaHoldActive(identityOf(out), time.Now())
	if !held || hold.Count != 1 || !hold.ResetStated || !hold.Until.Equal(reset) || hold.Username != "ct-quota" {
		t.Fatalf("want a hold until the stated reset %v, got held=%v %+v", reset, held, hold)
	}
	if _, held := hub.contributorQuotaHoldActive(identityOf(out), reset); held {
		t.Fatal("the hold must end at the stated reset")
	}
	// Held out of every repository, although myorg/fresh has work.
	refused := hub.selectTask(out)
	if refused == nil || refused.Type != "task_unavailable" || refused.Reason != taskUnavailableNoMatchingWork {
		t.Fatalf("a quota-held contributor must be offered nothing, got %+v", refused)
	}
	if !strings.Contains(refused.Message, "out of quota") || !strings.Contains(refused.Message, reset.Format(time.RFC3339)) {
		t.Fatalf("the refusal must say why and until when, got %q", refused.Message)
	}
	// The same contributor's relay on another backend (its own session) is
	// not held: the quota belongs to one provider account.
	otherBackend := streakConn("ct-quota")
	otherBackend.session = "agy"
	if _, held := hub.contributorQuotaHoldActive(identityOf(otherBackend), time.Now()); held {
		t.Fatal("the quota hold must not spread to the contributor's other session")
	}

	// The issue is let go, not failed: the release cooldown and no count.
	if !hub.isTaskInFailureCooldown("myorg/repo", 401) {
		t.Fatal("a quota failure must book the release cooldown on the issue")
	}
	if n := hub.recentFailureCount("myorg/repo", 401); n != 0 {
		t.Fatalf("a quota failure must not count toward the issue's quarantine, got %d", n)
	}
	if hub.inReclaimCooldown(identityOf(out), noPRKey("myorg/repo", 401)) {
		t.Fatal("a quota failure must not book the reclaim cooldown: the contributor never worked the issue")
	}
	// Inside the window nobody is handed it; after it, the next contributor is.
	assignedIssue(t, hub, streakConn("ct-has-quota"), "myorg/fresh", 402)
	ageReleaseCooldown(hub, "myorg/repo", 401)
	assignedIssue(t, hub, streakConn("ct-next"), "myorg/repo", 401)

	// And the failure streak was not advanced.
	if rec, ok := failureStreakOf(hub, identityOf(out)); ok {
		t.Fatalf("a quota failure must not count toward the failure streak, got %+v", rec)
	}
}

// TestQuotaHold_OtherFailuresStillPenaliseTheIssue: the control. A failure
// that is not a quota refusal books the ten-minute cooldown, counts toward
// quarantine and the failure streak, and holds nobody.
func TestQuotaHold_OtherFailuresStillPenaliseTheIssue(t *testing.T) {
	hub, s := covK2Hub(t)
	pushHoldRepos(s, []int{411}, []int{412})
	c := streakConn("ct-plain-failure")

	assignedIssue(t, hub, c, "myorg/repo", 411)
	failAssignedTask(t, hub, c, "headless CLI exited with error: code 1: Unrecognized flag: --auto in command opencode run")

	if !hub.isTaskInFailureCooldown("myorg/repo", 411) || hub.recentFailureCount("myorg/repo", 411) != 1 {
		t.Fatal("an ordinary failure must still book the failure cooldown and count toward quarantine")
	}
	if _, held := hub.contributorQuotaHoldActive(identityOf(c), time.Now()); held {
		t.Fatal("an ordinary failure must not book a quota hold")
	}
	if rec, ok := failureStreakOf(hub, identityOf(c)); !ok || rec.Count != 1 {
		t.Fatalf("an ordinary fast failure must still count toward the failure streak, got ok=%v %+v", ok, rec)
	}
	assignedIssue(t, hub, c, "myorg/fresh", 412)
}

// TestQuotaHold_ThreeQuotaFailuresDoNotQuarantineTheIssue: three contributors
// in a row out of quota on the same issue cost it ten minutes each and never
// the six-hour quarantine three ordinary failures would.
func TestQuotaHold_ThreeQuotaFailuresDoNotQuarantineTheIssue(t *testing.T) {
	hub, s := covK2Hub(t)
	pushHoldRepos(s, []int{421}, nil)
	for i := 0; i < consecutiveFailureQuarantineThreshold; i++ {
		c := streakConn("ct-out-" + itoa(i))
		assignedIssue(t, hub, c, "myorg/repo", 421)
		failAssignedTask(t, hub, c, quotaReasonNoReset)
		if !hub.isTaskInFailureCooldown("myorg/repo", 421) {
			t.Fatalf("quota failure %d must book the release cooldown", i+1)
		}
		if n := hub.recentFailureCount("myorg/repo", 421); n != 0 {
			t.Fatalf("quota failure %d must not count toward quarantine, got %d", i+1, n)
		}
		// Ten minutes on, the issue is admissible again: no quarantine.
		ageReleaseCooldown(hub, "myorg/repo", 421)
		if hub.isTaskInFailureCooldown("myorg/repo", 421) {
			t.Fatalf("after quota failure %d the issue must leave its cooldown in ten minutes, not six hours", i+1)
		}
	}
	assignedIssue(t, hub, streakConn("ct-works"), "myorg/repo", 421)
}

// TestQuotaHold_QuotaFailureClearsTheFailureStreak: a quota failure resets a
// fast-failure streak already running, so the streak's ten-minute pause never
// stacks on the quota hold.
func TestQuotaHold_QuotaFailureClearsTheFailureStreak(t *testing.T) {
	hub, s := covK2Hub(t)
	pushHoldRepos(s, []int{431}, nil)
	c := streakConn("ct-streak-then-quota")
	bookFastFailures(hub, identityOf(c), contributorFailureStreakThreshold-1)

	assignedIssue(t, hub, c, "myorg/repo", 431)
	failAssignedTask(t, hub, c, quotaReasonNoReset)

	if rec, ok := failureStreakOf(hub, identityOf(c)); ok {
		t.Fatalf("a quota failure must clear the failure streak, got %+v", rec)
	}
	if paused, _, _ := hub.contributorFailureStreakActive(identityOf(c), time.Now()); paused {
		t.Fatal("the failure streak must not pause a contributor the quota hold already holds")
	}
}

// TestQuotaHold_NoStatedResetEscalates: with no reset time to read, the hold
// lasts 30 minutes, then 1, 2 and 6 hours as quota failures repeat; a failure
// inside a standing hold changes nothing; a quiet day forgets the count.
func TestQuotaHold_NoStatedResetEscalates(t *testing.T) {
	hub, _ := covK2Hub(t)
	c := streakConn("ct-ladder")
	at := time.Now()
	for i, want := range []time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour, 6 * time.Hour, 6 * time.Hour} {
		hold, booked := hub.bookContributorQuotaHold(c, quotaReasonNoReset, at)
		if !booked || hold.Count != i+1 || hold.ResetStated || !hold.Until.Equal(at.Add(want)) {
			t.Fatalf("hold %d: want %v from %v, got booked=%v %+v", i+1, want, at, booked, hold)
		}
		inside, booked := hub.bookContributorQuotaHold(c, quotaReasonNoReset, at.Add(time.Minute))
		if booked || inside.Count != i+1 || !inside.Until.Equal(hold.Until) {
			t.Fatalf("hold %d: a failure inside the hold must not extend or escalate it, got booked=%v %+v", i+1, booked, inside)
		}
		at = hold.Until.Add(time.Minute)
	}
	// A stated reset is used whatever the count.
	stated, booked := hub.bookContributorQuotaHold(c, "You've hit your weekly limit · resets in 3h", at)
	if !booked || !stated.ResetStated || !stated.Until.Equal(at.Add(3*time.Hour)) {
		t.Fatalf("a stated reset must set the hold's end, got booked=%v %+v", booked, stated)
	}
	// Another identity counts on its own.
	if h, _ := hub.bookContributorQuotaHold(streakConn("ct-ladder-2"), quotaReasonNoReset, at); h.Count != 1 {
		t.Fatalf("the count is per contributor, got %+v", h)
	}
	// A day with no quota failure after the hold ended forgets the count.
	later := stated.Until.Add(contributorQuotaHoldMemory + time.Minute)
	fresh, booked := hub.bookContributorQuotaHold(c, quotaReasonNoReset, later)
	if !booked || fresh.Count != 1 || !fresh.Until.Equal(later.Add(30*time.Minute)) {
		t.Fatalf("a count older than the memory window must reset, got booked=%v %+v", booked, fresh)
	}
}

// TestQuotaHold_CompletedTaskClears: a completion with evidence behind it
// proves the provider answers and clears the hold and its count. An
// evidence-less one (the relay inferred the end from an idle prompt, which is
// what a CLI that printed its quota refusal looks like) does not.
func TestQuotaHold_CompletedTaskClears(t *testing.T) {
	hub, _ := covK2Hub(t)
	c := streakConn("ct-quota-back")
	sess := &wsSession{h: hub, contributor: c}
	now := time.Now()
	// The first hold (30 minutes) lapsed ten minutes ago; the second (1 hour)
	// was booked a minute after that and still stands.
	first, _ := hub.bookContributorQuotaHold(c, quotaReasonNoReset, now.Add(-40*time.Minute))
	hub.bookContributorQuotaHold(c, quotaReasonNoReset, first.Until.Add(time.Minute))
	if hold, held := hub.contributorQuotaHoldActive(identityOf(c), now); !held || hold.Count != 2 {
		t.Fatalf("setup: want a standing second hold, got held=%v %+v", held, hold)
	}
	complete := func(taskID string, number int, signal string) {
		t.Helper()
		c.mu.Lock()
		c.currentTask = &WSTaskAssign{TaskID: taskID, Kind: "issue", Repo: "myorg/repo1", Number: number}
		c.mu.Unlock()
		sess.handleTaskComplete(WSMessage{Type: "task_complete", TaskID: taskID, Result: "completed", CompletionSignal: signal})
	}

	for i, signal := range []string{completionSignalChromeIdle, completionSignalVerdict} {
		complete("evidence-less-"+itoa(i), 441+i, signal)
		if _, held := hub.contributorQuotaHoldActive(identityOf(c), time.Now()); !held {
			t.Fatalf("a no-PR completion with signal %q is no evidence the provider answered; the hold must stand", signal)
		}
	}

	// The headless path reports no signal: exit code 0 is the evidence.
	complete("in-flight-1", 451, "")
	if _, held := hub.contributorQuotaHoldActive(identityOf(c), time.Now()); held {
		t.Fatal("a completed task must clear the quota hold")
	}
	if again, _ := hub.bookContributorQuotaHold(c, quotaReasonNoReset, time.Now()); again.Count != 1 {
		t.Fatalf("a completed task must clear the count too, got %+v", again)
	}
}

// TestQuotaHold_RelayReportDoesNotFightTheHub: the relay's own quota_hold
// frames leave a hold with the provider's stated reset standing, and end one
// whose length the hub only guessed, keeping its count, but never inside the
// hold's first fifteen minutes (contributorQuotaHoldMin).
func TestQuotaHold_RelayReportDoesNotFightTheHub(t *testing.T) {
	hub, _ := covK2Hub(t)
	guessed := streakConn("ct-relay-guessed")
	fresh := streakConn("ct-relay-fresh")
	stated := streakConn("ct-relay-stated")
	now := time.Now()
	hub.bookContributorQuotaHold(guessed, quotaReasonNoReset, now.Add(-20*time.Minute))
	freshHold, _ := hub.bookContributorQuotaHold(fresh, quotaReasonNoReset, now)
	hub.bookContributorQuotaHold(stated, "You've hit your weekly limit · resets in 5h", now)

	for _, c := range []*ContributorConnection{guessed, fresh, stated} {
		sess := &wsSession{h: hub, contributor: c}
		sess.handleQuotaHold(WSMessage{Type: "quota_hold", State: "held", Reason: "provider_quota"})
		if _, held := hub.contributorQuotaHoldActive(identityOf(c), time.Now()); !held {
			t.Fatalf("%s: a relay reporting held must leave the hub hold standing", identityOf(c))
		}
		sess.handleQuotaHold(WSMessage{Type: "quota_hold", State: "released"})
	}
	if _, held := hub.contributorQuotaHoldActive(identityOf(stated), time.Now()); !held {
		t.Fatal("a relay release must not lift a hold that carries the provider's stated reset")
	}
	if _, held := hub.contributorQuotaHoldActive(identityOf(guessed), time.Now()); held {
		t.Fatal("a relay release must end a hold whose length the hub only guessed")
	}
	// Inside that floor the release only shortens the hold to
	// that floor.
	floor := freshHold.HeldAt.Add(contributorQuotaHoldMin)
	if hold, held := hub.contributorQuotaHoldActive(identityOf(fresh), time.Now()); !held || !hold.Until.Equal(floor) {
		t.Fatalf("a relay release inside the hold floor must leave the hold standing until %v, got held=%v %+v", floor, held, hold)
	}
	if _, held := hub.contributorQuotaHoldActive(identityOf(fresh), floor); held {
		t.Fatal("the shortened hold must end at the floor")
	}
	// The count survives the release: the next quota failure escalates.
	if next, booked := hub.bookContributorQuotaHold(guessed, quotaReasonNoReset, time.Now()); !booked || next.Count != 2 {
		t.Fatalf("the count must survive a relay release, got booked=%v %+v", booked, next)
	}
}

// TestQuotaHold_NeverShorterThanTheReleaseCooldown: whatever reset the reason
// states, the hold lasts at least as long as the release cooldown the issue
// was given, so the contributor is never back before the issue is.
func TestQuotaHold_NeverShorterThanTheReleaseCooldown(t *testing.T) {
	hub, _ := covK2Hub(t)
	if release := failedTaskCooldownMinutes * time.Minute; contributorQuotaHoldMin != release+5*time.Minute {
		t.Fatalf("the hold floor %v must be the issue's release cooldown %v plus five minutes, so the two never end together", contributorQuotaHoldMin, release)
	}
	now := time.Now()
	soon, booked := hub.bookContributorQuotaHold(streakConn("ct-quota-soon"), "You've hit your session limit · resets in 1s", now)
	if !booked || !soon.Until.Equal(now.Add(contributorQuotaHoldMin)) || soon.ResetStated {
		t.Fatalf("a reset a second away must still hold for the floor, as the hub's own time, got booked=%v %+v", booked, soon)
	}
	later, _ := hub.bookContributorQuotaHold(streakConn("ct-quota-later"), "You've hit your session limit · resets in 20m", now)
	if !later.ResetStated || !later.Until.Equal(now.Add(20*time.Minute)) {
		t.Fatalf("a reset past the floor must be kept as stated, got %+v", later)
	}
}

// TestQuotaHold_SurvivesRestartAndExpires: the hold round-trips through the
// on-disk ledger beside the push holds, and lapses on the clock.
func TestQuotaHold_SurvivesRestartAndExpires(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	c := streakConn("ct-quota-restart")
	id := identityOf(c)
	now := time.Now()

	hub1, _ := covK2Hub(t)
	booked, _ := hub1.bookContributorQuotaHold(c, "You've hit your weekly limit · resets in 5h", now)
	// A hold that lapsed long ago is not carried over a restart.
	hub1.bookContributorQuotaHold(streakConn("ct-quota-ancient"), quotaReasonNoReset, now.Add(-(contributorQuotaHoldMemory + 2*time.Hour)))

	hub2, s2 := covK2Hub(t)
	hold, held := hub2.contributorQuotaHoldActive(id, now.Add(time.Hour))
	if !held || hold.Count != 1 || !hold.ResetStated || hold.Reason != booked.Reason || !hold.Until.Equal(booked.Until) || hold.Username != "ct-quota-restart" {
		t.Fatalf("the hold must survive a restart, got held=%v %+v want %+v", held, hold, booked)
	}
	hub2.quotaHoldMu.Lock()
	_, ancient := hub2.quotaHolds["ct-quota-ancient"]
	hub2.quotaHoldMu.Unlock()
	if ancient {
		t.Fatal("a record past its memory window must not be carried over a restart")
	}

	pushHoldRepos(s2, []int{451}, nil)
	if msg := hub2.selectTask(c); msg == nil || msg.Type != "task_unavailable" {
		t.Fatalf("the restored hold must still refuse work, got %+v", msg)
	}
	if _, held := hub2.contributorQuotaHoldActive(id, booked.Until); held {
		t.Fatal("the hold must expire on the clock")
	}
	// Age the hold past its end: work is offered again.
	hub2.quotaHoldMu.Lock()
	rec := hub2.quotaHolds[id]
	rec.HeldAt = now.Add(-6 * time.Hour)
	rec.Until = now.Add(-time.Hour)
	hub2.quotaHolds[id] = rec
	hub2.quotaHoldMu.Unlock()
	assignedIssue(t, hub2, c, "myorg/repo", 451)
}

// TestQuotaHold_RecordsAreBounded: the ledger never grows past its limit.
func TestQuotaHold_RecordsAreBounded(t *testing.T) {
	hub, _ := covK2Hub(t)
	hub.persistTaskLedgers = false
	now := time.Now()
	for i := 0; i < contributorQuotaHoldLimit+25; i++ {
		// Later contributors are booked later, so the earliest are the oldest.
		hub.bookContributorQuotaHold(streakConn("ct-bound-"+itoa(i)), quotaReasonNoReset, now.Add(time.Duration(i)*time.Second))
	}
	hub.quotaHoldMu.Lock()
	n := len(hub.quotaHolds)
	_, oldest := hub.quotaHolds["ct-bound-0"]
	_, newest := hub.quotaHolds["ct-bound-"+itoa(contributorQuotaHoldLimit+24)]
	hub.quotaHoldMu.Unlock()
	if n != contributorQuotaHoldLimit || oldest || !newest {
		t.Fatalf("want %d records with the oldest dropped, got %d (oldest kept=%v, newest kept=%v)", contributorQuotaHoldLimit, n, oldest, newest)
	}
}

// TestQuotaHold_NoticeSentOncePerHold: booking the hold tells the relay once,
// in a notice frame every relay already prints; a second quota failure
// inside the same hold sends nothing.
func TestQuotaHold_NoticeSentOncePerHold(t *testing.T) {
	hub, _ := covK2Hub(t)
	server, client := wsPipe(t)
	c := streakConn("ct-quota-notice")
	c.ws = server
	reason, reset := quotaReasonResetting(26 * time.Hour)

	if !hub.holdContributorForQuota(c, pushHoldTask("myorg/repo", 461), reason) {
		t.Fatal("setup: the failure must book a hold")
	}
	if hub.holdContributorForQuota(c, pushHoldTask("myorg/repo", 462), reason) {
		t.Fatal("a second quota failure inside the hold must not book another")
	}
	if err := c.send(WSMessage{Type: "ping", Seq: 99}); err != nil {
		t.Fatalf("send marker: %v", err)
	}

	notice := readWire(t, client)
	if notice["type"] != "notice" {
		t.Fatalf("want a notice frame, got %v", notice)
	}
	text, _ := notice["message"].(string)
	for _, want := range []string{"out of quota", reset.Format(time.RFC3339), "the reset time the provider gave", "backend", "wait"} {
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

	// With no reset to read, the notice names the time the hold ends and no
	// duration: it is repeated with every refusal while the hold stands.
	guessed := contributorQuotaHold{Count: 2, Until: time.Now().Add(time.Hour)}
	guess := contributorQuotaHoldNotice(guessed)
	if !strings.Contains(guess, guessed.Until.UTC().Format(time.RFC3339)) || !strings.Contains(guess, "that is when it tries you again") {
		t.Errorf("a guessed hold must say when the hub tries again, got %q", guess)
	}
	if strings.Contains(guess, "minutes") || strings.Contains(guess, "hour") {
		t.Errorf("the notice must not state a duration that goes stale, got %q", guess)
	}
}

// TestQuotaHold_HeldRelayDoesNotCover: a relay the hub holds for quota is not
// counted as covering its lane, at once, whether or not it reported a quota
// hold itself.
func TestQuotaHold_HeldRelayDoesNotCover(t *testing.T) {
	hub, _, relay, _, read := noWorkRelay(t)
	if got := coverageAt(hub, relay, time.Now()); len(got) == 0 {
		t.Fatal("setup: the relay must cover its lane before the hold")
	}
	hub.holdContributorForQuota(relay, nil, quotaReasonNoReset)
	if f := read(); f["type"] != "notice" {
		t.Fatalf("setup: booking the hold must send the notice, got %v", f)
	}
	if got := coverageAt(hub, relay, time.Now()); got != nil {
		t.Fatalf("a quota-held relay counted as cover: RoleCoverage = %+v, want nil", got)
	}
}

// TestStandbyDispatchSkipsHeldRelays: standby dispatch does not hand an item
// to a relay the hub is holding, for provider quota or for the item's
// repository. TestStandbyAutoDispatchAssignsThroughHoldMarkedPath is the
// control: the same relay with no hold is assigned.
func TestStandbyDispatchSkipsHeldRelays(t *testing.T) {
	holds := map[string]func(*ContributeWSHub, *ContributorConnection){
		"quota hold": func(hub *ContributeWSHub, c *ContributorConnection) {
			hub.bookContributorQuotaHold(c, quotaReasonNoReset, time.Now())
		},
		"push hold for the item's repository": func(hub *ContributeWSHub, c *ContributorConnection) {
			hub.bookContributorRepoPushHold(c, "alice/repo", pushHoldReasonFailed, time.Now())
		},
	}
	for name, book := range holds {
		t.Run(name, func(t *testing.T) {
			s, conn := standbyAutoDispatchHarness(t, standbyAutoDispatchConfig(true, "T2", 1))
			hub := s.contributeHub
			hub.mu.RLock()
			var c *ContributorConnection
			for _, candidate := range hub.connections {
				c = candidate
			}
			hub.mu.RUnlock()
			if c == nil {
				t.Fatal("no live standby relay")
			}
			book(hub, c)

			hub.AutoDispatchStandby([]string{"quality"})
			conn.SetReadDeadline(testReadDeadline())
			if _, raw, err := conn.ReadMessage(); err == nil {
				t.Fatalf("a held relay was sent a standby assignment: %s", raw)
			}
		})
	}
}

// TestQuotaHold_ContributorsListCarriesActiveHolds: GET /api/contributors
// (the Contributors card and the admin MCP's contributors_list) lists each
// contributor's active quota holds, and only those.
func TestQuotaHold_ContributorsListCarriesActiveHolds(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	heldID, _ := registerTestUser(t, s, "quota-held")
	lapsedID, _ := registerTestUser(t, s, "quota-lapsed")
	hub := s.contributeHub
	now := time.Now()

	profile := &ContributorProfile{GitHubUsername: "quota-held", ContributorID: heldID}
	hub.bookContributorQuotaHold(&ContributorConnection{profile: profile, session: "claude"}, "You've hit your weekly limit · resets in 5h", now)
	hub.bookContributorQuotaHold(&ContributorConnection{profile: profile, session: "agy"}, quotaReasonNoReset, now)
	hub.bookContributorQuotaHold(&ContributorConnection{profile: &ContributorProfile{GitHubUsername: "quota-lapsed", ContributorID: lapsedID}},
		quotaReasonNoReset, now.Add(-time.Hour))

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
	if got := byLogin["quota-lapsed"].QuotaHolds; len(got) != 0 {
		t.Fatalf("a lapsed hold must not be listed, got %+v", got)
	}
	got := byLogin["quota-held"].QuotaHolds
	if len(got) != 2 || got[0].Session != "agy" || got[1].Session != "claude" {
		t.Fatalf("want the two active holds sorted by session, got %+v", got)
	}
	if got[0].ResetStated || got[0].Until != now.Add(30*time.Minute).UTC().Format(time.RFC3339) {
		t.Fatalf("a guessed hold must say so and end on its first rung, got %+v", got[0])
	}
	claude := got[1]
	if !claude.ResetStated || claude.Count != 1 || claude.Reason == "" ||
		claude.HeldAt != now.UTC().Format(time.RFC3339) || claude.Until != now.Add(5*time.Hour).UTC().Format(time.RFC3339) {
		t.Fatalf("the row must carry reason, held_at, until and count, got %+v", claude)
	}
	p, err := loadContributorProfile("quota-held")
	if err != nil {
		t.Fatalf("load quota-held: %v", err)
	}
	if len(p.QuotaHolds) != 0 {
		t.Fatalf("holds must not be persisted on the profile, got %+v", p.QuotaHolds)
	}
}

// TestContributorQuotaHoldsCard: the Contributors card renders the active
// quota holds from quota_holds, escaped, and nothing when there are none.
func TestContributorQuotaHoldsCard(t *testing.T) {
	html := indexHTML(t)
	render := jsFunctionBody(t, html, "function renderContributors(contributors)")
	if !strings.Contains(render, "${contributorQuotaHolds(c)}") {
		t.Fatal("contributor cards do not render the quota holds")
	}
	if !strings.Contains(html, ".contrib-quota-hold {") {
		t.Fatal("the quota hold row has no style")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH: quota hold rendering was NOT executed")
	}
	js := jsFunctionBody(t, html, "function esc(s)") + "\n" + jsFunctionBody(t, html, "function contributorQuotaHolds(c)") + `
function assert(ok, message) { if (!ok) throw new Error(message); }
function formatDashboardDateTime(value) { return 'T(' + value + ')'; }
for (const c of [{}, {quota_holds: null}, {quota_holds: []}, {quota_holds: 'x'}]) {
  assert(contributorQuotaHolds(c) === '', 'no holds should render nothing');
}
const holds = [
  {reason: 'You have hit your weekly limit', count: 1, held_at: '2026-11-28T10:00:00Z', until: '2026-12-01T15:00:00Z', reset_stated: true, session: 'claude'},
  {reason: 'budget exceeded', count: 3, held_at: '2026-11-28T11:00:00Z', until: '2026-11-28T13:00:00Z'},
];
const before = JSON.stringify(holds);
const out = contributorQuotaHolds({quota_holds: holds});
assert((out.match(/class="contrib-quota-hold"/g) || []).length === 2, out);
assert(out.includes('Out of provider quota until T(2026-12-01T15:00:00Z)'), out);
assert(out.includes('session claude'), out);
assert(out.includes('You have hit your weekly limit'), out);
assert(out.includes('the reset the provider gave'), out);
assert(out.includes('Out of provider quota until T(2026-11-28T13:00:00Z) (hold 3)'), out);
assert(out.includes('no reset time the hive could read'), out);
assert(JSON.stringify(holds) === before, 'render mutated source records');
const unsafe = contributorQuotaHolds({quota_holds: [{reason: '" onmouseover="evil', until: '<script>', count: '<b>', session: '<img>'}]});
assert(!unsafe.includes('<img>') && !unsafe.includes('<script>') && !unsafe.includes('<b>') && !unsafe.includes('" onmouseover="'), unsafe);
`
	if out, err := exec.Command(node, "-e", js).CombinedOutput(); err != nil {
		t.Fatalf("quota hold rendering failed: %v\n%s", err, out)
	}
}
