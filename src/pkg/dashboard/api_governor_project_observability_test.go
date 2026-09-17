package dashboard

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestGovernorProjectObservabilityRoundTripAndReportsAgentStatus(t *testing.T) {
	s := govServer(t)
	s.deps.Config.Agents = map[string]config.AgentConfig{
		"telemetry":  {Enabled: true},
		"operations": {Enabled: false},
	}
	s.deps.Config.Governor.Modes = map[string]config.ModeConfig{
		"idle":  {Cadences: map[string]config.Cadence{"telemetry": config.NewIntervalCadence("6h"), "operations": config.NewIntervalCadence("paused")}},
		"surge": {Cadences: map[string]config.Cadence{"telemetry": config.NewIntervalCadence("15m"), "operations": config.NewIntervalCadence("paused")}},
	}
	body := map[string]any{
		"open_source": []string{"opentelemetry", "prometheus"},
		"kube_native": []string{"servicemonitor"},
		"commercial":  []string{"honeycomb"},
		"references":  map[string]any{"honeycomb": map[string]string{"endpoint_env": "OTEL_EXPORTER_OTLP_ENDPOINT", "credential_secret": "observability/honeycomb-key"}},
	}
	rec := doPut(s, "/api/config/governor/project-observability", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}

	// The per-mode tuning above must survive untouched. Before #7261 a PUT
	// here rewrote every mode's cadence for both agents.
	for modeName, want := range map[string]string{"idle": "6h", "surge": "15m"} {
		if got := s.deps.Config.Governor.Modes[modeName].Cadences["telemetry"].Interval(); got != want {
			t.Errorf("%s telemetry cadence = %q, want %q (this tab must not write cadences)", modeName, got, want)
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

	// The status block reports the agent card's Enabled flag, not the old
	// "some mode is unpaused" proxy. operations is unpaused nowhere and
	// disabled on its card; telemetry is enabled with real cadences.
	telemetry, _ := response["telemetry_status"].(map[string]any)
	if telemetry["configured"] != true || telemetry["enabled"] != true {
		t.Errorf("telemetry_status = %#v, want configured+enabled", telemetry)
	}
	if cad, _ := telemetry["cadences"].(map[string]any); cad["idle"] != "6h" || cad["surge"] != "15m" {
		t.Errorf("telemetry cadences = %#v, want the real per-mode values", cad)
	}
	operations, _ := response["operations_status"].(map[string]any)
	if operations["configured"] != true || operations["enabled"] != false {
		t.Errorf("operations_status = %#v, want configured and disabled", operations)
	}

	if _, ok := response["telemetry_enabled"]; ok {
		t.Error("telemetry_enabled must be gone from the payload (#7261): it was a second writer for a fact the agent card owns")
	}
	if _, ok := response["operations_enabled"]; ok {
		t.Error("operations_enabled must be gone from the payload (#7261)")
	}
}

// TestProjectObservabilityPutNeverTouchesCadences is the guard #7261 asks for:
// no payload this endpoint accepts may mutate governor cadences.
//
// It is written as a quantifier over payloads -- including the two legacy
// enable keys, which a cached older dashboard tab will keep sending -- rather
// than a check on one request, because the bug was precisely that an
// innocuous-looking save silently rewrote per-mode tuning that another tab
// owns.
func TestProjectObservabilityPutNeverTouchesCadences(t *testing.T) {
	payloads := []map[string]any{
		{"telemetry_enabled": true},
		{"telemetry_enabled": false},
		{"operations_enabled": true},
		{"operations_enabled": false},
		{"telemetry_enabled": true, "operations_enabled": true},
		{"open_source": []string{"prometheus"}},
		{"open_source": []string{"prometheus"}, "telemetry_enabled": true},
		{},
	}
	for _, body := range payloads {
		s := govServer(t)
		s.deps.Config.Governor.Modes = map[string]config.ModeConfig{
			"idle":  {Cadences: map[string]config.Cadence{"telemetry": config.NewIntervalCadence("6h"), "operations": config.NewIntervalCadence("paused")}},
			"surge": {Cadences: map[string]config.Cadence{"telemetry": config.NewIntervalCadence("15m")}},
		}
		before := snapshotCadences(s.deps.Config)
		// Fail closed. If snapshotCadences ever stops seeing the cadences --
		// a refactor of ModeConfig, a renamed field -- then before and after
		// are both empty, they compare equal, and this guard passes while
		// protecting nothing.
		if len(before) != 3 {
			t.Fatalf("fixture snapshot has %d cadences, want 3; the snapshot helper has drifted and this guard would be vacuous", len(before))
		}

		rec := doPut(s, "/api/config/governor/project-observability", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("body %#v: PUT = %d: %s", body, rec.Code, rec.Body.String())
		}

		after := snapshotCadences(s.deps.Config)
		if !maps.Equal(before, after) {
			t.Errorf("body %#v mutated governor cadences:\n  before %v\n  after  %v", body, before, after)
		}
	}
}

// snapshotCadences flattens every mode's cadence map into a comparable form,
// so a rewrite in ANY mode for ANY agent shows up -- not just the two agents
// this tab used to write.
func snapshotCadences(cfg *config.Config) map[string]string {
	out := map[string]string{}
	for modeName, mode := range cfg.Governor.Modes {
		for agent, cadence := range mode.Cadences {
			out[modeName+"/"+agent] = cadence.String()
		}
	}
	return out
}

func TestGovernorProjectObservabilityRejectsLiteralsAndUnknownPlatforms(t *testing.T) {
	for _, body := range []map[string]any{
		{"commercial": []string{"unknown-vendor"}},
		{"references": map[string]any{"honeycomb": map[string]string{"endpoint_env": "https://api.honeycomb.io"}}},
		{"references": map[string]any{"honeycomb": map[string]string{"credential_secret": "sk-live-secret"}}},
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
