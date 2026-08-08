package dashboard

import (
	"net/http"
	"testing"
)

// TestCovGov_Features exercises PUT /api/config/governor/features: a bad body is
// rejected, each of the four features' fields is set on Config and persisted,
// the plan_from_label pointer is set to an explicit value, and an invalid
// tracing endpoint is rejected.
func TestCovGov_Features(t *testing.T) {
	s := covApiServer(t)

	// Malformed JSON → 400.
	if rec := doPutRaw(s, "/api/config/governor/features", "bad"); rec.Code != http.StatusBadRequest {
		t.Fatalf("features bad body: %d", rec.Code)
	}

	// A bad tracing endpoint (not http(s)) → 400.
	if rec := doPut(s, "/api/config/governor/features", map[string]any{
		"tracingEndpoint": "ftp://collector:4318",
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad tracing endpoint: expected 400, got %d", rec.Code)
	}

	// A sample ratio out of range → 400.
	if rec := doPut(s, "/api/config/governor/features", map[string]any{
		"tracingSampleRatio": 1.5,
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad sample ratio: expected 400, got %d", rec.Code)
	}
	if rec := doPut(s, "/api/config/governor/features", map[string]any{
		"otelEndpoint": "ftp://collector:4318",
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad otel endpoint: expected 400, got %d", rec.Code)
	}

	// Set every field in one valid PUT.
	planTrue := true
	if rec := doPut(s, "/api/config/governor/features", map[string]any{
		"ioscanEnabled":      true,
		"tracingEnabled":     true,
		"tracingEndpoint":    "https://otel-collector:4318",
		"tracingSampleRatio": 0.5,
		"otelServiceName":    "hive-ui",
		"otelInsecure":       true,
		"otelHeaders":        map[string]string{"authorization": "${OTEL_TOKEN}"},
		"retroEnabled":       true,
		"retroAnalysisModel": "claude-sonnet",
		"mintEnabled":        true,
		"mintIssuer":         "https://mint.example.com",
		"planFromLabel":      planTrue,
	}); rec.Code != http.StatusOK {
		t.Fatalf("features ok: %d", rec.Code)
	}

	cfg := s.deps.Config
	if !cfg.Ioscan.IsEnabled() {
		t.Errorf("ioscan not enabled")
	}
	if !cfg.Tracing.Enabled || !cfg.OTel.Enabled {
		t.Errorf("tracing/otel not enabled: tracing=%v otel=%v", cfg.Tracing.Enabled, cfg.OTel.Enabled)
	}
	if cfg.Tracing.Endpoint != "https://otel-collector:4318" || cfg.OTel.Endpoint != "https://otel-collector:4318" {
		t.Errorf("tracing endpoint = %q otel endpoint = %q", cfg.Tracing.Endpoint, cfg.OTel.Endpoint)
	}
	if cfg.Tracing.SampleRatio != 0.5 || cfg.OTel.SampleRatio != 0.5 {
		t.Errorf("tracing sample ratio = %v otel sample ratio = %v", cfg.Tracing.SampleRatio, cfg.OTel.SampleRatio)
	}
	if cfg.OTel.ServiceName != "hive-ui" || !cfg.OTel.Insecure || cfg.OTel.Headers["authorization"] != "${OTEL_TOKEN}" {
		t.Errorf("advanced otel fields not set: %+v", cfg.OTel)
	}
	if !cfg.Retro.Enabled || cfg.Retro.AnalysisModel != "claude-sonnet" {
		t.Errorf("retro fields not set: %+v", cfg.Retro)
	}
	if !cfg.Mint.Enabled {
		t.Errorf("mint not enabled")
	}
	if cfg.Mint.Issuer != "https://mint.example.com" {
		t.Errorf("mint issuer = %q", cfg.Mint.Issuer)
	}
	// KeyPath must never be touched by this handler.
	if cfg.Mint.KeyPath != "" {
		t.Errorf("mint key path unexpectedly set: %q", cfg.Mint.KeyPath)
	}
	if cfg.Planning.PlanFromLabel == nil || *cfg.Planning.PlanFromLabel != true {
		t.Errorf("plan_from_label pointer = %v (want explicit true)", cfg.Planning.PlanFromLabel)
	}

	// An explicit false must flip the pointer to false, not clear it.
	if rec := doPut(s, "/api/config/governor/features", map[string]any{
		"planFromLabel": false,
	}); rec.Code != http.StatusOK {
		t.Fatalf("plan false ok: %d", rec.Code)
	}
	if cfg.Planning.PlanFromLabel == nil || *cfg.Planning.PlanFromLabel != false {
		t.Errorf("plan_from_label pointer after false = %v (want explicit false)", cfg.Planning.PlanFromLabel)
	}

	// An absent field leaves prior values unchanged (only ioscan sent here).
	if rec := doPut(s, "/api/config/governor/features", map[string]any{
		"ioscanEnabled": false,
	}); rec.Code != http.StatusOK {
		t.Fatalf("partial update ok: %d", rec.Code)
	}
	if cfg.Ioscan.IsEnabled() {
		t.Errorf("ioscan should be disabled after partial update")
	}
	// Tracing endpoint set earlier must survive the partial update.
	if cfg.Tracing.Endpoint != "https://otel-collector:4318" {
		t.Errorf("tracing endpoint lost on partial update: %q", cfg.Tracing.Endpoint)
	}
}

func TestCovGov_FeaturesEndpointOnlyPreservesLegacyEnabled(t *testing.T) {
	s := covApiServer(t)
	s.deps.Config.Tracing.Enabled = true

	if rec := doPut(s, "/api/config/governor/features", map[string]any{
		"tracingEndpoint": "https://otel-new:4318",
	}); rec.Code != http.StatusOK {
		t.Fatalf("endpoint-only update: %d", rec.Code)
	}

	if got := s.deps.Config.EffectiveOTel(); !got.Enabled || got.Endpoint != "https://otel-new:4318" {
		t.Fatalf("effective otel after endpoint-only update = %+v", got)
	}
}
