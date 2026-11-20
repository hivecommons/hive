package github

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func historyTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// useTempReworkCache points the rework cache at a scratch file for the test.
func useTempReworkCache(t *testing.T) {
	t.Helper()
	old := PRReworkCachePath
	PRReworkCachePath = filepath.Join(t.TempDir(), PRReworkCacheFile)
	t.Cleanup(func() { PRReworkCachePath = old })
}

// useHistoryAt gives c a fresh store read from path (not the process-wide
// registry), which is what a restart does.
func useHistoryAt(c *Client, path string) *attributedPRHistory {
	h := loadAttributedPRHistory(path, historyTestLogger())
	c.attributedHistory.Store(h)
	return h
}

func historyNumbers(prs []PullRequest) []int {
	var out []int
	for _, pr := range prs {
		out = append(out, pr.Number)
	}
	return out
}

// A PR closed 20 days ago drops out of the bounded live scan; the store keeps
// it, and a restart (reload from disk) still has it.
func TestAttributedPRHistoryKeepsClosedPRPastLiveScanAndRestart(t *testing.T) {
	useTempReworkCache(t)
	now := time.Now().UTC()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/repos/o/r/pulls/") || strings.HasPrefix(r.URL.Path, "/repos/o/r/issues/") {
			writeJSON(t, w, []map[string]any{})
			return
		}
		if r.URL.Path != "/repos/o/r/pulls" {
			t.Fatalf("unexpected request: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		switch r.URL.Query().Get("state") {
		case "open":
			writeJSON(t, w, []map[string]any{})
		case "closed":
			pr := attributedClosedPR(7, now.Add(-20*24*time.Hour))
			pr["created_at"] = now.Add(-22 * 24 * time.Hour).Format(time.RFC3339)
			pr["merged_at"] = now.Add(-20 * 24 * time.Hour).Format(time.RFC3339)
			writeJSON(t, w, []map[string]any{pr})
		default:
			t.Fatalf("unexpected state query: %q", r.URL.Query().Get("state"))
		}
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), AttributedPRHistoryFile)
	c := NewClientForTest(srv.URL, "o", []string{"r"}, historyTestLogger())
	useHistoryAt(c, path)

	// A 30-day lookback lists the PR once...
	t.Setenv(attributedClosedPRLookbackEnv, "30d")
	if _, _, _, _, attributed, _, _, err := c.fetchPRs(context.Background(), "r", nil); err != nil || len(attributed) != 1 {
		t.Fatalf("first fetchPRs: attributed=%v err=%v, want #7 listed", historyNumbers(attributed), err)
	}
	// ...the default 14-day scan no longer does.
	t.Setenv(attributedClosedPRLookbackEnv, "14d")
	_, _, _, _, attributed, _, _, err := c.fetchPRs(context.Background(), "r", nil)
	if err != nil {
		t.Fatalf("second fetchPRs: %v", err)
	}
	if len(attributed) != 0 {
		t.Fatalf("live attributed = %v, want the 14-day scan to drop #7", historyNumbers(attributed))
	}
	stored, _ := c.AttributedPRHistory()
	if len(stored) != 1 || stored[0].Number != 7 || stored[0].MergedAt.IsZero() || stored[0].HiveModel != "auto" {
		t.Fatalf("stored = %+v, want merged #7 with its attribution", stored)
	}
	c.syncAttributedPRHistory(context.Background(), []string{"r"})

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("history file not written: %v", err)
	}
	if strings.Contains(string(raw), "-- hive:") || strings.Contains(string(raw), `"body"`) {
		t.Fatalf("history file holds a PR body: %s", raw)
	}

	restarted := NewClientForTest(srv.URL, "o", []string{"r"}, historyTestLogger())
	useHistoryAt(restarted, path)
	stored, _ = restarted.AttributedPRHistory()
	if len(stored) != 1 || stored[0].Number != 7 || stored[0].HiveBackend != "bob" {
		t.Fatalf("after restart stored = %+v, want #7", stored)
	}
}

