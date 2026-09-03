package dashboard

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// generalAdvancedValue reads the general-advanced payload from its GET
// endpoint.
func generalAdvancedValue(t *testing.T, s *Server) (int, bool) {
	t.Helper()
	rec := doOwnerGet(s, "/api/config/governor/general-advanced")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET general-advanced: %d", rec.Code)
	}
	var resp struct {
		EvalIntervalS      int  `json:"eval_interval_s"`
		AttributionTrailer bool `json:"attribution_trailer"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.EvalIntervalS, resp.AttributionTrailer
}

func TestGovGeneralAdvanced_GetAndPut(t *testing.T) {
	s := govServer(t)

	// Untouched config → attribution defaults ON.
	if _, on := generalAdvancedValue(t, s); !on {
		t.Fatal("attribution_trailer must default to on")
	}

	// Non-owner GET → 403 (owner-only surface).
	if rec := doGet(s, "/api/config/governor/general-advanced"); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner GET: expected 403, got %d", rec.Code)
	}

	// Bad body → 400.
	if rec := doPutRaw(s, "/api/config/governor/general-advanced", "bad"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: expected 400, got %d", rec.Code)
	}

	// Out-of-range eval interval → 400, nothing stored.
	if rec := doPut(s, "/api/config/governor/general-advanced", map[string]any{"eval_interval_s": 5}); rec.Code != http.StatusBadRequest {
		t.Fatalf("interval 5: expected 400, got %d", rec.Code)
	}
	if rec := doPut(s, "/api/config/governor/general-advanced", map[string]any{"eval_interval_s": 90000}); rec.Code != http.StatusBadRequest {
		t.Fatalf("interval 90000: expected 400, got %d", rec.Code)
	}

	// Valid interval persists.
	if rec := doPut(s, "/api/config/governor/general-advanced", map[string]any{"eval_interval_s": 120}); rec.Code != http.StatusOK {
		t.Fatalf("put interval: %d", rec.Code)
	}
	if s.deps.Config.Governor.EvalIntervalS != 120 {
		t.Fatalf("EvalIntervalS = %d, want 120", s.deps.Config.Governor.EvalIntervalS)
	}
	if got, _ := generalAdvancedValue(t, s); got != 120 {
		t.Fatalf("GET eval_interval_s = %d, want 120", got)
	}

	// Toggle attribution OFF: stored as an explicit false pointer.
	if rec := doPut(s, "/api/config/governor/general-advanced", map[string]any{"attribution_trailer": false}); rec.Code != http.StatusOK {
		t.Fatalf("put false: %d", rec.Code)
	}
	if s.deps.Config.Governor.AttributionTrailer == nil || *s.deps.Config.Governor.AttributionTrailer {
		t.Fatal("explicit false must be stored as an explicit false pointer")
	}
	if _, on := generalAdvancedValue(t, s); on {
		t.Fatal("GET must reflect the trailer off")
	}

	// Absent fields → no change (partial-update convention).
	if rec := doPut(s, "/api/config/governor/general-advanced", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("put empty: %d", rec.Code)
	}
	if got, on := generalAdvancedValue(t, s); got != 120 || on {
		t.Fatalf("empty body must not change stored values: got %d/%v", got, on)
	}

	// Toggle back ON.
	if rec := doPut(s, "/api/config/governor/general-advanced", map[string]any{"attribution_trailer": true}); rec.Code != http.StatusOK {
		t.Fatalf("put true: %d", rec.Code)
	}
	if _, on := generalAdvancedValue(t, s); !on {
		t.Fatal("GET must reflect the trailer back on")
	}
}

// explainModeValue reads the three explain-mode fields from the
// general-advanced GET payload: configured, resolved, and where it came from.
func explainModeValue(t *testing.T, s *Server) (configured, effective, source string) {
	t.Helper()
	rec := doOwnerGet(s, "/api/config/governor/general-advanced")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET general-advanced: %d", rec.Code)
	}
	var resp struct {
		ExplainMode          string `json:"explain_mode"`
		ExplainModeEffective string `json:"explain_mode_effective"`
		ExplainModeSource    string `json:"explain_mode_source"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.ExplainMode, resp.ExplainModeEffective, resp.ExplainModeSource
}

