package github

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestAttributedClosedPRStoreKeepsPRsThePastScanNoLongerLists(t *testing.T) {
	t.Setenv(attributedClosedPRLookbackEnv, "14d")
	now := time.Now().UTC()
	scan := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scan++
		if scan == 1 {
			writeJSON(t, w, []map[string]any{
				attributedClosedPR(1, now.Add(-2*24*time.Hour)),
				attributedClosedPR(2, now.Add(-13*24*time.Hour)),
			})
			return
		}
		// Later scans no longer list #2: it aged out of the live window.
		writeJSON(t, w, []map[string]any{attributedClosedPR(1, now.Add(-time.Hour)), attributedClosedPR(3, now.Add(-20*24*time.Hour))})
	}))
	defer srv.Close()

	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := c.fetchAttributedClosedPRs(context.Background(), "o", "r", "r"); err != nil {
		t.Fatal(err)
	}
	prs, err := c.fetchAttributedClosedPRs(context.Background(), "o", "r", "r")
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]bool{}
	for _, pr := range prs {
		got[pr.Number] = true
	}
	if len(prs) != 2 || !got[1] || !got[2] {
		t.Fatalf("PRs = %+v, want #1 (fresh) and #2 (stored); #3 is past the lookback", prs)
	}
	// A new client over the same store file still has them: restarts keep history.
	c2 := NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c2.attributedPRStoreFile = c.attributedPRStoreFile
	if stored := storedAttributedClosedPRs(c2.attributedPRStorePath(), "r"); len(stored) != 2 {
		t.Fatalf("stored after restart = %+v, want 2", stored)
	}
}

func TestAttributedClosedPRBackfillIsPagedAndResumes(t *testing.T) {
	t.Setenv(attributedClosedPRLookbackEnv, "14d")
	t.Setenv(attributedPRBackfillEnv, "30d")
	now := time.Now().UTC()
	// 12 pages of one PR each, newest first: pages 1-2 inside 14d, the rest
	// older, with page 11 the first past the 30d horizon.
	age := func(page int) time.Duration {
		switch {
		case page <= 2:
			return time.Duration(page) * 24 * time.Hour
		case page <= 10:
			return time.Duration(14+page) * 24 * time.Hour
		default:
			return 40 * 24 * time.Hour
		}
	}
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page == 0 {
			page = 1
		}
		if page < 12 {
			w.Header().Set("Link", `<`+srvURL(r)+`/repos/o/r/pulls?state=closed&page=`+strconv.Itoa(page+1)+`>; rel="next"`)
		}
		writeJSON(t, w, []map[string]any{attributedClosedPR(page, now.Add(-age(page)))})
	}))
	defer srv.Close()

	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	prs, err := c.fetchAttributedClosedPRs(context.Background(), "o", "r", "r")
	if err != nil {
		t.Fatal(err)
	}
	// Head: pages 1-3 (page 3 is past 14d). Backfill budget: 5 pages from 3.
	if calls != 3+attributedPRBackfillPagesPerTick {
		t.Fatalf("first tick calls = %d, want head 3 + backfill budget %d", calls, attributedPRBackfillPagesPerTick)
	}
	if st := attributedPRStoreBackfillState(c.attributedPRStorePath(), "r"); st.Done || st.NextPage != 3+attributedPRBackfillPagesPerTick {
		t.Fatalf("backfill state after tick 1 = %+v, want unfinished, resume at page %d", st, 3+attributedPRBackfillPagesPerTick)
	}
	if len(prs) != 2+attributedPRBackfillPagesPerTick {
		t.Fatalf("PRs after tick 1 = %d, want %d", len(prs), 2+attributedPRBackfillPagesPerTick)
	}

	calls = 0
	if _, err := c.fetchAttributedClosedPRs(context.Background(), "o", "r", "r"); err != nil {
		t.Fatal(err)
	}
	st := attributedPRStoreBackfillState(c.attributedPRStorePath(), "r")
	if !st.Done {
		t.Fatalf("backfill state after tick 2 = %+v, want done", st)
	}
	stored := storedAttributedClosedPRs(c.attributedPRStorePath(), "r")
	if len(stored) != 10 {
		t.Fatalf("stored = %d PRs, want the 10 within 30d", len(stored))
	}

	// Steady state: no backfill calls any more.
	calls = 0
	if _, err := c.fetchAttributedClosedPRs(context.Background(), "o", "r", "r"); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("steady-state calls = %d, want 3 (head scan only)", calls)
	}
}

func TestMergeAttributedClosedPRsPrefersFresh(t *testing.T) {
	fresh := []PullRequest{{Number: 1, Title: "new"}}
	stored := []PullRequest{{Number: 1, Title: "old"}, {Number: 2}}
	got := mergeAttributedClosedPRs(fresh, stored)
	if len(got) != 2 || got[0].Title != "new" {
		t.Fatalf("merged = %+v", got)
	}
}
