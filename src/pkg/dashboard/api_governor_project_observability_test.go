package dashboard

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestGovernorProjectObservabilityRoundTripAndOptIn(t *testing.T) {
	s := govServer(t)
	s.deps.Config.Governor.Modes = map[string]config.ModeConfig{
		"idle":  {Cadences: map[string]config.Cadence{"telemetry": config.NewIntervalCadence("paused"), "operations": config.NewIntervalCadence("paused")}},
		"surge": {Cadences: map[string]config.Cadence{"telemetry": config.NewIntervalCadence("paused"), "operations": config.NewIntervalCadence("paused")}},
	}
	body := map[string]any{
		"open_source":        []string{"opentelemetry", "prometheus"},
		"kube_native":        []string{"servicemonitor"},
		"commercial":         []string{"honeycomb"},
		"references":         map[string]any{"honeycomb": map[string]string{"endpoint_env": "OTEL_EXPORTER_OTLP_ENDPOINT", "credential_secret": "observability/honeycomb-key"}},
		"telemetry_enabled":  true,
		"operations_enabled": false,
	}
	rec := doPut(s, "/api/config/governor/project-observability", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	for modeName, mode := range s.deps.Config.Governor.Modes {
		if got := mode.Cadences["telemetry"].Interval(); got != defaultOperabilityCadence {
			t.Errorf("%s telemetry cadence = %q, want %q", modeName, got, defaultOperabilityCadence)
		}
		if !mode.Cadences["operations"].IsPaused() {
			t.Errorf("%s operations cadence must remain paused", modeName)
		}
	}

	get := doOwnerGet(s, "/api/config/governor/project-observability")
	if get.Code != http.StatusOK {
		t.Fatalf("GET = %d", get.Code)
	}
	var response map[string]any
	if err := json.Unmarshal(get.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["telemetry_enabled"] != true || response["operations_enabled"] != false {
		t.Fatalf("enabled state = %#v", response)
	}
}

func TestGovernorProjectObservabilityRejectsLiteralsAndUnknownPlatforms(t *testing.T) {
	for _, body := range []map[string]any{
		{"commercial": []string{"unknown-vendor"}},
		{"references": map[string]any{"honeycomb": map[string]string{"endpoint_env": "https://api.honeycomb.io"}}},
		{"references": map[string]any{"honeycomb": map[string]string{"credential_secret": "sk-live-secret"}}},
		{"telemetry_enabled": true},
	} {
		s := govServer(t)
		if rec := doPut(s, "/api/config/governor/project-observability", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("body %#v: status = %d, want 400", body, rec.Code)
		}
	}
}

func TestGovernorProjectObservabilityRequiresOwner(t *testing.T) {
	s := govServer(t)
	if rec := doGet(s, "/api/config/governor/project-observability"); rec.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated GET = %d, want 403", rec.Code)
	}
}

func TestDetectProjectObservabilityPlatforms(t *testing.T) {
	got := detectProjectObservabilityPlatforms([]string{
		"ServiceMonitor resources send Prometheus metrics through OpenTelemetry.",
		"The production traces use Honeycomb.",
	})
	for family, want := range map[string][]string{
		"open_source": {"opentelemetry", "prometheus"},
		"kube_native": {"servicemonitor"},
		"commercial":  {"honeycomb"},
	} {
		if !slices.Equal(got[family], want) {
			t.Errorf("%s detections = %v, want %v", family, got[family], want)
		}
	}
}