// A store that cannot be written only warns: enumeration still returns the
// repo's attributed PRs.
func TestAttributedPRHistoryWriteErrorDoesNotFailEnumeration(t *testing.T) {
	useTempReworkCache(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") == "closed" {
			writeJSON(t, w, []map[string]any{attributedClosedPR(7, time.Now().Add(-time.Hour))})
			return
		}
		writeJSON(t, w, []map[string]any{})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, []map[string]any{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// The store's directory is a regular file, so every write fails.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newTestClient(t, srv, "o", []string{"r"})
	useHistoryAt(c, filepath.Join(blocker, AttributedPRHistoryFile))

	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("a history write error failed enumeration: %v", err)
	}
	if got := historyNumbers(result.PRs.Attributed); len(got) != 1 || got[0] != 7 {
		t.Fatalf("attributed = %v, want #7", got)
	}
	if err := c.attributedPRHistory().flush(time.Now()); err == nil {
		t.Fatal("flush into a file-as-directory succeeded; the test did not exercise a write error")
	}
}

// backfillTestServer serves a closed-PR list sorted by creation over
// len(pages) pages, plus empty rework listings, and counts every request.
type backfillTestServer struct {
	mu        sync.Mutex
	listPages []string
	rework    int
	other     []string
}

func (b *backfillTestServer) handler(t *testing.T, pages [][]map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		switch {
		case r.URL.Path == "/repos/o/r/pulls" && r.URL.Query().Get("state") == "closed" && r.URL.Query().Get("sort") == "created":
			if got := r.URL.Query().Get("direction"); got != "desc" {
				t.Errorf("backfill direction = %q, want desc", got)
			}
			page := r.URL.Query().Get("page")
			if page == "" {
				page = "1"
			}
			b.listPages = append(b.listPages, page)
			n, _ := strconv.Atoi(page)
			if n < 1 || n > len(pages) {
				t.Errorf("backfill asked for page %q of %d", page, len(pages))
				writeJSON(t, w, []map[string]any{})
				return
			}
			if n < len(pages) {
				w.Header().Set("Link", `<`+srvURL(r)+`/repos/o/r/pulls?state=closed&sort=created&direction=desc&page=`+strconv.Itoa(n+1)+`>; rel="next"`)
			}
			writeJSON(t, w, pages[n-1])
		case strings.HasPrefix(r.URL.Path, "/repos/o/r/pulls/") || strings.HasPrefix(r.URL.Path, "/repos/o/r/issues/"):
			b.rework++
			writeJSON(t, w, []map[string]any{})
		default:
			b.other = append(b.other, r.URL.Path+"?"+r.URL.RawQuery)
			writeJSON(t, w, []map[string]any{})
		}
	}
}

func (b *backfillTestServer) take() (pages []string, rework int, other []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	pages, rework, other = b.listPages, b.rework, b.other
	b.listPages, b.rework, b.other = nil, 0, nil
	return pages, rework, other
}

func backfillMergedPR(number int, created time.Time) map[string]any {
	pr := attributedClosedPR(number, created.Add(time.Hour))
	pr["created_at"] = created.Format(time.RFC3339)
	pr["merged_at"] = created.Add(time.Hour).Format(time.RFC3339)
	return pr
}

