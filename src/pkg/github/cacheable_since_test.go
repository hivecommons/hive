package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// These tests pin the ETag fix for moving since= cutoffs: a sweep that built
// its since from time.Now() changed the URL on every pass, so the ETag cache
// (keyed on the full URL) never matched and every pass was charged. Rounded
// down to the hour, every pass in that hour asks for the same URL and the
// repeat gets a free 304.

func TestCacheableSince(t *testing.T) {
	if got := cacheableSince(time.Time{}); !got.IsZero() {
		t.Fatalf("zero since = %v, want zero (no filter)", got)
	}
	east := time.FixedZone("UTC+5:30", 5*3600+1800)
	cases := []struct {
		in, want time.Time
	}{
		{time.Date(2026, 11, 7, 19, 42, 17, 500, time.UTC), time.Date(2026, 11, 7, 19, 0, 0, 0, time.UTC)},
		{time.Date(2026, 11, 7, 19, 0, 0, 0, time.UTC), time.Date(2026, 11, 7, 19, 0, 0, 0, time.UTC)},
		{time.Date(2026, 11, 7, 19, 59, 59, 999, time.UTC), time.Date(2026, 11, 7, 19, 0, 0, 0, time.UTC)},
		// A half-hour zone still rounds to the UTC hour, so two hives in
		// different zones ask for the same URL.
		{time.Date(2026, 11, 8, 1, 12, 0, 0, east), time.Date(2026, 11, 7, 19, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		got := cacheableSince(tc.in)
		if !got.Equal(tc.want) || got.Location() != time.UTC {
			t.Errorf("cacheableSince(%v) = %v, want %v in UTC", tc.in, got, tc.want)
		}
		if got.After(tc.in) || tc.in.Sub(got) >= time.Hour {
			t.Errorf("cacheableSince(%v) = %v: must be earlier by less than an hour", tc.in, got)
		}
	}
}

// etagListServer serves fixed JSON bodies by path with an ETag, and 304 when
// the client presents it. It records the since= and status of every request
// to the paths it watches.
type etagListServer struct {
	t      *testing.T
	bodies map[string]any
	watch  string

	mu   sync.Mutex
	seen []etagListRequest
}

type etagListRequest struct {
	since  string
	status int
}

func (s *etagListServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, ok := s.bodies[r.URL.Path]
	if r.Method != http.MethodGet || !ok {
		s.t.Errorf("unexpected operation: %s %s", r.Method, r.URL.String())
		w.WriteHeader(http.StatusNotFound)
		return
	}
	etag := `W/"` + r.URL.Path + `"`
	status := http.StatusOK
	if r.Header.Get("If-None-Match") == etag {
		status = http.StatusNotModified
	}
	if r.URL.Path == s.watch {
		s.mu.Lock()
		s.seen = append(s.seen, etagListRequest{since: r.URL.Query().Get("since"), status: status})
		s.mu.Unlock()
	}
	w.Header().Set("ETag", etag)
	if status == http.StatusNotModified {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(body)
}

func (s *etagListServer) requests() []etagListRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]etagListRequest(nil), s.seen...)
}

// assertCacheablePasses checks three passes: the second (same hour) asks for
// the first's exact URL and is answered 304; the third (next hour) moves
// since forward to that hour.
func assertCacheablePasses(t *testing.T, got []etagListRequest, sameHour, nextHour string) {
	t.Helper()
	if len(got) != 3 {
		t.Fatalf("watched list requests = %+v, want one per pass (3)", got)
	}
	if got[0].since != sameHour || got[0].status != http.StatusOK {
		t.Errorf("pass 1 = %+v, want since=%s answered 200", got[0], sameHour)
	}
	if got[1].since != got[0].since || got[1].status != http.StatusNotModified {
		t.Errorf("pass 2 in the same hour = %+v, want since=%s (identical URL) answered 304", got[1], got[0].since)
	}
	if got[2].since != nextHour || got[2].status != http.StatusOK {
		t.Errorf("pass 3 after the hour = %+v, want since=%s answered 200", got[2], nextHour)
	}
}

func TestSweepIssueUnparkCommands_CommentListSinceIsCacheable(t *testing.T) {
	t0 := time.Date(2026, 11, 7, 19, 5, 13, 0, time.UTC)
	srv := &etagListServer{t: t, watch: "/repos/org/repo/issues/7/comments", bodies: map[string]any{
		"/repos/org/repo/issues": []*gh.Issue{{
			Number:           gh.Ptr(7),
			Comments:         gh.Ptr(1),
			UpdatedAt:        &gh.Timestamp{Time: t0.Add(-time.Hour)},
			PullRequestLinks: &gh.PullRequestLinks{URL: gh.Ptr("pr")},
		}},
		"/repos/org/repo/issues/7/comments": []*gh.IssueComment{{
			ID:        gh.Ptr(int64(1)),
			User:      &gh.User{Login: gh.Ptr("owner")},
			Body:      gh.Ptr("looks good"),
			CreatedAt: &gh.Timestamp{Time: t0.Add(-2 * time.Hour)},
		}},
	}}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	client := newTestClient(t, ts, "org", []string{"repo"})
	client.SetAppBotLogin("hive[bot]")

	for _, now := range []time.Time{t0, t0.Add(40 * time.Minute), t0.Add(time.Hour)} {
		opts := IssueUnparkSweepOptions{
			Now: func() time.Time { return now },
			PRApproveReply: func(context.Context, string, int, *gh.IssueComment) (string, error) {
				t.Fatal("no /hive approve command was posted")
				return "", nil
			},
		}
		if _, err := client.SweepIssueUnparkCommands(context.Background(), opts); err != nil {
			t.Fatalf("sweep at %v: %v", now, err)
		}
		// The item never changes, so the sweep's per-item read cache would
		// answer passes 2 and 3 without any request at all
		// (issue_unpark_read_cache.go). Drop it: this test is about the URL
		// a read sends when one is needed.
		client.unparkReads.prune("org", "repo", nil)
	}
	assertCacheablePasses(t, srv.requests(), "2026-10-31T19:00:00Z", "2026-10-31T20:00:00Z")
}

func TestSweepClosedIssueStageLabels_ListSinceIsCacheable(t *testing.T) {
	srv := &etagListServer{t: t, watch: "/repos/org/repo/issues", bodies: map[string]any{
		"/repos/org/repo/issues": []*gh.Issue{},
	}}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	client := newTestClient(t, ts, "org", []string{"repo"})

	// The scheduler passes lastRun-10m, which moves on every 10-minute pass.
	for _, since := range []time.Time{
		time.Date(2026, 11, 7, 19, 5, 13, 0, time.UTC),
		time.Date(2026, 11, 7, 19, 15, 13, 0, time.UTC),
		time.Date(2026, 11, 7, 20, 5, 13, 0, time.UTC),
	} {
		if _, err := client.SweepClosedIssueStageLabels(context.Background(), ClosedIssueStageSweepOptions{Since: since}); err != nil {
			t.Fatalf("sweep since %v: %v", since, err)
		}
	}
	assertCacheablePasses(t, srv.requests(), "2026-11-07T19:00:00Z", "2026-11-07T20:00:00Z")
}
