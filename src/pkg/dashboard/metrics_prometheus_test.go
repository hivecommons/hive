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
	// handler directly — the exposition format is what's under test. A token is
	// mandatory (#3785), so authenticate.
	t.Setenv("HIVE_METRICS_TOKEN", "sk-metrics")
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer sk-metrics")
	rec := httptest.NewRecorder()
	s.handleMetrics(rec, req)
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

// TestHandleMetricsBearerToken covers the mandatory HIVE_METRICS_TOKEN gate
// (#3399, hardened in #3785): fails closed (403) when no token is configured,
// and requires a matching Bearer token when set.
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

	t.Run("no token configured fails closed", func(t *testing.T) {
		// SECURITY (#3785): enabling metrics without HIVE_METRICS_TOKEN must
		// NOT expose the cost/agent series — the handler refuses to serve.
		t.Setenv("HIVE_METRICS_TOKEN", "")
		rec := newRec("")
		if rec.Code != http.StatusForbidden {
			t.Errorf("unauthenticated /metrics with no token = %d, want 403", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "hive_estimated_cost_usd") {
			t.Error("fail-closed response must not include the cost exposition")
		}
		if !strings.Contains(rec.Body.String(), "HIVE_METRICS_TOKEN") {
			t.Error("fail-closed response should name HIVE_METRICS_TOKEN so the operator knows the fix")
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

// TestMetricsRouteRegistration exercises /metrics end-to-end through the mux:
// the route only exists when HIVE_METRICS_ENABLED is set at server
// construction (404 otherwise), and the registered route enforces the
// mandatory HIVE_METRICS_TOKEN gate (#3785).
func TestMetricsRouteRegistration(t *testing.T) {
	serve := func(t *testing.T, auth string) *httptest.ResponseRecorder {
		t.Helper()
		s := covApiServer(t) // registerCoreRoutes runs here, reading the env
		s.deps.Config.HiveID = "test-hive"
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		return rec
	}

	t.Run("disabled means no route", func(t *testing.T) {
		t.Setenv("HIVE_METRICS_ENABLED", "")
		t.Setenv("HIVE_METRICS_TOKEN", "")
		if rec := serve(t, ""); rec.Code != http.StatusNotFound {
			t.Errorf("/metrics while disabled = %d, want 404", rec.Code)
		}
	})

	t.Run("enabled without token fails closed", func(t *testing.T) {
		t.Setenv("HIVE_METRICS_ENABLED", "1")
		t.Setenv("HIVE_METRICS_TOKEN", "")
		rec := serve(t, "")
		if rec.Code != http.StatusForbidden {
			t.Errorf("/metrics enabled w/o token = %d, want 403", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "hive_estimated_cost_usd") {
			t.Error("fail-closed route must not leak the cost exposition")
		}
	})

	t.Run("enabled with token serves authenticated scrape", func(t *testing.T) {
		t.Setenv("HIVE_METRICS_ENABLED", "1")
		t.Setenv("HIVE_METRICS_TOKEN", "sk-route")
		rec := serve(t, "Bearer sk-route")
		if rec.Code != http.StatusOK {
			t.Errorf("authenticated scrape = %d, want 200", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "hive_estimated_cost_usd_total") {
			t.Error("authenticated scrape should return the exposition")
		}
	})
}