// The backfill spends at most its per-pass page budget, resumes from the
// saved cursor after a restart, stops at the horizon, marks itself complete
// and then makes no calls at all.
func TestAttributedPRBackfillBudgetResumeAndComplete(t *testing.T) {
	useTempReworkCache(t)
	now := time.Now().UTC()
	// No rate-limit evidence from other tests may gate the backfill.
	resetRESTAccountingForTest(t, now)
	day := 24 * time.Hour
	pages := [][]map[string]any{
		{backfillMergedPR(30, now.Add(-16*day))},
		{backfillMergedPR(20, now.Add(-20*day))},
		{backfillMergedPR(10, now.Add(-25*day)), backfillMergedPR(5, now.Add(-45*day))},
	}
	fake := &backfillTestServer{}
	srv := httptest.NewServer(fake.handler(t, pages))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), AttributedPRHistoryFile)
	c := NewClientForTest(srv.URL, "o", []string{"r"}, historyTestLogger())
	h := useHistoryAt(c, path)
	// The live scan has run once (14-day floor): the backfill's anchor.
	h.noteScan("r", now, now.Add(-14*day), false)

	c.syncAttributedPRHistory(context.Background(), []string{"r"})
	gotPages, rework, other := fake.take()
	if strings.Join(gotPages, ",") != "1,2" {
		t.Fatalf("pass 1 listed pages %v, want the per-pass budget of %d pages (1,2)", gotPages, attributedPRBackfillPagesPerPass)
	}
	if rework == 0 || len(other) != 0 {
		t.Fatalf("pass 1 rework calls=%d other=%v, want rework measured and nothing else", rework, other)
	}

	// Restart: the cursor comes back from disk.
	h = useHistoryAt(c, path)
	if st := h.file.Repos["r"]; st == nil || st.Backfill.NextPage != 3 || st.Backfill.Listed || st.Backfill.Done {
		t.Fatalf("persisted backfill = %+v, want cursor at page 3, not listed", st)
	}
	c.syncAttributedPRHistory(context.Background(), []string{"r"})
	gotPages, _, other = fake.take()
	if strings.Join(gotPages, ",") != "3" || len(other) != 0 {
		t.Fatalf("pass 2 listed pages %v other=%v, want only page 3", gotPages, other)
	}
	st := h.file.Repos["r"]
	if !st.Backfill.Listed || st.Backfill.ReachedEnd || !st.Backfill.Done {
		t.Fatalf("backfill after pass 2 = %+v, want listed to the horizon and done", st.Backfill)
	}
	stored, cov := c.AttributedPRHistory()
	if got := historyNumbers(stored); len(got) != 3 || got[0] != 10 || got[1] != 20 || got[2] != 30 {
		t.Fatalf("stored = %v, want #10 #20 #30 (#5 is past the horizon)", got)
	}
	for _, pr := range stored {
		if pr.Rework.TimeToMergeMinutes != 60 {
			t.Fatalf("stored #%d rework = %+v, want it measured", pr.Number, pr.Rework)
		}
	}
	if !cov.Available || cov.Full || cov.Since.After(now.Add(-29*day)) {
		t.Fatalf("coverage = %+v, want it to reach the 30-day horizon", cov)
	}

	for i := 0; i < 3; i++ {
		c.syncAttributedPRHistory(context.Background(), []string{"r"})
	}
	if gotPages, rework, other := fake.take(); len(gotPages) != 0 || rework != 0 || len(other) != 0 {
		t.Fatalf("finished backfill made calls: pages=%v rework=%d other=%v", gotPages, rework, other)
	}
}

// Outside the normal API budget the backfill makes no call and leaves its
// cursor where it was.
func TestAttributedPRBackfillRespectsAPIBudget(t *testing.T) {
	useTempReworkCache(t)
	now := time.Now().UTC()
	resetRESTAccountingForTest(t, now)
	fake := &backfillTestServer{}
	srv := httptest.NewServer(fake.handler(t, [][]map[string]any{{backfillMergedPR(1, now.Add(-20*24*time.Hour))}}))
	defer srv.Close()

	c := NewClientForTest(srv.URL, "o", []string{"r"}, historyTestLogger())
	c.rateLimits.now = func() time.Time { return now }
	c.rateLimits.mu.Lock()
	if c.rateLimits.windows == nil {
		c.rateLimits.windows = map[string]rateLimitWindow{}
	}
	c.rateLimits.windows[githubCoreRateLimitResource] = rateLimitWindow{limit: 5000, remaining: 500, reset: now.Add(time.Hour), observedAt: now}
	c.rateLimits.mu.Unlock()
	if mode, _ := c.APIBudgetMode(); mode == APIBudgetNormal {
		t.Fatal("test setup: API budget mode is normal")
	}
	h := useHistoryAt(c, filepath.Join(t.TempDir(), AttributedPRHistoryFile))
	h.noteScan("r", now, now.Add(-14*24*time.Hour), false)

	c.syncAttributedPRHistory(context.Background(), []string{"r"})
	if gotPages, rework, other := fake.take(); len(gotPages) != 0 || rework != 0 || len(other) != 0 {
		t.Fatalf("backfill spent calls outside the normal budget: pages=%v rework=%d other=%v", gotPages, rework, other)
	}
	if bf := h.file.Repos["r"].Backfill; bf.NextPage != 1 || bf.Listed || bf.Done {
		t.Fatalf("backfill = %+v, want the cursor still at page 1", bf)
	}
}

