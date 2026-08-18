package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// TestContributeMetricsEndpointRegistered pins that GET /api/contribute/metrics
// is wired and returns the four series with the "hour" bucket contract the client
// sparklines depend on. An empty store must still return well-formed (empty)
// arrays, never 404 or a nil-map panic.
func TestContributeMetricsEndpointRegistered(t *testing.T) {
	setupContributeEnv(t)
	t.Setenv("HIVE_METRICS_FILE", filepath.Join(t.TempDir(), "metrics.json"))

	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	rec := doGet(s, "/api/contribute/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/contribute/metrics = %d, want 200", rec.Code)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	for _, key := range []string{"queue_depth", "tasks_done", "fleet_size", "per_user_done", "bucket", "collected_at"} {
		if _, ok := body[key]; !ok {
			t.Errorf("metrics response missing key %q", key)
		}
	}
	var bucket string
	if err := json.Unmarshal(body["bucket"], &bucket); err != nil || bucket != "hour" {
		t.Errorf("bucket = %q (err %v), want \"hour\"", bucket, err)
	}
}

// TestContributeMetricsSparklineClientWired pins the client-side pieces on the
// rendered /contribute page: the dependency-free SVG sparkline renderer, the
// metrics fetch, and every sparkline home (queue header, throughput, fleet,
// quota, plus the leaderboard per-row + hive-wide trend). Mirrors the
// contribute_devlog_rail_test.go strings.Contains pin style so the feature
// cannot silently regress.
func TestContributeMetricsSparklineClientWired(t *testing.T) {
	body := renderContributePage(t)

	for _, want := range []string{
		"function sparkline(",              // the dependency-free SVG renderer
		"<svg width=",                      // renders an inline SVG (CSP-safe, no lib)
		"/api/contribute/metrics",          // the fetch target
		"function ccMetricsPoll(",          // the poll driver, called from opsPoll
		"ccMetricsPoll();",                 // actually invoked
		`id="spark-queue"`,                 // (a) ready-work queue header trend
		`id="spark-throughput"`,            // (b) tasks/hour throughput
		`id="spark-fleet"`,                 // (c) connected-clanker fleet trend
		`id="spark-quota"`,                 // (d) your daily-quota usage trend
		`id="spark-lb-trend"`,              // (f) hive-wide total-tasks trend
		`class="lb-spark" data-user=`,      // (e) leaderboard per-row sparkline slot
		"function ccRenderLeaderboardSparklines(", // leaderboard painter
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered contribute page missing sparkline marker %q", want)
		}
	}
}
