package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetricsEnabledToggle(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on"} {
		t.Setenv("HIVE_METRICS_ENABLED", v)
		if !metricsEnabled() {
			t.Errorf("HIVE_METRICS_ENABLED=%q should enable metrics", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "off", "garbage"} {
		t.Setenv("HIVE_METRICS_ENABLED", v)
		if metricsEnabled() {
			t.Errorf("HIVE_METRICS_ENABLED=%q should NOT enable metrics", v)
		}
	}
}

func TestHandleMetricsExposition(t *testing.T) {
	s := covApiServer(t)
	s.deps.Config.HiveID = "test-hive"
	// The route is only registered when metrics are enabled, so exercise the
	// handler directly — the exposition format is what's under test.
	rec := httptest.NewRecorder()
	s.handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE hive_estimated_cost_usd_total counter",
		"hive_estimated_cost_usd_total{hive_id=\"test-hive\"}",
		"# HELP hive_model_input_tokens_total",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q\n---\n%s", want, body)
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain exposition", ct)
	}
}

// TestHandleMetricsBearerToken covers the optional HIVE_METRICS_TOKEN gate
// (#3399): open when unset (backward-compatible), and requires a matching
// Bearer token when set.
func TestHandleMetricsBearerToken(t *testing.T) {
	newRec := func(auth string) *httptest.ResponseRecorder {
		s := covApiServer(t)
		s.deps.Config.HiveID = "test-hive"
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		s.handleMetrics(rec, req)
		return rec
	}

	t.Run("no token configured stays open", func(t *testing.T) {
		t.Setenv("HIVE_METRICS_TOKEN", "")
		if rec := newRec(""); rec.Code != http.StatusOK {
			t.Errorf("unauthenticated /metrics with no token = %d, want 200", rec.Code)
		}
	})

	t.Run("token configured rejects missing bearer", func(t *testing.T) {
		t.Setenv("HIVE_METRICS_TOKEN", "sk-metrics")
		rec := newRec("")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("missing bearer = %d, want 401", rec.Code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
			t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", got)
		}
	})

	t.Run("token configured rejects wrong bearer", func(t *testing.T) {
		t.Setenv("HIVE_METRICS_TOKEN", "sk-metrics")
		if rec := newRec("Bearer wrong"); rec.Code != http.StatusUnauthorized {
			t.Errorf("wrong bearer = %d, want 401", rec.Code)
		}
	})

	t.Run("token configured accepts correct bearer", func(t *testing.T) {
		t.Setenv("HIVE_METRICS_TOKEN", "sk-metrics")
		rec := newRec("Bearer sk-metrics")
		if rec.Code != http.StatusOK {
			t.Errorf("correct bearer = %d, want 200", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "hive_estimated_cost_usd_total") {
			t.Error("authenticated /metrics should return the exposition")
		}
	})
}