// Coverage follows the run of live scans: the first scan's floor, the
// backfill horizon once done, and a fresh floor after a gap nobody listed.
func TestAttributedPRHistoryCoverageTracksScanContinuity(t *testing.T) {
	now := time.Now().UTC()
	day := 24 * time.Hour
	h := loadAttributedPRHistory(filepath.Join(t.TempDir(), AttributedPRHistoryFile), historyTestLogger())
	if _, cov := h.snapshot([]string{"r"}, now); !cov.Available || cov.Full || !cov.Since.Equal(now) {
		t.Fatalf("coverage before any scan = %+v, want nothing covered", cov)
	}
	h.noteScan("r", now.Add(-2*day), now.Add(-16*day), false)
	h.noteScan("r", now.Add(-day), now.Add(-15*day), false)
	if _, cov := h.snapshot([]string{"r"}, now); !cov.Since.Equal(now.Add(-16 * day)) {
		t.Fatalf("coverage = %+v, want the run's first floor", cov)
	}
	st := h.file.Repos["r"]
	st.Backfill = attributedPRBackfillState{StartedAt: now.Add(-day), Horizon: now.Add(-31 * day), Listed: true, Done: true}
	if _, cov := h.snapshot([]string{"r"}, now); !cov.Since.Equal(now.Add(-31 * day)) {
		t.Fatalf("coverage = %+v, want the backfill horizon", cov)
	}
	// Twenty days with no scan: the next floor is past the last scan.
	later := now.Add(20 * day)
	h.noteScan("r", later, later.Add(-14*day), false)
	if _, cov := h.snapshot([]string{"r"}, later); !cov.Since.Equal(later.Add(-14 * day)) {
		t.Fatalf("coverage after a gap = %+v, want the new run's floor", cov)
	}
	h.noteScan("r", later.Add(time.Minute), time.Time{}, true)
	if _, cov := h.snapshot([]string{"r"}, later); !cov.Full {
		t.Fatalf("coverage = %+v, want full once a scan lists the whole history", cov)
	}
}

func TestAttributedPRHistoryPrunesPastRetention(t *testing.T) {
	now := time.Now().UTC()
	h := loadAttributedPRHistory(filepath.Join(t.TempDir(), AttributedPRHistoryFile), historyTestLogger())
	old := PullRequest{Repo: "r", Number: 1, HiveAttributed: true, State: "closed", CreatedAt: now.Add(-attributedPRHistoryRetention - 24*time.Hour), ClosedAt: now.Add(-attributedPRHistoryRetention)}
	recent := PullRequest{Repo: "r", Number: 2, HiveAttributed: true, State: "closed", CreatedAt: now.Add(-48 * time.Hour), ClosedAt: now.Add(-24 * time.Hour)}
	h.upsert([]PullRequest{old, recent}, nil, now)
	h.noteScan("r", now, time.Time{}, true)
	if err := h.flush(now); err != nil {
		t.Fatal(err)
	}
	stored, cov := h.snapshot([]string{"r"}, now)
	if got := historyNumbers(stored); len(got) != 1 || got[0] != 2 {
		t.Fatalf("stored = %v, want only #2 kept", got)
	}
	if cov.Full || cov.Since.Before(now.Add(-attributedPRHistoryRetention-time.Minute)) {
		t.Fatalf("coverage = %+v, want it to start at the retention cutoff", cov)
	}
}
