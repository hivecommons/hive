package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// forgeCountServer answers the Search-API count queries, counting how many
// it received, and fails every query while failing is set.
func forgeCountServer(t *testing.T, calls *atomic.Int32, failing *atomic.Bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/search/issues") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
			return
		}
		calls.Add(1)
		if failing.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"message":"unavailable"}`)
			return
		}
		q := r.URL.Query().Get("q")
		total := 7
		if strings.Contains(q, "type:issue") {
			total = 11
		}
		if strings.Contains(q, "is:open") {
			total = 3
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": total, "items": []any{}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

type warnCounter struct {
	slog.Handler
	warns *atomic.Int32
}

func (h warnCounter) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		h.warns.Add(1)
	}
	return h.Handler.Handle(ctx, r)
}

func newForgeCountCollector(t *testing.T, srv *httptest.Server, now *time.Time, warns *atomic.Int32) *MetricsCollector {
	t.Helper()
	logger := slog.New(warnCounter{Handler: slog.NewTextHandler(io.Discard, nil), warns: warns})
	return &MetricsCollector{
		ghClient:          ghpkg.NewClientForTest(srv.URL, "hivecommons", []string{"hive"}, logger),
		org:               "hivecommons",
		repo:              "hive",
		aiAuthor:          "hive-bot[bot]",
		projectName:       "Hive",
		logger:            logger,
		metrics:           map[string]any{},
		prIssueCachePath:  filepath.Join(t.TempDir(), "counts.json"),
		now:               func() time.Time { return *now },
		rateLimitCautious: func() bool { return false },
	}
}

// The per-repo Search counts are re-asked at most once per TTL; collects
// inside the window serve the cache and spend no Search calls.
func TestCollectPRIssueCounts_CachedWithinTTL(t *testing.T) {
	var calls atomic.Int32
	var failing atomic.Bool
	var warns atomic.Int32
	srv := forgeCountServer(t, &calls, &failing)
	now := time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)
	mc := newForgeCountCollector(t, srv, &now, &warns)

	mc.collectPRIssueCounts(context.Background())
	first := calls.Load()
	if first == 0 {
		t.Fatal("first collect issued no Search calls")
	}
	got := mc.GetPRIssueCounts()
	if got == nil || got.MergedPRs != 7 || got.ClosedIssues != 11 || got.Stale {
		t.Fatalf("first counts = %+v", got)
	}

	for i := 0; i < 3; i++ {
		now = now.Add(metricsCollectInterval)
		mc.collectPRIssueCounts(context.Background())
	}
	if calls.Load() != first {
		t.Fatalf("collects inside the TTL issued %d extra Search calls", calls.Load()-first)
	}

	now = now.Add(forgeCountCacheTTLDefault)
	mc.collectPRIssueCounts(context.Background())
	if calls.Load() == first {
		t.Fatal("collect past the TTL did not refresh")
	}
}

// A failed refresh serves the last-good counts (marked stale), backs off
// before retrying, and WARNs once per log interval instead of per collect.
func TestCollectPRIssueCounts_FailureServesLastGoodAndRateLimitsLog(t *testing.T) {
	var calls atomic.Int32
	var failing atomic.Bool
	var warns atomic.Int32
	srv := forgeCountServer(t, &calls, &failing)
	now := time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)
	mc := newForgeCountCollector(t, srv, &now, &warns)
	mc.collectPRIssueCounts(context.Background())
	warns.Store(0)

	failing.Store(true)
	now = now.Add(forgeCountCacheTTLDefault + time.Minute)
	failedAt := now
	mc.collectPRIssueCounts(context.Background())
	afterFail := calls.Load()
	got := mc.GetPRIssueCounts()
	if got == nil || got.MergedPRs != 7 || got.ClosedIssues != 11 || !got.Stale {
		t.Fatalf("failed refresh did not keep last-good stale counts: %+v", got)
	}
	if warns.Load() != 1 {
		t.Fatalf("first failure logged %d warns, want 1", warns.Load())
	}

	// Inside the retry backoff: no Search call, no new WARN.
	now = now.Add(metricsCollectInterval)
	mc.collectPRIssueCounts(context.Background())
	if calls.Load() != afterFail {
		t.Fatalf("collect inside the retry backoff issued %d Search calls", calls.Load()-afterFail)
	}
	if warns.Load() != 1 {
		t.Fatalf("backoff collect logged %d warns, want still 1", warns.Load())
	}

	// Past the backoff but inside the log interval: retries, still no WARN.
	now = failedAt.Add(forgeCountRetryBackoff)
	mc.collectPRIssueCounts(context.Background())
	if calls.Load() == afterFail {
		t.Fatal("collect past the retry backoff did not retry")
	}
	if warns.Load() != 1 {
		t.Fatalf("repeat failure inside the log interval logged %d warns, want 1", warns.Load())
	}

	// Next due retry, now past the log interval: the WARN repeats once.
	now = now.Add(forgeCountRetryBackoff)
	if now.Sub(failedAt) < forgeCountErrorLogInterval {
		t.Fatalf("test clock %v after failure is inside the %v log interval", now.Sub(failedAt), forgeCountErrorLogInterval)
	}
	mc.collectPRIssueCounts(context.Background())
	if warns.Load() != 2 {
		t.Fatalf("failure past the log interval logged %d warns, want 2", warns.Load())
	}

	// Recovery replaces the stale counts with fresh ones.
	failing.Store(false)
	now = now.Add(forgeCountRetryBackoff)
	mc.collectPRIssueCounts(context.Background())
	if got := mc.GetPRIssueCounts(); got == nil || got.Stale || got.MergedPRs != 7 {
		t.Fatalf("recovered counts = %+v", got)
	}
}

// While the slow-start caution window is engaged a due refresh is deferred
// as long as something is cached; with nothing cached it still runs.
func TestCollectPRIssueCounts_DefersWhileRateLimitCautious(t *testing.T) {
	var calls atomic.Int32
	var failing atomic.Bool
	var warns atomic.Int32
	srv := forgeCountServer(t, &calls, &failing)
	now := time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)
	mc := newForgeCountCollector(t, srv, &now, &warns)
	cautious := true
	mc.rateLimitCautious = func() bool { return cautious }

	mc.collectPRIssueCounts(context.Background())
	if calls.Load() == 0 || mc.GetPRIssueCounts() == nil {
		t.Fatal("with nothing cached the first collect must still fetch")
	}
	first := calls.Load()
	now = now.Add(forgeCountCacheTTLDefault + time.Minute)
	mc.collectPRIssueCounts(context.Background())
	if calls.Load() != first {
		t.Fatal("due refresh ran during the caution window with cached counts available")
	}
	cautious = false
	mc.collectPRIssueCounts(context.Background())
	if calls.Load() == first {
		t.Fatal("refresh did not run once the caution window ended")
	}
}

// Outreach PR counts get the same TTL cache and last-good fallback.
func TestCountOutreachPRs_CachedAndLastGood(t *testing.T) {
	var calls atomic.Int32
	var failing atomic.Bool
	var warns atomic.Int32
	srv := forgeCountServer(t, &calls, &failing)
	now := time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)
	mc := newForgeCountCollector(t, srv, &now, &warns)

	open, merged := mc.countOutreachPRs(context.Background())
	if open != 3 || merged != 7 {
		t.Fatalf("outreach counts = %d/%d, want 3/7", open, merged)
	}
	if calls.Load() != 2 {
		t.Fatalf("outreach refresh issued %d Search calls, want 2", calls.Load())
	}

	now = now.Add(metricsCollectInterval)
	if o, m := mc.countOutreachPRs(context.Background()); o != 3 || m != 7 || calls.Load() != 2 {
		t.Fatalf("cached outreach counts = %d/%d after %d calls", o, m, calls.Load())
	}

	failing.Store(true)
	now = now.Add(forgeCountCacheTTLDefault)
	if o, m := mc.countOutreachPRs(context.Background()); o != 3 || m != 7 {
		t.Fatalf("failed outreach refresh did not serve last-good: %d/%d", o, m)
	}
	if warns.Load() != 1 {
		t.Fatalf("failed outreach refresh logged %d warns, want 1", warns.Load())
	}
	// Open failed, so merged is not even asked: one call, not two.
	if calls.Load() != 3 {
		t.Fatalf("failed refresh issued %d Search calls total, want 3", calls.Load())
	}
	now = now.Add(metricsCollectInterval)
	mc.countOutreachPRs(context.Background())
	if calls.Load() != 3 || warns.Load() != 1 {
		t.Fatalf("collect inside backoff: calls %d warns %d", calls.Load(), warns.Load())
	}
}

func TestForgeCountCacheTTL_Env(t *testing.T) {
	t.Setenv(forgeCountCacheTTLEnv, "")
	if got := forgeCountCacheTTL(); got != forgeCountCacheTTLDefault {
		t.Fatalf("default TTL = %v, want %v", got, forgeCountCacheTTLDefault)
	}
	t.Setenv(forgeCountCacheTTLEnv, "45m")
	if got := forgeCountCacheTTL(); got != 45*time.Minute {
		t.Fatalf("env TTL = %v, want 45m", got)
	}
	t.Setenv(forgeCountCacheTTLEnv, "nonsense")
	if got := forgeCountCacheTTL(); got != forgeCountCacheTTLDefault {
		t.Fatalf("unparseable TTL = %v, want default", got)
	}
}
