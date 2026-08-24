package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Tests for the two governor-config endpoints whose handlers had zero
// coverage: the stall-replan lane (api_governor_features.go) and the
// threshold-scaling curve (api.go). Both follow the governor-config
// contract the other sections already test: owner-gated reads AND writes,
// "only what you send is changed" pointer semantics, and
// validate-before-mutate.

type replanBody struct {
	Enabled         bool `json:"enabled"`
	IntervalS       int  `json:"interval_s"`
	StallThresholdS int  `json:"stall_threshold_s"`
	MaxReplans      int  `json:"max_replans"`
}

func decodeReplan(t *testing.T, rec *httptest.ResponseRecorder) replanBody {
	t.Helper()
	var body replanBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode replan response: %v: %s", err, rec.Body.String())
	}
	return body
}

// --- replan -----------------------------------------------------------------

func TestGovernorReplanGet_OwnerSeesDefaults(t *testing.T) {
	s := covApiServer(t)
	rec := doOwnerGet(s, "/api/config/governor/replan")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET replan: expected 200, got %d", rec.Code)
	}
	body := decodeReplan(t, rec)
	// nil Enabled resolves to the effective default (on); the numeric knobs
	// default to 0 meaning "use the built-in default".
	if !body.Enabled {
		t.Fatalf("default enabled tri-state wrong: %+v", body)
	}
	if body.IntervalS != 0 || body.StallThresholdS != 0 || body.MaxReplans != 0 {
		t.Fatalf("expected zero-valued numeric defaults, got %+v", body)
	}
}

func TestGovernorReplanGet_RejectsNonOwner(t *testing.T) {
	s := covApiServer(t)
	if rec := doGetNoRole(s, "/api/config/governor/replan"); rec.Code != http.StatusForbidden {
		t.Fatalf("un-gated GET replan: expected 403, got %d", rec.Code)
	}
}

