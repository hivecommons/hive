package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// oneActionableIssueN installs a status with a single actionable issue whose
// number/url are keyed off n, so successive selectTask passes are handed DIFFERENT
// issues (the same issue would be skipped once held/active). Author is someone else
// so the #2390 own-work ordering does not skew selection. Mirrors oneActionableIssue
// in contribute_ws_trust_controls_test.go.
func oneActionableIssueN(s *Server, n int) {
	s.statusMu.Lock()
	s.status = &StatusPayload{
		Repos: []FrontendRepo{
			{
				Name: "repo1",
				Full: "myorg/repo1",
				ActionableIssues: []any{
					map[string]any{
						"number": float64(1000 + n),
						"title":  "Actionable issue",
						"url":    "https://github.com/myorg/repo1/issues/1000",
						"author": "someone-else",
					},
				},
			},
		},
	}
	s.statusMu.Unlock()
}

// TestRateLimits_HourlyEnforced (#2566): a contributor at tier_limits.max_per_hour
// for their tier is refused the next assignment with an hourly_limit negative-ack,
// mirroring the concurrency_limit gate. One assignment under the cap is admitted.
func TestRateLimits_HourlyEnforced(t *testing.T) {
	hub, s := covK2Hub(t)

	// max_per_hour: 2, and a high max_concurrent/max_per_day so ONLY the hourly cap
	// can trip. currentTask is cleared between passes so concurrency never binds.
	s.deps.Config.Hub.TierLimits = map[string]config.TierRate{
		"newcomer": {MaxPerHour: 2, MaxPerDay: 100, MaxConcurrent: 100},
	}

	conn := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "newcomer"},
		lastPong: time.Now(),
	}

	// Two assignments are admitted (0 -> 1 -> at cap after the 2nd).
	for i := 0; i < 2; i++ {
		oneActionableIssueN(s, i)
		msg := hub.selectTask(conn)
		if msg == nil || msg.Type != "task_assign" {
			t.Fatalf("assignment %d under max_per_hour=2 should be served, got %+v", i+1, msg)
		}
		conn.currentTask = nil // release the concurrency hold for the next pass
	}

	// Third assignment: identity now has 2 == max_per_hour in the trailing hour, so
	// it is refused with hourly_limit rather than silently handed work.
	oneActionableIssueN(s, 2)
	msg := hub.selectTask(conn)
	if msg == nil {
		t.Fatalf("expected an hourly-limit negative-ack, got nil (silent)")
	}
	if msg.Type != "task_unavailable" || msg.Reason != taskUnavailableHourlyLimit {
		t.Fatalf("expected task_unavailable/%s, got type=%q reason=%q",
			taskUnavailableHourlyLimit, msg.Type, msg.Reason)
	}

	// Fail-before shape: raising max_per_hour admits the same identity again.
	s.deps.Config.Hub.TierLimits["newcomer"] = config.TierRate{MaxPerHour: 3, MaxPerDay: 100, MaxConcurrent: 100}
	oneActionableIssueN(s, 3)
	if msg := hub.selectTask(conn); msg == nil || msg.Type != "task_assign" {
		t.Fatalf("with max_per_hour raised to 3 the next assignment should be served, got %+v", msg)
	}
}

