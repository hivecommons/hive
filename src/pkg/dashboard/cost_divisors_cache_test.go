package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func TestCostDivisorsRetainLastGoodCounts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"rate limited", http.StatusForbidden, `{"message":"API rate limit exceeded"}`},
		{"too many requests", http.StatusTooManyRequests, `{"message":"rate limited"}`},
		{"unavailable", http.StatusServiceUnavailable, `{"message":"unavailable"}`},
		{"incomplete zero", http.StatusOK, `{"total_count":0,"incomplete_results":true,"items":[]}`},
		{"incomplete nonzero", http.StatusOK, `{"total_count":2,"incomplete_results":true,"items":[]}`},
		{"missing total", http.StatusOK, `{"items":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var failing atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Fail the second query, so even a successful merged-PR query
				// cannot cause a partial refresh to replace the cached pair.
				if failing.Load() && strings.Contains(r.URL.Query().Get("q"), "type:issue") {
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
					return
				}
				total := 937
				if strings.Contains(r.URL.Query().Get("q"), "type:issue") {
					total = 1182
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"total_count": total, "items": []any{}})
			}))
			defer srv.Close()

			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			mc := &MetricsCollector{
				ghClient: ghpkg.NewClientForTest(srv.URL, "hivecommons", []string{"hive"}, logger),
				repo:     "hive", aiAuthor: "hive-bot[bot]", logger: logger,
				prIssueCachePath: filepath.Join(t.TempDir(), "counts.json"),
			}
			mc.collectPRIssueCounts(context.Background())
			want := mc.GetPRIssueCounts()
			if want == nil || want.MergedPRs != 937 || want.ClosedIssues != 1182 {
				t.Fatalf("initial counts = %+v", want)
			}
			before, err := os.ReadFile(mc.prIssueCountsPath())
			if err != nil {
				t.Fatal(err)
			}

			failing.Store(true)
			mc.collectPRIssueCounts(context.Background())
			if got := mc.GetPRIssueCounts(); got == nil || got.MergedPRs != want.MergedPRs || got.ClosedIssues != want.ClosedIssues || got.Author != want.Author || got.Basis != want.Basis || !got.Stale {
				t.Fatalf("failed refresh did not retain stale last-good counts: got %+v, want counts %+v", got, want)
			}
			after, err := os.ReadFile(mc.prIssueCountsPath())
			if err != nil || string(after) != string(before) {
				t.Fatalf("failed refresh changed disk cache: %s (error %v)", after, err)
			}

			// A restart during the outage must still serve persisted divisors.
			restarted := &MetricsCollector{logger: logger, prIssueCachePath: mc.prIssueCountsPath()}
			restarted.loadPRIssueCountsFromDisk()
			s, deps := covServer(t)
			deps.MetricsCollector = restarted
			rec := doGet(s, "/api/cost")
			var resp costResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.MergedPRs != want.MergedPRs || resp.ClosedIssues != want.ClosedIssues || resp.CountAuthor != want.Author || resp.CountBasis != want.Basis {
				t.Fatalf("cost endpoint lost persisted divisors: %+v", resp)
			}
		})
	}
}
