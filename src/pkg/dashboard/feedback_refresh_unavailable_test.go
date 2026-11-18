package dashboard

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// hivecommons/hive#19008: a hub restart answers 503 "hive unavailable" to the
// feedback issue refresh. The backoff already retries, so the failure is
// logged below WARN and never feeds the feedback_issue_refresh_failed
// self-report; any other failure still logs at WARN.
func TestFeedbackMineRefreshUpstreamUnavailableLogsBelowWarn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		wantWarn bool
	}{
		{name: "503 hive unavailable", status: http.StatusServiceUnavailable},
		{name: "502 bad gateway", status: http.StatusBadGateway},
		{name: "504 gateway timeout", status: http.StatusGatewayTimeout},
		{name: "500 stays a warning", status: http.StatusInternalServerError, wantWarn: true},
		{name: "404 stays a warning", status: http.StatusNotFound, wantWarn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldStore := feedbackSubmissionsPath
			oldBase := feedbackGitHubAPIBase
			t.Cleanup(func() {
				feedbackSubmissionsPath = oldStore
				feedbackGitHubAPIBase = oldBase
			})
			feedbackSubmissionsPath = filepath.Join(t.TempDir(), "feedback-submissions.json")
			if err := saveFeedbackSubmissions([]feedbackSubmissionRecord{{
				Owner: "hivecommons", Repo: "hive", Number: 42, State: "open",
				HTMLURL: "https://github.com/hivecommons/hive/issues/42",
			}}); err != nil {
				t.Fatal(err)
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"ok":false,"error":"hive unavailable"}`))
			}))
			defer upstream.Close()
			feedbackGitHubAPIBase = func() string { return upstream.URL }

			var logs bytes.Buffer
			s := NewServer(0, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			s.deps = &Dependencies{Config: &config.Config{}}
			req := httptest.NewRequest(http.MethodGet, "/api/feedback/mine", nil)
			markOwnerRequest(req)
			rec := httptest.NewRecorder()
			s.handleFeedbackMine(rec, req)

			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Could not refresh issue activity") {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			if !s.feedbackRefreshBackoff.blocked(time.Now()) {
				t.Fatal("refresh failure must still arm the backoff")
			}
			out := logs.String()
			gotWarn := strings.Contains(out, "level=WARN") && strings.Contains(out, "feedback: issue refresh failed")
			if gotWarn != tc.wantWarn {
				t.Fatalf("WARN %q logged = %v, want %v:\n%s", "feedback: issue refresh failed", gotWarn, tc.wantWarn, out)
			}
			if !tc.wantWarn && !strings.Contains(out, "level=INFO") {
				t.Fatalf("transient failure not logged at INFO:\n%s", out)
			}
		})
	}
}
