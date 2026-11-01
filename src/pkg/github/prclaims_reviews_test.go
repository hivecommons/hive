package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// reviewScanServer is mergedClaimServer plus a /pulls/{n}/reviews endpoint.
// reviews maps a PR number to its review pages (page 1 first); a page past
// the last one, or a PR absent from the map, serves an empty list. failing
// PR numbers answer the reviews call with a 500. calls counts reviews
// requests per PR number.
type reviewScanServer struct {
	*httptest.Server
	mu      sync.Mutex
	calls   map[int]int
	failing map[int]bool
}

func (s *reviewScanServer) reviewCalls(number int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[number]
}

func (s *reviewScanServer) setFailing(number int, fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing[number] = fail
}

func newReviewScanServer(t *testing.T, closed []map[string]any, reviews map[int][][]map[string]any) *reviewScanServer {
	t.Helper()
	s := &reviewScanServer{calls: map[int]int{}, failing: map[int]bool{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/reviews") {
			var number int
			if _, err := fmt.Sscanf(r.URL.Path, "/repos/torch-spyre/spyre-inference/pulls/%d/reviews", &number); err != nil {
				t.Errorf("unexpected reviews path %q", r.URL.Path)
				http.NotFound(w, r)
				return
			}
			s.mu.Lock()
			s.calls[number]++
			fail := s.failing[number]
			s.mu.Unlock()
			if fail {
				http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
				return
			}
			page := 1
			if p := r.URL.Query().Get("page"); p != "" {
				_, _ = fmt.Sscanf(p, "%d", &page)
			}
			pages := reviews[number]
			if page < len(pages) {
				next := *r.URL
				q := next.Query()
				q.Set("page", fmt.Sprint(page+1))
				next.RawQuery = q.Encode()
				w.Header().Set("Link", fmt.Sprintf(`<%s%s>; rel="next"`, s.URL, next.RequestURI()))
			}
			if page >= 1 && page <= len(pages) {
				_ = json.NewEncoder(w).Encode(pages[page-1])
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{})
			return
		}
		if r.URL.Query().Get("state") == "closed" {
			_ = json.NewEncoder(w).Encode(closed)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	}))
	t.Cleanup(s.Close)
	return s
}

func reviewFixture(id int64, login, state string, submittedAt time.Time) map[string]any {
	m := map[string]any{
		"id":    id,
		"user":  map[string]any{"login": login},
		"state": state,
	}
	if !submittedAt.IsZero() {
		m["submitted_at"] = submittedAt.Format(time.RFC3339)
	}
	return m
}

// TestFetchClaimScan_ObservesOtherPeoplesReviewsOnTerminalPRs pins #16116:
// the settle scan reads a newly terminal PR's reviews once and reports every
// submitted review the hive did not write — a person's as human, a [bot]'s
// as other_automation — at its own submitted_at. The hive's own review is
// skipped (agent_pr_reviewed already counts it) and a pending review with no
// submitted_at is not a review yet. A second scan of the same window serves
// the reviews from cache instead of reading GitHub again.
func TestFetchClaimScan_ObservesOtherPeoplesReviewsOnTerminalPRs(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	merged := now.Add(-2 * time.Hour)
	closed := now.Add(-90 * time.Minute)
	t1, t2, t3, t4, t5 := now.Add(-5*time.Hour), now.Add(-4*time.Hour), now.Add(-3*time.Hour), now.Add(-150*time.Minute), now.Add(-100*time.Minute)

	unmerged := mergedPR(801, "alice", "abandoned", "", time.Time{}, closed)
	unmerged["closed_at"] = closed.Format(time.RFC3339)
	srv := newReviewScanServer(t, []map[string]any{
		mergedPR(800, "hive-app[bot]", "fix", "Fixes #300", merged, merged),
		unmerged,
	}, map[int][][]map[string]any{
		800: {
			// Page 1 / page 2: the reviews listing is paginated.
			{
				reviewFixture(1, "alice", "APPROVED", t1),
				reviewFixture(2, "bob", "COMMENTED", t2),
				reviewFixture(3, "renovate[bot]", "CHANGES_REQUESTED", t3),
			},
			{
				reviewFixture(4, "hive-app[bot]", "APPROVED", t4),
				reviewFixture(5, "carol", "PENDING", time.Time{}),
			},
		},
		801: {{reviewFixture(6, "dave", "DISMISSED", t5)}},
	})
	c := NewClientForTest(srv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())
	var got []PRReviewObservation
	c.SetPRReviewObservedHook(func(o PRReviewObservation) { got = append(got, o) })
	identity := HiveIdentity{AppLogin: "hive-app[bot]"}

	if _, err := c.FetchClaimScan(context.Background(), identity); err != nil {
		t.Fatalf("FetchClaimScan: %v", err)
	}
	want := []PRReviewObservation{
		{Repo: "spyre-inference", Number: 800, ReviewID: 1, Reviewer: "alice", Attribution: "human", State: "APPROVED", SubmittedAt: t1},
		{Repo: "spyre-inference", Number: 800, ReviewID: 2, Reviewer: "bob", Attribution: "human", State: "COMMENTED", SubmittedAt: t2},
		{Repo: "spyre-inference", Number: 800, ReviewID: 3, Reviewer: "renovate[bot]", Attribution: "other_automation", State: "CHANGES_REQUESTED", SubmittedAt: t3},
		{Repo: "spyre-inference", Number: 801, ReviewID: 6, Reviewer: "dave", Attribution: "human", State: "DISMISSED", SubmittedAt: t5},
	}
	assertReviewObservations(t, got, want)

	got = nil
	if _, err := c.FetchClaimScan(context.Background(), identity); err != nil {
		t.Fatalf("second FetchClaimScan: %v", err)
	}
	assertReviewObservations(t, got, want)
	// Two pages for #800 on the first scan, one for #801; none after.
	for n, wantCalls := range map[int]int{800: 2, 801: 1} {
		if calls := srv.reviewCalls(n); calls != wantCalls {
			t.Errorf("PR #%d reviews requests = %d after two scans, want %d (the second scan must read the cache)", n, calls, wantCalls)
		}
	}
}

