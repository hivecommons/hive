package github

import (
	"context"
	"fmt"
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

	gh "github.com/google/go-github/v72/github"
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
	if err := c.attributedPRHistory().flush(time.Now(), nil); err == nil {
		t.Fatal("flush into a file-as-directory succeeded; the test did not exercise a write error")
	}
}

// attributedBackfillFake serves a closed-PR list sorted by creation over
// len(pages) pages, plus empty rework listings, and counts every request.
type attributedBackfillFake struct {
	mu        sync.Mutex
	listPages []string
	rework    int
	other     []string
}

func (b *attributedBackfillFake) handler(t *testing.T, pages [][]map[string]any) http.HandlerFunc {
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

func (b *attributedBackfillFake) take() (pages []string, rework int, other []string) {
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
	fake := &attributedBackfillFake{}
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
	if st := h.file.Repos["r"]; st == nil || st.Backfill.NextPage != 3 || st.Backfill.LastPage != 2 || st.Backfill.Listed || st.Backfill.Done {
		t.Fatalf("persisted backfill = %+v, want cursor at page 3 after page 2, not listed", st)
	}
	c.syncAttributedPRHistory(context.Background(), []string{"r"})
	gotPages, _, other = fake.take()
	// The resumed walk re-reads the page it read last, then goes on.
	if strings.Join(gotPages, ",") != "2,3" || len(other) != 0 {
		t.Fatalf("pass 2 listed pages %v other=%v, want page 2 re-read, then page 3", gotPages, other)
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
	fake := &attributedBackfillFake{}
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
	h.upsert([]PullRequest{recent}, nil, now)
	// #1 was stored while young and has since aged past the retention.
	h.file.Records["r#1"] = attributedPRRecordFrom(old)
	// An open record not seen within the retention (its close was never
	// observed) goes too; one seen recently stays.
	h.file.Records["r#3"] = &attributedPRRecord{Repo: "r", Number: 3, State: "open", LastSeenAt: now.Add(-attributedPRHistoryRetention - time.Hour)}
	h.file.Records["r#4"] = &attributedPRRecord{Repo: "r", Number: 4, State: "open", LastSeenAt: now.Add(-time.Hour)}
	h.noteScan("r", now, time.Time{}, true)
	if err := h.flush(now, nil); err != nil {
		t.Fatal(err)
	}
	if h.file.Records["r#3"] != nil || h.file.Records["r#4"] == nil {
		t.Fatalf("open records after prune: #3=%v #4=%v, want the stale one dropped", h.file.Records["r#3"], h.file.Records["r#4"])
	}
	stored, cov := h.snapshot([]string{"r"}, now)
	if got := historyNumbers(stored); len(got) != 1 || got[0] != 2 {
		t.Fatalf("stored = %v, want only #2 kept", got)
	}
	if cov.Full || cov.Since.Before(now.Add(-attributedPRHistoryRetention-time.Minute)) {
		t.Fatalf("coverage = %+v, want it to start at the retention cutoff", cov)
	}
}

// Only an error about the PR itself costs it a rework try; a GitHub hiccup or
// a budget/cancellation error leaves it pending.
func TestAttributedPRBackfillReworkTriesCountOnlyPerPRErrors(t *testing.T) {
	useTempReworkCache(t)
	now := time.Now().UTC()
	resetRESTAccountingForTest(t, now)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/repos/o/r/pulls/2/"):
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		case strings.HasPrefix(r.URL.Path, "/repos/o/r/pulls/1/"):
			http.Error(w, `{"message":"unavailable"}`, http.StatusServiceUnavailable)
		default:
			writeJSON(t, w, []map[string]any{})
		}
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "o", []string{"r"}, historyTestLogger())
	h := useHistoryAt(c, filepath.Join(t.TempDir(), AttributedPRHistoryFile))
	merged := func(n int, created time.Time) PullRequest {
		return PullRequest{Repo: "r", Number: n, HiveAttributed: true, State: "closed", CreatedAt: created, ClosedAt: created.Add(time.Hour), MergedAt: created.Add(time.Hour)}
	}
	// #2 is newer, so it is measured first.
	h.upsert([]PullRequest{merged(1, now.Add(-48*time.Hour)), merged(2, now.Add(-24*time.Hour))}, nil, now)

	_, stopped, _ := c.backfillAttributedPRRework(context.Background(), h, "r", 5, map[string]PRReworkStats{})
	if stopped {
		t.Fatal("a 503 ended the whole pass; it should only move on to the next repo")
	}
	if got := h.file.Records["r#2"].ReworkTries; got != 1 {
		t.Fatalf("#2 (404) tries = %d, want 1", got)
	}
	if got := h.file.Records["r#1"].ReworkTries; got != 0 {
		t.Fatalf("#1 (503) tries = %d, want 0", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, err := range map[string]error{
		"shed":      fmt.Errorf("listing reviews: %w", ErrPacingShed),
		"cancelled": context.Canceled,
		"429":       &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusTooManyRequests}},
	} {
		if got := classifyAttributedPRBackfillErr(context.Background(), err); got != attributedPRBackfillErrStop {
			t.Errorf("%s: class = %v, want stop", name, got)
		}
	}
	notFound := &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}}
	if got := classifyAttributedPRBackfillErr(ctx, notFound); got != attributedPRBackfillErrStop {
		t.Errorf("error after the pass's context ended: class = %v, want stop", got)
	}
	if got := classifyAttributedPRBackfillErr(context.Background(), notFound); got != attributedPRBackfillErrItem {
		t.Errorf("404: class = %v, want per-PR", got)
	}
}