// #4712: an operator could not find where the hive's default explain mode is
// configured, because it only existed as a deployment env var. This is the
// form field they were looking for.
func TestGovGeneralAdvanced_ExplainMode(t *testing.T) {
	t.Setenv(config.ExplainModeEnvVar, "")
	s := govServer(t)

	// Untouched config → no hive default, so the resolved answer is off and
	// the UI can say "not set" rather than implying a choice was made.
	if configured, effective, source := explainModeValue(t, s); configured != "" || effective != config.ExplainModeOff || source != "" {
		t.Fatalf("unset: got %q/%q/%q, want \"\"/%q/\"\"", configured, effective, source, config.ExplainModeOff)
	}

	// An unknown mode is rejected rather than stored — it would read back as a
	// set default that silently does nothing.
	if rec := doPut(s, "/api/config/governor/general-advanced", map[string]any{"explain_mode": "verbose"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("explain_mode verbose: expected 400, got %d", rec.Code)
	}
	if s.deps.Config.Governor.ExplainMode != "" {
		t.Fatalf("rejected value must not be stored, got %q", s.deps.Config.Governor.ExplainMode)
	}

	// Valid mode persists and is reported as config-sourced.
	if rec := doPut(s, "/api/config/governor/general-advanced", map[string]any{"explain_mode": config.ExplainModeBrief}); rec.Code != http.StatusOK {
		t.Fatalf("put brief: %d", rec.Code)
	}
	if s.deps.Config.Governor.ExplainMode != config.ExplainModeBrief {
		t.Fatalf("Governor.ExplainMode = %q, want %q", s.deps.Config.Governor.ExplainMode, config.ExplainModeBrief)
	}
	if configured, effective, source := explainModeValue(t, s); configured != config.ExplainModeBrief || effective != config.ExplainModeBrief || source != config.ExplainModeSourceConfig {
		t.Fatalf("after put: got %q/%q/%q", configured, effective, source)
	}

	// Absent field → no change, matching the partial-update convention the
	// other fields on this endpoint follow.
	if rec := doPut(s, "/api/config/governor/general-advanced", map[string]any{"eval_interval_s": 120}); rec.Code != http.StatusOK {
		t.Fatalf("put interval: %d", rec.Code)
	}
	if configured, _, _ := explainModeValue(t, s); configured != config.ExplainModeBrief {
		t.Fatalf("absent explain_mode must not clear it, got %q", configured)
	}

	// Empty string is a real value: it CLEARS the hive default and hands the
	// question back to the environment.
	if rec := doPut(s, "/api/config/governor/general-advanced", map[string]any{"explain_mode": ""}); rec.Code != http.StatusOK {
		t.Fatalf("put empty: %d", rec.Code)
	}
	if configured, effective, source := explainModeValue(t, s); configured != "" || effective != config.ExplainModeOff || source != "" {
		t.Fatalf("after clear: got %q/%q/%q", configured, effective, source)
	}
}

// With no default in config, the deployment env var still supplies one — and
// the GET says so, so an operator can see a default they did not set.
func TestGovGeneralAdvanced_ExplainModeEnvFallbackIsAttributed(t *testing.T) {
	t.Setenv(config.ExplainModeEnvVar, config.ExplainModeFull)
	s := govServer(t)

	configured, effective, source := explainModeValue(t, s)
	if configured != "" {
		t.Errorf("configured = %q, want empty (the env var is not a config value)", configured)
	}
	if effective != config.ExplainModeFull {
		t.Errorf("effective = %q, want %q", effective, config.ExplainModeFull)
	}
	if source != config.ExplainModeSourceEnv {
		t.Errorf("source = %q, want %q", source, config.ExplainModeSourceEnv)
	}

	// A config value overrides what the deployment set — the point of #4712 for
	// hosted owners, who cannot change the env var at all.
	if rec := doPut(s, "/api/config/governor/general-advanced", map[string]any{"explain_mode": config.ExplainModeOff}); rec.Code != http.StatusOK {
		t.Fatalf("put off: %d", rec.Code)
	}
	if _, effective, source := explainModeValue(t, s); effective != config.ExplainModeOff || source != config.ExplainModeSourceConfig {
		t.Fatalf("config must win over env: got %q/%q", effective, source)
	}
}
