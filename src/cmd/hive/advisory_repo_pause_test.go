package main

import (
	"context"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// A repo-paused refusal from the poster (#6203) is a SKIP: nothing is
// recorded as posted, no error is raised, no App banner hook fires, and only
// the pacing hook runs so the "skipped: repo paused" line lands once per
// update interval. The moment the poster stops refusing, the very next
// due cycle posts and records normally.
func TestPublishAdvisoryDigest_RepoPausedIsSkipNotFailure(t *testing.T) {
	r := &publishRecorder{githubErr: &github.RepoPausedError{Repo: "acme/widget", By: "clubanderson", Reason: "freeze"}}
	got := publishAdvisoryDigest(context.Background(), "# digest", twoFindingDigest(), githubRoute(), r.deps(), restoreTestLogger())
	if got != advisoryPublishSkipped {
		t.Fatalf("outcome = %v, want skipped", got)
	}
	if len(r.github) != 1 {
		t.Fatalf("poster must be consulted (it owns the pause check): calls = %v", r.github)
	}
	if len(r.posted) != 0 || len(r.errors) != 0 || r.proven != 0 || r.forbidden != 0 || r.authFail != 0 || len(r.backoffs) != 0 {
		t.Fatalf("paused skip leaked into other hooks: %+v", r)
	}
	if len(r.pausedFor) != 1 || r.pausedFor[0] != "acme/widget" {
		t.Fatalf("recordRepoPaused = %v, want [acme/widget]", r.pausedFor)
	}

	// Pause lifted → posts.
	r.githubErr = nil
	got = publishAdvisoryDigest(context.Background(), "# digest", twoFindingDigest(), githubRoute(), r.deps(), restoreTestLogger())
	if got != advisoryPublishPosted || len(r.posted) != 1 || r.proven != 1 {
		t.Fatalf("after unpause: outcome=%v recorder=%+v", got, r)
	}
}

// With update_interval_s set, a paused skip paces like a post: one attempt
// (and one log line) per interval, and the first due cycle after the pause
// lifts posts. Fake clock throughout.
func TestAdvisoryPostGate_PausedSkipPacesPerInterval(t *testing.T) {
	gate := advisory.NewPostGate()
	advCfg := config.AdvisoryConfig{UpdateIntervalS: 300}
	interval := advCfg.UpdateInterval()
	now := time.Date(2026, 10, 28, 23, 22, 0, 0, time.UTC)
	repo := "acme/widget"
	logger := restoreTestLogger()

	paused := true
	attempts := 0
	cycle := func() bool {
		if !gate.Due(advCfg, repo, now, logger) {
			return false
		}
		attempts++
		gate.RecordSuccess(repo, now) // what recordRepoPaused / recordPosted both do
		return !paused
	}

	// Paused: 10 one-minute cycles across two intervals → exactly 2 attempts.
	for i := 0; i < 10; i++ {
		if posted := cycle(); posted {
			t.Fatalf("cycle %d posted while paused", i)
		}
		now = now.Add(time.Minute)
	}
	wantAttempts := 1 + int((9*time.Minute)/interval)
	if attempts != wantAttempts {
		t.Fatalf("paused attempts = %d, want %d (one per %s)", attempts, wantAttempts, interval)
	}

	// Lift the pause: the next due cycle posts, no later than one interval on.
	paused = false
	posted := false
	for i := 0; i < int(interval/time.Minute)+1 && !posted; i++ {
		posted = cycle()
		now = now.Add(time.Minute)
	}
	if !posted {
		t.Fatalf("pause lifted but no post within one interval (%s)", interval)
	}
}
