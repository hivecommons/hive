package rotation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/claude/claudetest"
)

// On the operator's long-lived token the usage read degrades to unknown:
// nothing is sent to /api/oauth/usage, the reading fails open, and the error
// carries none of the markers the watchdog's auth probe reads as a logout
// (#17759).
func TestClaudeProber_LongLivedTokenIsUnknown(t *testing.T) {
	claudetest.SetOperatorToken(t, "sk-ant-oat01-PROBE-SENTINEL-9c1d")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("usage endpoint called in long-lived-token mode: %s", r.URL.Path)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	h := ClaudeProber{ThresholdPct: 80, BaseURL: srv.URL, CredentialsPath: claudeCredsFile(t, "expired-dashboard-token")}.Probe(context.Background())
	if h.ProbeErr == nil || !h.Available {
		t.Fatalf("headroom = %+v; want a fail-open unknown reading", h)
	}
	lower := strings.ToLower(h.ProbeErr.Error())
	for _, marker := range []string{"login", "unauthorized", "401", "invalid api key", "authentication", "sk-ant"} {
		if strings.Contains(lower, marker) {
			t.Fatalf("probe error %q contains %q", h.ProbeErr, marker)
		}
	}
}
