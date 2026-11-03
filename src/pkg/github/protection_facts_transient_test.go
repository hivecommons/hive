package github

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchReviewDecisionsFailureLogging(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		level  string
	}{
		{"gateway outage", http.StatusBadGateway, "DEBUG"},
		{"service unavailable", http.StatusServiceUnavailable, "DEBUG"},
		{"permission denied", http.StatusForbidden, "WARN"},
		{"bad credentials", http.StatusUnauthorized, "WARN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"message":"request failed"}`))
			}))
			defer srv.Close()
			c := newTestClient(t, srv, "org", []string{"repo"})
			var logs bytes.Buffer
			c.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			if got := c.fetchReviewDecisions(t.Context(), "repo"); got != nil {
				t.Fatalf("failed fetch must leave reviews unknown, got %v", got)
			}
			if calls != 1 {
				t.Fatalf("transport failure must not trigger schema fallback: %d requests", calls)
			}
			if !strings.Contains(logs.String(), "level="+tc.level) || !strings.Contains(logs.String(), "failed to fetch PR review decisions") {
				t.Fatalf("expected %s review-fetch diagnostic, got %s", tc.level, logs.String())
			}
		})
	}
}