func assertReviewObservations(t *testing.T, got, want []PRReviewObservation) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("review observations = %+v, want %d: %+v", got, len(want), want)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Repo != w.Repo || g.Number != w.Number || g.ReviewID != w.ReviewID || g.Reviewer != w.Reviewer ||
			g.Attribution != w.Attribution || g.State != w.State || !g.SubmittedAt.Equal(w.SubmittedAt) {
			t.Errorf("observation[%d] = %+v, want %+v", i, g, w)
		}
	}
}

// Without a review hook nobody consumes the reviews, so the scan must not
// spend a GitHub request reading them.
func TestFetchClaimScan_NoReviewHookReadsNoReviews(t *testing.T) {
	merged := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	srv := newReviewScanServer(t, []map[string]any{
		mergedPR(810, "hive-app[bot]", "fix", "Fixes #1", merged, merged),
	}, map[int][][]map[string]any{810: {{reviewFixture(1, "alice", "APPROVED", merged)}}})
	c := NewClientForTest(srv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())

	if _, err := c.FetchClaimScan(context.Background(), HiveIdentity{AppLogin: "hive-app[bot]"}); err != nil {
		t.Fatalf("FetchClaimScan: %v", err)
	}
	if calls := srv.reviewCalls(810); calls != 0 {
		t.Fatalf("reviews requests = %d with no hook installed, want 0", calls)
	}
}

// A failed reviews read is best effort: it does not fail the claim scan,
// emits nothing for that PR, and is not cached — the next scan retries.
func TestFetchClaimScan_ReviewReadFailureIsRetriedNotCached(t *testing.T) {
	merged := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	srv := newReviewScanServer(t, []map[string]any{
		mergedPR(820, "hive-app[bot]", "fix", "Fixes #2", merged, merged),
	}, map[int][][]map[string]any{820: {{reviewFixture(9, "alice", "APPROVED", merged)}}})
	srv.setFailing(820, true)
	c := NewClientForTest(srv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())
	var got []PRReviewObservation
	c.SetPRReviewObservedHook(func(o PRReviewObservation) { got = append(got, o) })
	identity := HiveIdentity{AppLogin: "hive-app[bot]"}

	if _, err := c.FetchClaimScan(context.Background(), identity); err != nil {
		t.Fatalf("FetchClaimScan must not fail on a reviews read error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("observations after a failed read = %+v, want none", got)
	}
	srv.setFailing(820, false)
	if _, err := c.FetchClaimScan(context.Background(), identity); err != nil {
		t.Fatalf("second FetchClaimScan: %v", err)
	}
	if len(got) != 1 || got[0].ReviewID != 9 || got[0].Attribution != "human" {
		t.Fatalf("observations after the retry = %+v, want alice's review 9", got)
	}
}
