package dashboard

import (
	"testing"
	"time"
)

// #6450 suite: a contributor whose agent runtime dies at startup keeps
// claiming assignments and insta-failing them; the hub books issue cooldowns
// and the contributor's standing degrades, but nothing contributor-visible
// says why. These tests pin the fast-failure streak detector: after
// contributorFailureStreakThreshold consecutive hub-measured sub-minute
// failures, selectTask refuses the identity's next claim with an explicit
// contributor_failure_streak negative-ack (reason + human message) for
// contributorFailureStreakPause, and the streak resets on a completion or a
// genuinely-attempted (slow) failure.

func streakConn(id string) *ContributorConnection {
	return &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: id, ContributorID: id, TrustTier: "contributor"},
		lastPong: time.Now(),
	}
}

func streakIssue(number int) map[string]any {
	return map[string]any{
		"number": float64(number),
		"title":  "streak test issue",
		"url":    "https://github.com/myorg/repo1/issues/" + itoa(number),
		"author": "someone",
	}
}

// bookFastFailures books n sub-minute failures for the identity.
func bookFastFailures(hub *ContributeWSHub, identity string, n int) {
	for i := 0; i < n; i++ {
		hub.recordContributorFastFailure(identity, 5*time.Second, "Provider is not configured")
	}
}

// TestFailureStreak_PausesClaimsWithExplicitReason: the reported #6450 shape —
// three consecutive seconds-long failures — must convert the next claim into a
// contributor_failure_streak refusal that names the streak and the expiry,
// instead of assigning (and then burning) a fourth issue.
func TestFailureStreak_PausesClaimsWithExplicitReason(t *testing.T) {
	hub, s := covK2Hub(t)
	setStatusIssues(s, streakIssue(71))
	c := streakConn("ct-streak")

	bookFastFailures(hub, identityOf(c), contributorFailureStreakThreshold)

	msg := hub.selectTask(c)
	if msg == nil || msg.Type != "task_unavailable" {
		t.Fatalf("want task_unavailable, got %+v", msg)
	}
	if msg.Reason != taskUnavailableFailureStreak {
		t.Fatalf("want reason %q, got %q", taskUnavailableFailureStreak, msg.Reason)
	}
	if msg.Message == "" {
		t.Fatal("want a human-readable message naming the streak and pause expiry, got empty")
	}
}

// TestFailureStreak_BelowThresholdStillOffers: two fast failures are not a
// verdict — the third assignment must still be offered.
func TestFailureStreak_BelowThresholdStillOffers(t *testing.T) {
	hub, s := covK2Hub(t)
	setStatusIssues(s, streakIssue(72))
	c := streakConn("ct-streak-2")

	bookFastFailures(hub, identityOf(c), contributorFailureStreakThreshold-1)

	msg := hub.selectTask(c)
	if msg == nil || msg.Type != "task_assign" {
		t.Fatalf("want task_assign below threshold, got %+v", msg)
	}
}

// TestFailureStreak_SlowFailureResets: a failure that took longer than the
// fast-failure bound proves the runtime ran — the streak must reset, and the
// per-issue cooldown machinery (untouched here) remains the right tool.
func TestFailureStreak_SlowFailureResets(t *testing.T) {
	hub, s := covK2Hub(t)
	setStatusIssues(s, streakIssue(73))
	c := streakConn("ct-streak-3")

	bookFastFailures(hub, identityOf(c), contributorFailureStreakThreshold-1)
	hub.recordContributorFastFailure(identityOf(c), contributorFastFailureMax+time.Second, "task failed on its merits")
	bookFastFailures(hub, identityOf(c), 1)

	msg := hub.selectTask(c)
	if msg == nil || msg.Type != "task_assign" {
		t.Fatalf("want task_assign after slow-failure reset, got %+v", msg)
	}
}

// TestFailureStreak_CompletionResets: a genuine completion clears the streak.
func TestFailureStreak_CompletionResets(t *testing.T) {
	hub, s := covK2Hub(t)
	setStatusIssues(s, streakIssue(74))
	c := streakConn("ct-streak-4")

	bookFastFailures(hub, identityOf(c), contributorFailureStreakThreshold)
	hub.resetContributorFailureStreak(identityOf(c))

	msg := hub.selectTask(c)
	if msg == nil || msg.Type != "task_assign" {
		t.Fatalf("want task_assign after completion reset, got %+v", msg)
	}
}

// TestFailureStreak_PauseExpiresIntoOneProbe: after the pause window lapses
// the identity gets a probe assignment again; a further fast failure re-arms
// the pause immediately (streak count persists above threshold).
func TestFailureStreak_PauseExpiresIntoOneProbe(t *testing.T) {
	hub, s := covK2Hub(t)
	setStatusIssues(s, streakIssue(75))
	c := streakConn("ct-streak-5")
	id := identityOf(c)

	bookFastFailures(hub, id, contributorFailureStreakThreshold)

	// Age the streak's last failure past the pause window.
	hub.failureStreakMu.Lock()
	rec := hub.contributorFailureStreaks[id]
	rec.LastAt = time.Now().Add(-(contributorFailureStreakPause + time.Second))
	hub.contributorFailureStreaks[id] = rec
	hub.failureStreakMu.Unlock()

	if msg := hub.selectTask(c); msg == nil || msg.Type != "task_assign" {
		t.Fatalf("want probe task_assign after pause expiry, got %+v", msg)
	}

	// The probe insta-fails too: the pause must re-arm at once.
	bookFastFailures(hub, id, 1)
	msg := hub.selectTask(c)
	if msg == nil || msg.Type != "task_unavailable" || msg.Reason != taskUnavailableFailureStreak {
		t.Fatalf("want re-armed contributor_failure_streak refusal, got %+v", msg)
	}
}

// TestFailureStreak_UnknownDurationNotCounted: a failure with no hub-measured
// duration (adopted task, taskAssignedAt zero) must not advance the streak.
func TestFailureStreak_UnknownDurationNotCounted(t *testing.T) {
	hub, s := covK2Hub(t)
	setStatusIssues(s, streakIssue(76))
	c := streakConn("ct-streak-6")

	for i := 0; i < contributorFailureStreakThreshold+2; i++ {
		hub.recordContributorFastFailure(identityOf(c), 0, "unknown duration")
	}

	msg := hub.selectTask(c)
	if msg == nil || msg.Type != "task_assign" {
		t.Fatalf("want task_assign (unknown durations not counted), got %+v", msg)
	}
}

// TestFailureStreak_ScopedToIdentity: one contributor's dying runtime must not
// pause anyone else's claims.
func TestFailureStreak_ScopedToIdentity(t *testing.T) {
	hub, s := covK2Hub(t)
	setStatusIssues(s, streakIssue(77))
	broken := streakConn("ct-broken")
	healthy := streakConn("ct-healthy")

	bookFastFailures(hub, identityOf(broken), contributorFailureStreakThreshold)

	msg := hub.selectTask(healthy)
	if msg == nil || msg.Type != "task_assign" {
		t.Fatalf("want task_assign for healthy contributor, got %+v", msg)
	}
}
