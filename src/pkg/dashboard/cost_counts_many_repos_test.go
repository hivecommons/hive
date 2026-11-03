package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/forgestate"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// rateLimitedSearch is a fake GitHub Search API with the real limit's
// shape: `limit` calls per minute of fake-clock time, then 403s until the
// minute turns over.
type rateLimitedSearch struct {
	mu       sync.Mutex
	now      func() time.Time
	limit    int
	window   time.Time
	inWindow int
	calls    int
	refused  int
}

func (f *rateLimitedSearch) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	if now.Sub(f.window) >= time.Minute {
		f.window, f.inWindow = now, 0
	}
	f.calls++
	f.inWindow++
	if f.inWindow > f.limit {
		f.refused++
		// No X-RateLimit-* headers: a primary-limit refusal with a reset
		// would engage the process-wide pacing ledger for every later test
		// in the binary (see ghpkg.ResetRateLimitPacingForTest).
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"API rate limit exceeded for installation ID 1."}`)
		return
	}
	total := 16
	if strings.Contains(r.URL.Query().Get("q"), "type:issue") {
		total = 47
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"total_count": total, "items": []any{}})
}

func bluefinRepos() []string {
	repos := make([]string, 0, 19)
	for i := 0; i < 19; i++ {
		repos = append(repos, fmt.Sprintf("projectbluefin/repository-%02d", i))
	}
	return repos
}

// projectbluefin's hive (19 repos) showed "—" for 19 days: every hourly
// reconcile asked GitHub about all 19 repos in one burst (38–76 search
// calls against a 30/minute limit), the refused repos never got a baseline,
// and the aggregate was never ready — so /api/cost fell back to the
// merged-by-hive counter, legitimately 0 at ACMM L5. The reconcile is now
// budgeted across paced passes and partial progress is served, marked.
func TestCostCountsManyReposUnderSearchRateLimit(t *testing.T) {
	clock := time.Date(2026, 10, 3, 1, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	search := &rateLimitedSearch{now: now, limit: 30}
	srv := httptest.NewServer(search)
	defer srv.Close()
	t.Cleanup(ghpkg.ResetRateLimitPacingForTest)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	repos := bluefinRepos()
	mc := NewMetricsCollector(ghpkg.NewClientForTest(srv.URL, "projectbluefin", repos, logger), "projectbluefin", repos[0], "", "hivecommons-hive[bot]", "P", logger)
	mc.prIssueCachePath = filepath.Join(t.TempDir(), "counts.json")
	mc.now = now
	mc.rateLimitCautious = func() bool { return false }
	mc.SetProjectScope("projectbluefin", repos[0], repos, "hivecommons-hive[bot]")
	fc := forgestate.NewCounts(forgestate.CountsOptions{
		Repos:     mc.countReposQualified(),
		Reconcile: mc.reconcileOutcomeCounts,
		Author:    "hivecommons-hive[bot]",
		Now:       now,
	})
	mc.prIssueMu.Lock()
	mc.forgeCounts = fc
	mc.prIssueMu.Unlock()
	ctx := context.Background()

	s, deps := covServer(t)
	deps.Config.Project.Org = "projectbluefin"
	deps.Config.Project.Repos = repos
	deps.Config.Project.AIAuthor = "hivecommons-hive[bot]"
	deps.MetricsCollector = mc
	// The legacy merged-by-hive counter is 0 at L5 — it must never be what
	// the tile falls back to once any attributed count exists.
	s.audit = &AuditLog{prCounters: PRThroughputCounters{Since: "2026-10-02T00:00:00Z"}}

	// First collect: one budgeted pass, well under the limit.
	mc.collectPRIssueCounts(ctx)
	if search.refused != 0 {
		t.Fatalf("first pass refused %d of %d calls; must stay under the search limit", search.refused, search.calls)
	}
	got := mc.GetPRIssueCounts()
	if got == nil || !got.Partial || got.MergedPRs != 16*forgestate.CountsReconcileBudget || got.ClosedIssues != 47*forgestate.CountsReconcileBudget {
		t.Fatalf("after one pass = %+v, want a partial count over the first %d repos", got, forgestate.CountsReconcileBudget)
	}
	if !strings.HasPrefix(got.Status, "partial: 5 of 19 repos") {
		t.Fatalf("Status = %q, want partial: 5 of 19 repos counted", got.Status)
	}
	rec := doGet(s, "/api/cost")
	var resp costResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.CountAvailable || resp.CountBasis != "hive-attributed" || !resp.CountPartial || resp.MergedPRs != got.MergedPRs {
		t.Fatalf("/api/cost after one pass = %+v, want the partial attributed count, not the persisted fallback", resp)
	}

	// Collects every 5 minutes (the collector's cadence) finish the round
	// without ever being refused.
	for i := 0; i < 6 && fc.ReconcilePending() > 0; i++ {
		clock = clock.Add(5 * time.Minute)
		mc.collectPRIssueCounts(ctx)
	}
	if search.refused != 0 {
		t.Fatalf("round refused %d of %d calls", search.refused, search.calls)
	}
	got = mc.GetPRIssueCounts()
	if got == nil || got.Partial || got.Stale || got.MergedPRs != 16*19 || got.ClosedIssues != 47*19 || got.Status != "fresh" {
		t.Fatalf("after the round = %+v, want fresh %d/%d over all 19 repos", got, 16*19, 47*19)
	}
	if search.calls != 38 {
		t.Fatalf("search calls for the round = %d, want 2 per repo (38) spread over paced passes", search.calls)
	}

	// An hour later GitHub refuses everything: the complete attributed
	// count is served stale with the reason — never "—", never the
	// merged-by-hive fallback.
	search.limit = 0
	clock = clock.Add(time.Hour)
	mc.collectPRIssueCounts(ctx)
	got = mc.GetPRIssueCounts()
	if got == nil || !got.Stale || got.MergedPRs != 16*19 || got.ClosedIssues != 47*19 {
		t.Fatalf("after the outage = %+v, want the last complete count served stale", got)
	}
	if !strings.Contains(strings.ToLower(got.Error), "rate limit") {
		t.Fatalf("Error = %q, want the rate-limit reason", got.Error)
	}
	rec = doGet(s, "/api/cost")
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.CountBasis != "hive-attributed" || !resp.CountStale || resp.MergedPRs != 16*19 || !strings.Contains(strings.ToLower(resp.CountError), "rate limit") {
		t.Fatalf("/api/cost during the outage = %+v, want stale attributed count with count_error", resp)
	}
}

// With nothing cached and GitHub refusing every query, the API still says
// why instead of a bare "counts unavailable (GitHub rate-limited)" guess.
func TestCostCountErrorSurfacedWhenNothingAttributedYet(t *testing.T) {
	clock := time.Date(2026, 10, 3, 1, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	search := &rateLimitedSearch{now: now, limit: 0}
	srv := httptest.NewServer(search)
	defer srv.Close()
	t.Cleanup(ghpkg.ResetRateLimitPacingForTest)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	repos := bluefinRepos()
	mc := NewMetricsCollector(ghpkg.NewClientForTest(srv.URL, "projectbluefin", repos, logger), "projectbluefin", repos[0], "", "hivecommons-hive[bot]", "P", logger)
	mc.prIssueCachePath = filepath.Join(t.TempDir(), "counts.json")
	mc.now = now
	mc.rateLimitCautious = func() bool { return false }
	mc.SetProjectScope("projectbluefin", repos[0], repos, "hivecommons-hive[bot]")
	fc := mc.EnableForgeCounts(nil)
	mc.collectPRIssueCounts(context.Background())
	if fc.Outcomes().Ready || mc.GetPRIssueCounts() != nil {
		t.Fatal("nothing should be ready when every query is refused")
	}
	if err := mc.GetPRIssueCountError(); !strings.Contains(strings.ToLower(err), "rate limit") {
		t.Fatalf("GetPRIssueCountError = %q, want the rate-limit reason", err)
	}

	s, deps := covServer(t)
	deps.MetricsCollector = mc
	rec := doGet(s, "/api/cost")
	var resp costResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.CountAvailable || !strings.Contains(strings.ToLower(resp.CountError), "rate limit") || !strings.HasPrefix(resp.CountStatus, "counts unavailable (") {
		t.Fatalf("/api/cost = %+v, want count_error with the reason", resp)
	}

	// The persisted merged-by-hive fallback, when it has data, still carries
	// the attributed refresh's error so the tooltip can explain the basis.
	s.audit = &AuditLog{prCounters: PRThroughputCounters{
		Since:   "2026-10-02T00:00:00Z",
		ByActor: PRThroughputActorMatrix{prThroughputKindIssue: {prThroughputRoleClosed: {prThroughputActorHive: 9}}},
	}}
	rec = doGet(s, "/api/cost")
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.CountAvailable || resp.CountBasis != "persisted-pr-throughput" || resp.ClosedIssues != 9 || !strings.Contains(strings.ToLower(resp.CountError), "rate limit") {
		t.Fatalf("/api/cost with fallback = %+v, want persisted basis plus count_error", resp)
	}
}
