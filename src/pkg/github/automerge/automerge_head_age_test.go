package automerge

// Tests for the min_head_age head-time source (hivecommons/hive#12491): a PR
// updated_at bump from a label/comment write on the same head must not reset
// head age, while a new head SHA must. No test sleeps; time advances by
// moving the injected clock.

import (
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

func headAgeTestPR(updated time.Time) *gh.PullRequest {
	return &gh.PullRequest{UpdatedAt: &gh.Timestamp{Time: updated}}
}

func TestHeadPushedAtIgnoresUpdatedAtBumpOnSameHead(t *testing.T) {
	pushed := time.Date(2026, 10, 16, 16, 38, 40, 0, time.UTC)
	clock := pushed.Add(time.Minute)
	c := &Engine{now: func() time.Time { return clock }}
	tracker := NewHeadFirstSeenTracker()

	if got := c.headPushedAtWith(tracker, "acme", "widget", 7, "sha1", headAgeTestPR(pushed)); !got.Equal(pushed) {
		t.Fatalf("first observation = %v, want updated_at %v", got, pushed)
	}

	// The stage-label engine writes a label and a comment 50 minutes later;
	// GitHub bumps updated_at but the head is unchanged.
	clock = pushed.Add(50 * time.Minute)
	bumped := clock.Add(-time.Second)
	if got := c.headPushedAtWith(tracker, "acme", "widget", 7, "sha1", headAgeTestPR(bumped)); !got.Equal(pushed) {
		t.Fatalf("after label/comment bump = %v, want original %v", got, pushed)
	}
	if age := clock.Sub(c.headPushedAtWith(tracker, "acme", "widget", 7, "sha1", headAgeTestPR(bumped))); age < 49*time.Minute {
		t.Fatalf("head age reset to %v by updated_at bump", age)
	}
}

func TestHeadPushedAtResetsOnNewHeadSHA(t *testing.T) {
	pushed := time.Date(2026, 10, 16, 16, 38, 40, 0, time.UTC)
	clock := pushed.Add(time.Minute)
	c := &Engine{now: func() time.Time { return clock }}
	tracker := NewHeadFirstSeenTracker()

	c.headPushedAtWith(tracker, "acme", "widget", 7, "sha1", headAgeTestPR(pushed))

	clock = pushed.Add(30 * time.Minute)
	repushed := clock.Add(-10 * time.Second)
	if got := c.headPushedAtWith(tracker, "acme", "widget", 7, "sha2", headAgeTestPR(repushed)); !got.Equal(repushed) {
		t.Fatalf("new head = %v, want fresh %v", got, repushed)
	}
	// A different PR with the same SHA string is tracked independently.
	if got := c.headPushedAtWith(tracker, "acme", "widget", 8, "sha1", headAgeTestPR(repushed)); !got.Equal(repushed) {
		t.Fatalf("other PR = %v, want %v", got, repushed)
	}
}

func TestHeadPushedAtZeroAndEmptySHA(t *testing.T) {
	clock := time.Date(2026, 10, 16, 17, 0, 0, 0, time.UTC)
	c := &Engine{now: func() time.Time { return clock }}
	tracker := NewHeadFirstSeenTracker()

	// Unknown updated_at keeps the gate's skip and does not pin the head.
	if got := c.headPushedAtWith(tracker, "acme", "widget", 7, "sha1", &gh.PullRequest{}); !got.IsZero() {
		t.Fatalf("zero updated_at = %v, want zero", got)
	}
	later := clock.Add(-time.Minute)
	if got := c.headPushedAtWith(tracker, "acme", "widget", 7, "sha1", headAgeTestPR(later)); !got.Equal(later) {
		t.Fatalf("after zero observation = %v, want %v", got, later)
	}
	// No head SHA: fall back to updated_at without recording.
	if got := c.headPushedAtWith(tracker, "acme", "widget", 9, "", headAgeTestPR(later)); !got.Equal(later) {
		t.Fatalf("empty sha = %v, want %v", got, later)
	}
}

func TestHeadFirstSeenTrackerPrunesStaleHeads(t *testing.T) {
	tracker := NewHeadFirstSeenTracker()
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	tracker.observe("acme/widget#7@old", start, start)
	later := start.Add(headFirstSeenPruneAfter + time.Hour)
	tracker.observe("acme/widget#8@new", later, later)
	if _, ok := tracker.firstSeen["acme/widget#7@old"]; ok {
		t.Fatal("stale head not pruned")
	}
	if got := tracker.observe("acme/widget#7@old", later, later); !got.Equal(later) {
		t.Fatalf("re-observed pruned head = %v, want %v", got, later)
	}
}
