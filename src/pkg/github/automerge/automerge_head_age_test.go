package automerge

// Tests for the min_head_age head-time source (hivecommons/hive#12491): a PR
// updated_at bump from a label/comment write on the same head must not reset
// head age, while a new head SHA must. No test sleeps; time advances by
// moving the injected clock.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

func headAgeTestPR(updated time.Time) *gh.PullRequest {
	return &gh.PullRequest{UpdatedAt: &gh.Timestamp{Time: updated}}
}

func TestHeadPushedAtIgnoresUpdatedAtBumpOnSameHead(t *testing.T) {
	pushed := time.Date(2026, 10, 16, 16, 38, 40, 0, time.UTC)
	clock := pushed.Add(time.Minute)
	c := &Engine{now: func() time.Time { return clock }}
	tracker := NewHeadFirstSeenTracker()

	if got := c.headPushedAtWith(context.Background(), tracker, "acme", "widget", 7, "sha1", headAgeTestPR(pushed)); !got.Equal(pushed) {
		t.Fatalf("first observation = %v, want updated_at %v", got, pushed)
	}

	// The stage-label engine writes a label and a comment 50 minutes later;
	// GitHub bumps updated_at but the head is unchanged.
	clock = pushed.Add(50 * time.Minute)
	bumped := clock.Add(-time.Second)
	if got := c.headPushedAtWith(context.Background(), tracker, "acme", "widget", 7, "sha1", headAgeTestPR(bumped)); !got.Equal(pushed) {
		t.Fatalf("after label/comment bump = %v, want original %v", got, pushed)
	}
	if age := clock.Sub(c.headPushedAtWith(context.Background(), tracker, "acme", "widget", 7, "sha1", headAgeTestPR(bumped))); age < 49*time.Minute {
		t.Fatalf("head age reset to %v by updated_at bump", age)
	}
}

func TestHeadPushedAtResetsOnNewHeadSHA(t *testing.T) {
	pushed := time.Date(2026, 10, 16, 16, 38, 40, 0, time.UTC)
	clock := pushed.Add(time.Minute)
	c := &Engine{now: func() time.Time { return clock }}
	tracker := NewHeadFirstSeenTracker()

	c.headPushedAtWith(context.Background(), tracker, "acme", "widget", 7, "sha1", headAgeTestPR(pushed))

	clock = pushed.Add(30 * time.Minute)
	repushed := clock.Add(-10 * time.Second)
	if got := c.headPushedAtWith(context.Background(), tracker, "acme", "widget", 7, "sha2", headAgeTestPR(repushed)); !got.Equal(repushed) {
		t.Fatalf("new head = %v, want fresh %v", got, repushed)
	}
	// A different PR with the same SHA string is tracked independently.
	if got := c.headPushedAtWith(context.Background(), tracker, "acme", "widget", 8, "sha1", headAgeTestPR(repushed)); !got.Equal(repushed) {
		t.Fatalf("other PR = %v, want %v", got, repushed)
	}
}