func TestGovernorReplanPut_ValidatesAndApplies(t *testing.T) {
	s := covApiServer(t)

	// Malformed body → 400.
	if rec := doPutRaw(s, "/api/config/governor/replan", "{nope"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: expected 400, got %d", rec.Code)
	}
	// Each negative numeric knob must be refused BEFORE mutating.
	for _, field := range []string{"interval_s", "stall_threshold_s", "max_replans"} {
		if rec := doPut(s, "/api/config/governor/replan", map[string]any{field: -1}); rec.Code != http.StatusBadRequest {
			t.Fatalf("negative %s: expected 400, got %d", field, rec.Code)
		}
	}
	rp := s.deps.Config.Governor.Replan
	if rp.IntervalS != 0 || rp.StallThresholdS != 0 || rp.MaxReplans != 0 {
		t.Fatalf("rejected writes still mutated replan config: %+v", rp)
	}

	// Valid write applies every provided field and echoes the section back.
	rec := doPut(s, "/api/config/governor/replan", map[string]any{
		"enabled":           false,
		"interval_s":        120,
		"stall_threshold_s": 900,
		"max_replans":       2,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("valid put: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeReplan(t, rec)
	if body.Enabled || body.IntervalS != 120 || body.StallThresholdS != 900 || body.MaxReplans != 2 {
		t.Fatalf("put response did not echo applied values: %+v", body)
	}
	rp = s.deps.Config.Governor.Replan
	if rp.Enabled == nil || *rp.Enabled {
		t.Fatalf("enabled not applied: %+v", rp.Enabled)
	}
	if rp.IntervalS != 120 || rp.StallThresholdS != 900 || rp.MaxReplans != 2 {
		t.Fatalf("numeric knobs not applied: %+v", rp)
	}

	// Absent keys leave settings untouched (pointer semantics).
	if rec := doPut(s, "/api/config/governor/replan", map[string]any{"max_replans": 5}); rec.Code != http.StatusOK {
		t.Fatalf("partial put: expected 200, got %d", rec.Code)
	}
	rp = s.deps.Config.Governor.Replan
	if rp.Enabled == nil || *rp.Enabled || rp.IntervalS != 120 || rp.StallThresholdS != 900 {
		t.Fatalf("partial put clobbered untouched fields: %+v", rp)
	}
	if rp.MaxReplans != 5 {
		t.Fatalf("partial put did not apply max_replans: %d", rp.MaxReplans)
	}
}

func TestGovernorReplanPut_RejectsNonOwner(t *testing.T) {
	s := covApiServer(t)
	if rec := doPutNoRole(s, "/api/config/governor/replan", `{"max_replans":9}`); rec.Code != http.StatusForbidden {
		t.Fatalf("un-gated PUT replan: expected 403, got %d", rec.Code)
	}
	if s.deps.Config.Governor.Replan.MaxReplans == 9 {
		t.Fatal("refused write still mutated replan config")
	}
}

// --- threshold scaling --------------------------------------------------------

func thresholdScalingOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		ThresholdScaling string `json:"threshold_scaling"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode threshold-scaling response: %v: %s", err, rec.Body.String())
	}
	return body.ThresholdScaling
}

func TestGovernorThresholdScalingGet_DefaultsToLinear(t *testing.T) {
	s := covApiServer(t)
	rec := doOwnerGet(s, "/api/config/governor/threshold-scaling")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET threshold-scaling: expected 200, got %d", rec.Code)
	}
	// Unset resolves to the documented default rather than echoing "".
	if got := thresholdScalingOf(t, rec); got != "linear" {
		t.Fatalf("default scaling: expected linear, got %q", got)
	}
}

func TestGovernorThresholdScalingGet_RejectsNonOwner(t *testing.T) {
	s := covApiServer(t)
	if rec := doGetNoRole(s, "/api/config/governor/threshold-scaling"); rec.Code != http.StatusForbidden {
		t.Fatalf("un-gated GET threshold-scaling: expected 403, got %d", rec.Code)
	}
}

func TestGovernorThresholdScalingPut_ValidatesAndApplies(t *testing.T) {
	s := covApiServer(t)

	// Malformed body → 400.
	if rec := doPutRaw(s, "/api/config/governor/threshold-scaling", "{nope"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: expected 400, got %d", rec.Code)
	}
	// A curve config.validate would reject must be refused BEFORE mutating —
	// this is the same gate, so the write path cannot persist a value that
	// fails the next config load.
	if rec := doPut(s, "/api/config/governor/threshold-scaling", map[string]any{"thresholdScaling": "exponential"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid curve: expected 400, got %d", rec.Code)
	}
	if s.deps.Config.Governor.ThresholdScaling != "" {
		t.Fatalf("rejected write still mutated scaling: %q", s.deps.Config.Governor.ThresholdScaling)
	}

	// camelCase key applies.
	if rec := doPut(s, "/api/config/governor/threshold-scaling", map[string]any{"thresholdScaling": "sqrt"}); rec.Code != http.StatusOK {
		t.Fatalf("sqrt put: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.deps.Config.Governor.ThresholdScaling != "sqrt" {
		t.Fatalf("camelCase key not applied: %q", s.deps.Config.Governor.ThresholdScaling)
	}

	// snake_case alias applies too — callers may echo back the GET's key.
	if rec := doPut(s, "/api/config/governor/threshold-scaling", map[string]any{"threshold_scaling": "none"}); rec.Code != http.StatusOK {
		t.Fatalf("none put: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.deps.Config.Governor.ThresholdScaling != "none" {
		t.Fatalf("snake_case alias not applied: %q", s.deps.Config.Governor.ThresholdScaling)
	}

	// Empty string resets to "unset", and the GET resolves that to linear.
	if rec := doPut(s, "/api/config/governor/threshold-scaling", map[string]any{"thresholdScaling": ""}); rec.Code != http.StatusOK {
		t.Fatalf("reset put: expected 200, got %d", rec.Code)
	}
	if s.deps.Config.Governor.ThresholdScaling != "" {
		t.Fatalf("reset not applied: %q", s.deps.Config.Governor.ThresholdScaling)
	}
	if got := thresholdScalingOf(t, doOwnerGet(s, "/api/config/governor/threshold-scaling")); got != "linear" {
		t.Fatalf("GET after reset: expected linear, got %q", got)
	}
}

func TestGovernorThresholdScalingPut_RejectsNonOwner(t *testing.T) {
	s := covApiServer(t)
	if rec := doPutNoRole(s, "/api/config/governor/threshold-scaling", `{"thresholdScaling":"sqrt"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("un-gated PUT threshold-scaling: expected 403, got %d", rec.Code)
	}
	if s.deps.Config.Governor.ThresholdScaling == "sqrt" {
		t.Fatal("refused write still mutated threshold scaling")
	}
}
