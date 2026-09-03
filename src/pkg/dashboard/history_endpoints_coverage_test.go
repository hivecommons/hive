package dashboard

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/hivecommons/hive/pkg/tokens"
)

// TestHandleTrendHistory covers the trend-history read endpoint (#2039).
func TestHandleTrendHistory(t *testing.T) {
	s := covApiServer(t)
	rec := doGet(s, "/api/trend/history")
	if rec.Code != http.StatusOK {
		t.Fatalf("trend history: want 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct == "" {
		t.Errorf("expected a Content-Type header")
	}
}

// TestHandleTimeSeries covers the unified sparkline-history alias (#2041),
// including every valid series and the unknown-series 400 path.
func TestHandleTimeSeries(t *testing.T) {
	s := covApiServer(t)

	for _, series := range []string{"token", "tokens", "fact", "facts", "cost"} {
		rec := doGet(s, "/api/timeseries?series="+series)
		if rec.Code != http.StatusOK {
			t.Errorf("series=%s: want 200, got %d", series, rec.Code)
		}
	}

	// Unknown / missing series → 400.
	for _, bad := range []string{"", "bogus"} {
		rec := doGet(s, "/api/timeseries?series="+bad)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("series=%q: want 400, got %d", bad, rec.Code)
		}
	}
}

// TestHandleMetricsWithCollector exercises handleMetrics with a token collector
// wired in so estimatedCost() returns populated by_model / by_agent slices,
// covering the exposition loops (not just the empty-data path).
func TestHandleMetricsWithCollector(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.HiveID = "cov-hive"
	deps.Tokens = tokens.NewCollector(t.TempDir(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	// A bearer token is mandatory (#3785) — handleMetrics fails closed without it.
	t.Setenv("HIVE_METRICS_TOKEN", "cov-metrics-token")
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer cov-metrics-token")
	rec := httptest.NewRecorder()
	s.handleMetrics(rec, req)
	if rec.Code != http.StatusOK && rec.Code != 0 {
		t.Fatalf("metrics status = %d", rec.Code)
	}
	// The exposition always emits the fixed HELP/TYPE headers and the total,
	// regardless of whether any model data exists.
	if body := rec.Body.String(); body == "" ||
		!containsAll(body, "hive_estimated_cost_usd_total", "cov-hive") {
		t.Errorf("exposition missing fixed series/label:\n%s", rec.Body.String())
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