func TestHeadPushedAtZeroAndEmptySHA(t *testing.T) {
	clock := time.Date(2026, 10, 16, 17, 0, 0, 0, time.UTC)
	c := &Engine{now: func() time.Time { return clock }}
	tracker := NewHeadFirstSeenTracker()

	// Unknown updated_at keeps the gate's skip and does not pin the head.
	if got := c.headPushedAtWith(context.Background(), tracker, "acme", "widget", 7, "sha1", &gh.PullRequest{}); !got.IsZero() {
		t.Fatalf("zero updated_at = %v, want zero", got)
	}
	later := clock.Add(-time.Minute)
	if got := c.headPushedAtWith(context.Background(), tracker, "acme", "widget", 7, "sha1", headAgeTestPR(later)); !got.Equal(later) {
		t.Fatalf("after zero observation = %v, want %v", got, later)
	}
	// No head SHA: fall back to updated_at without recording.
	if got := c.headPushedAtWith(context.Background(), tracker, "acme", "widget", 9, "", headAgeTestPR(later)); !got.Equal(later) {
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

// hivecommons/hive#12557: the head is hours old, but the first sweep to see
// it ran right after a hive stage-regressed comment bumped updated_at. Head
// age must come from the head commit, so the gate does not report "head
// pushed 54s ago", and the commit lookup is cached per head.
func TestHeadPushedAtUsesHeadCommitWhenUpdatedAtIsFresh(t *testing.T) {
	committed := time.Date(2026, 10, 16, 17, 15, 0, 0, time.UTC)
	var commitGets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/git/commits/73708b4":
			commitGets.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sha":"73708b4","committer":{"name":"c","date":"2026-10-16T17:15:00Z"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	clock := time.Date(2026, 10, 16, 20, 45, 0, 0, time.UTC)
	minHeadAge := 10 * time.Minute
	e := New(hgithub.NewClient("token", "acme", []string{"widget"}, nil, srv.URL), Options{
		Now:           func() time.Time { return clock },
		MinHeadAge:    minHeadAge,
		HeadFirstSeen: NewHeadFirstSeenTracker(),
	})
	pr := &gh.PullRequest{
		CreatedAt: &gh.Timestamp{Time: committed.Add(-time.Hour)},
		UpdatedAt: &gh.Timestamp{Time: clock.Add(-54 * time.Second)},
	}

	got := e.headPushedAt(context.Background(), "acme", "widget", 12423, "73708b4", pr)
	if !got.Equal(committed) {
		t.Fatalf("headPushedAt = %v, want head commit time %v", got, committed)
	}
	if age := clock.Sub(got); age < minHeadAge {
		t.Fatalf("head age %v still under min_head_age %v", age, minHeadAge)
	}

	// Another hive comment bumps updated_at again on the same head.
	clock = clock.Add(6 * time.Minute)
	pr.UpdatedAt = &gh.Timestamp{Time: clock.Add(-51 * time.Second)}
	if got := e.headPushedAt(context.Background(), "acme", "widget", 12423, "73708b4", pr); !got.Equal(committed) {
		t.Fatalf("after bump headPushedAt = %v, want %v", got, committed)
	}
	if n := commitGets.Load(); n != 1 {
		t.Fatalf("head commit fetched %d times, want 1 (cached per head)", n)
	}
}

// A committer date older than the PR (an old branch just opened) does not
// make a brand-new PR look settled: the commit time is clamped to created_at.
func TestHeadPushedAtClampsCommitTimeToPRCreation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sha":"old","committer":{"name":"c","date":"2026-01-01T00:00:00Z"}}`))
	}))
	defer srv.Close()
	clock := time.Date(2026, 10, 16, 20, 45, 0, 0, time.UTC)
	created := clock.Add(-2 * time.Minute)
	e := New(hgithub.NewClient("token", "acme", []string{"widget"}, nil, srv.URL), Options{
		Now:           func() time.Time { return clock },
		MinHeadAge:    10 * time.Minute,
		HeadFirstSeen: NewHeadFirstSeenTracker(),
	})
	pr := &gh.PullRequest{CreatedAt: &gh.Timestamp{Time: created}, UpdatedAt: &gh.Timestamp{Time: clock.Add(-time.Minute)}}
	if got := e.headPushedAt(context.Background(), "acme", "widget", 5, "old", pr); !got.Equal(created) {
		t.Fatalf("headPushedAt = %v, want PR created_at %v", got, created)
	}
}

// A failed commit lookup keeps the per-head pinned updated_at: it can only
// over-hold, and later bumps on the same head still cannot restart it.
func TestHeadPushedAtFallsBackToPinWhenCommitLookupFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	clock := time.Date(2026, 10, 16, 20, 45, 0, 0, time.UTC)
	e := New(hgithub.NewClient("token", "acme", []string{"widget"}, nil, srv.URL), Options{
		Now:           func() time.Time { return clock },
		MinHeadAge:    10 * time.Minute,
		HeadFirstSeen: NewHeadFirstSeenTracker(),
	})
	first := clock.Add(-time.Minute)
	pr := &gh.PullRequest{UpdatedAt: &gh.Timestamp{Time: first}}
	if got := e.headPushedAt(context.Background(), "acme", "widget", 6, "sha", pr); !got.Equal(first) {
		t.Fatalf("fallback = %v, want pinned %v", got, first)
	}
	clock = clock.Add(5 * time.Minute)
	pr.UpdatedAt = &gh.Timestamp{Time: clock.Add(-10 * time.Second)}
	if got := e.headPushedAt(context.Background(), "acme", "widget", 6, "sha", pr); !got.Equal(first) {
		t.Fatalf("after bump fallback = %v, want pinned %v", got, first)
	}
}