// TestRateLimits_DailyEnforced (#2566): a contributor at tier_limits.max_per_day is
// refused with a daily_limit negative-ack. Seeds the assignment ledger directly to
// keep the test independent of how many issues are enumerated.
func TestRateLimits_DailyEnforced(t *testing.T) {
	hub, s := covK2Hub(t)

	// High hourly cap so only the daily cap can trip; max_per_day: 3.
	s.deps.Config.Hub.TierLimits = map[string]config.TierRate{
		"newcomer": {MaxPerHour: 100, MaxPerDay: 3, MaxConcurrent: 100},
	}

	conn := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "bob", ContributorID: "c-bob", TrustTier: "newcomer"},
		lastPong: time.Now(),
	}

	// Seed 3 assignments spread across the last day but OUTSIDE the trailing hour, so
	// the hourly window is empty and only the daily cap is at its limit.
	now := time.Now()
	hub.assignmentTimes[identityOf(conn)] = []time.Time{
		now.Add(-20 * time.Hour),
		now.Add(-10 * time.Hour),
		now.Add(-3 * time.Hour),
	}

	oneActionableIssueN(s, 0)
	msg := hub.selectTask(conn)
	if msg == nil {
		t.Fatalf("expected a daily-limit negative-ack, got nil (silent)")
	}
	if msg.Type != "task_unavailable" || msg.Reason != taskUnavailableDailyLimit {
		t.Fatalf("expected task_unavailable/%s, got type=%q reason=%q",
			taskUnavailableDailyLimit, msg.Type, msg.Reason)
	}

	// Fail-before shape: raising max_per_day admits the identity again.
	s.deps.Config.Hub.TierLimits["newcomer"] = config.TierRate{MaxPerHour: 100, MaxPerDay: 4, MaxConcurrent: 100}
	oneActionableIssueN(s, 1)
	if msg := hub.selectTask(conn); msg == nil || msg.Type != "task_assign" {
		t.Fatalf("with max_per_day raised to 4 the assignment should be served, got %+v", msg)
	}
}

// TestRateLimits_RollingWindowReset (#2566): the windows are ROLLING, not calendar
// buckets. An assignment older than the hour window no longer counts against
// max_per_hour, and one older than the day window no longer counts against
// max_per_day and is pruned from the ledger.
func TestRateLimits_RollingWindowReset(t *testing.T) {
	hub, _ := covK2Hub(t)

	conn := &ContributorConnection{
		profile: &ContributorProfile{GitHubUsername: "carol", ContributorID: "c-carol", TrustTier: "newcomer"},
	}
	id := identityOf(conn)
	now := time.Now()

	// One assignment 90 minutes ago (outside the hour window, inside the day window)
	// and one 25 hours ago (outside BOTH windows).
	hub.assignmentTimes[id] = []time.Time{
		now.Add(-90 * time.Minute),
		now.Add(-25 * time.Hour),
	}

	hour, day := hub.rateWindowCounts(id, now)
	if hour != 0 {
		t.Fatalf("assignment 90m ago must not count in the trailing hour, got hour=%d", hour)
	}
	if day != 1 {
		t.Fatalf("only the 90m-ago assignment falls inside the day window, got day=%d", day)
	}

	// The 25h-ago entry is beyond the widest window and must have been pruned.
	if got := len(hub.assignmentTimes[id]); got != 1 {
		t.Fatalf("expected the >24h assignment to be pruned, ledger len=%d", got)
	}

	// A fresh identity with no history counts zero on both windows.
	if h, d := hub.rateWindowCounts("nobody", now); h != 0 || d != 0 {
		t.Fatalf("unknown identity should count zero, got hour=%d day=%d", h, d)
	}
}

// TestRateLimits_ZeroMeansUnlimited (#2566): a limit <= 0 disables that window (the
// "advisor" tier defaults every field to 0), so the identity is never refused on a
// zero cap regardless of history.
func TestRateLimits_ZeroMeansUnlimited(t *testing.T) {
	hub, s := covK2Hub(t)

	s.deps.Config.Hub.TierLimits = map[string]config.TierRate{
		"advisor": {MaxPerHour: 0, MaxPerDay: 0, MaxConcurrent: 0},
	}

	conn := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "dave", ContributorID: "c-dave", TrustTier: "advisor"},
		lastPong: time.Now(),
	}

	// Pre-load a large history; with all caps at 0 it must be ignored.
	now := time.Now()
	var seeded []time.Time
	for i := 0; i < 50; i++ {
		seeded = append(seeded, now.Add(-time.Duration(i)*time.Minute))
	}
	hub.assignmentTimes[identityOf(conn)] = seeded

	oneActionableIssueN(s, 0)
	if msg := hub.selectTask(conn); msg == nil || msg.Type != "task_assign" {
		t.Fatalf("with all tier limits 0 (unlimited) the assignment should be served, got %+v", msg)
	}
}