// A closed PR older than what retention keeps is not stored again, so the
// next write does not have to prune it (and rewrite the file) every pass.
func TestAttributedPRHistoryUpsertSkipsPrunedPRs(t *testing.T) {
	now := time.Now().UTC()
	h := loadAttributedPRHistory(filepath.Join(t.TempDir(), AttributedPRHistoryFile), historyTestLogger())
	old := PullRequest{Repo: "r", Number: 1, HiveAttributed: true, State: "closed", CreatedAt: now.Add(-attributedPRHistoryRetention - time.Hour), ClosedAt: now.Add(-24 * time.Hour)}
	h.upsert([]PullRequest{old}, nil, now)
	if len(h.file.Records) != 0 || h.dirty {
		t.Fatalf("records = %d dirty = %v, want the pre-retention PR skipped", len(h.file.Records), h.dirty)
	}
	h.file.PrunedThrough = now.Add(-10 * 24 * time.Hour)
	capped := PullRequest{Repo: "r", Number: 2, HiveAttributed: true, State: "closed", CreatedAt: now.Add(-11 * 24 * time.Hour), ClosedAt: now.Add(-24 * time.Hour)}
	h.upsert([]PullRequest{capped}, nil, now)
	if len(h.file.Records) != 0 {
		t.Fatalf("records = %d, want a PR created before pruned_through skipped", len(h.file.Records))
	}
}

// A listing error that is not about the budget leaves that repo's cursor for
// the next pass and lets the pass go on to the next repo.
func TestAttributedPRBackfillListingErrorMovesToNextRepo(t *testing.T) {
	useTempReworkCache(t)
	now := time.Now().UTC()
	resetRESTAccountingForTest(t, now)
	var mu sync.Mutex
	var listed []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sort") == "created" {
			mu.Lock()
			listed = append(listed, r.URL.Path)
			mu.Unlock()
		}
		switch r.URL.Path {
		case "/repos/o/a/pulls":
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		case "/repos/o/b/pulls":
			writeJSON(t, w, []map[string]any{backfillMergedPR(9, now.Add(-20*24*time.Hour))})
		default:
			writeJSON(t, w, []map[string]any{})
		}
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "o", []string{"a", "b"}, historyTestLogger())
	h := useHistoryAt(c, filepath.Join(t.TempDir(), AttributedPRHistoryFile))
	h.noteScan("a", now, now.Add(-14*24*time.Hour), false)
	h.noteScan("b", now, now.Add(-14*24*time.Hour), false)

	c.syncAttributedPRHistory(context.Background(), []string{"a", "b"})
	mu.Lock()
	got := strings.Join(listed, ",")
	listed = nil
	mu.Unlock()
	if got != "/repos/o/a/pulls,/repos/o/b/pulls" {
		t.Fatalf("listed %q, want repo a tried and repo b still listed", got)
	}
	if bf := h.file.Repos["a"].Backfill; bf.Listed || bf.NextPage != 1 {
		t.Fatalf("repo a backfill = %+v, want its cursor kept for a retry", bf)
	}
	if bf := h.file.Repos["b"].Backfill; !bf.Listed || !bf.Done {
		t.Fatalf("repo b backfill = %+v, want listed and done", bf)
	}
	c.syncAttributedPRHistory(context.Background(), []string{"a", "b"})
	mu.Lock()
	got = strings.Join(listed, ",")
	mu.Unlock()
	if got != "/repos/o/a/pulls" {
		t.Fatalf("second pass listed %q, want only repo a retried", got)
	}
}

// A repo that never scanned has nothing live or stored, so it must not make
// every window partial; with no scanned repo at all nothing is covered.
func TestAttributedPRHistoryCoverageIgnoresNeverScannedRepos(t *testing.T) {
	now := time.Now().UTC()
	day := 24 * time.Hour
	h := loadAttributedPRHistory(filepath.Join(t.TempDir(), AttributedPRHistoryFile), historyTestLogger())
	h.noteScan("r", now.Add(-time.Hour), now.Add(-15*day), false)
	h.file.Repos["r"].Backfill = attributedPRBackfillState{StartedAt: now, Horizon: now.Add(-30 * day), Listed: true, Done: true}
	if _, cov := h.snapshot([]string{"r", "gone"}, now); cov.Full || !cov.Since.Equal(now.Add(-30*day)) {
		t.Fatalf("coverage = %+v, want the scanned repo's horizon", cov)
	}
	if _, cov := h.snapshot([]string{"gone"}, now); !cov.Available || cov.Full || !cov.Since.Equal(now) {
		t.Fatalf("coverage with no scanned repo = %+v, want nothing covered", cov)
	}
}

// Null entries in a hand-edited or truncated file are dropped on load
// instead of panicking inside enumeration.
func TestAttributedPRHistoryLoadDropsNullEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), AttributedPRHistoryFile)
	raw := `{"schema_version":1,"repos":{"r":null},"records":{"r#1":null,"r#2":{"repo":"r","number":2,"state":"closed","closed_at":"2026-11-01T00:00:00Z"}}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	h := loadAttributedPRHistory(path, historyTestLogger())
	if len(h.file.Repos) != 0 || len(h.file.Records) != 1 {
		t.Fatalf("loaded repos=%d records=%d, want the null entries dropped", len(h.file.Repos), len(h.file.Records))
	}
	now := time.Now()
	h.snapshot([]string{"r"}, now)
	h.reworkPending("r")
	h.backfillFinish("r", now)
	h.dirty = true
	if err := h.flush(now, []string{"r"}); err != nil {
		t.Fatal(err)
	}
}
