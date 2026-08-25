package dashboard

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
)

// Tests for three owner-gated read/write surfaces that had zero coverage:
// the governor threshold-scaling curve (api.go), the stall-replan lane
// (api_governor_features.go), and the providers headroom snapshot
// (api_providers_headroom.go). All follow the same governor-config contract:
// owner-gated, validate-before-mutate, defaults resolved on read.

// --- threshold scaling -------------------------------------------------------

func TestGovernorThresholdScalingGet_DefaultsToLinear(t *testing.T) {
	s := covApiServer(t)
	rec := doOwnerGet(s, "/api/config/governor/threshold-scaling")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET threshold-scaling: expected 200, got %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Unset config must resolve to the documented default, not "".
	if body["threshold_scaling"] != config.ThresholdScalingLinear {
		t.Fatalf("default curve: expected %q, got %q", config.ThresholdScalingLinear, body["threshold_scaling"])
	}
}

func TestGovernorThresholdScalingGet_RejectsNonOwner(t *testing.T) {
	s := covApiServer(t)
	if rec := doGetNoRole(s, "/api/config/governor/threshold-scaling"); rec.Code != http.StatusForbidden {
		t.Fatalf("un-gated GET: expected 403, got %d", rec.Code)
	}
}

func TestGovernorThresholdScalingPut_RejectsNonOwner(t *testing.T) {
	s := covApiServer(t)
	if rec := doPutNoRole(s, "/api/config/governor/threshold-scaling", `{"thresholdScaling":"sqrt"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("un-gated PUT: expected 403, got %d", rec.Code)
	}
	if s.deps.Config.Governor.ThresholdScaling != "" {
		t.Fatalf("rejected PUT still mutated config: %q", s.deps.Config.Governor.ThresholdScaling)
	}
}

func TestGovernorThresholdScalingPut_ValidatesAndApplies(t *testing.T) {
	s := covApiServer(t)

	// Malformed body → 400.
	if rec := doPutRaw(s, "/api/config/governor/threshold-scaling", "{nope"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: expected 400, got %d", rec.Code)
	}
	// A curve config.validate would reject must be refused BEFORE mutating.
	if rec := doPut(s, "/api/config/governor/threshold-scaling", map[string]any{"thresholdScaling": "cubic"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid curve: expected 400, got %d", rec.Code)
	}
	if s.deps.Config.Governor.ThresholdScaling != "" {
		t.Fatalf("rejected curve still persisted: %q", s.deps.Config.Governor.ThresholdScaling)
	}

	// Valid write via the camelCase key.
	if rec := doPut(s, "/api/config/governor/threshold-scaling", map[string]any{"thresholdScaling": config.ThresholdScalingSqrt}); rec.Code != http.StatusOK {
		t.Fatalf("valid PUT: expected 200, got %d", rec.Code)
	}
	if got := s.deps.Config.Governor.ThresholdScaling; got != config.ThresholdScalingSqrt {
		t.Fatalf("curve not applied: %q", got)
	}
}

func TestGovernorThresholdScalingPut_AcceptsSnakeCaseAndEmpty(t *testing.T) {
	s := covApiServer(t)

	// snake_case alias is the same key the GET returns and the YAML uses.
	if rec := doPut(s, "/api/config/governor/threshold-scaling", map[string]any{"threshold_scaling": config.ThresholdScalingNone}); rec.Code != http.StatusOK {
		t.Fatalf("snake-case PUT: expected 200, got %d", rec.Code)
	}
	if got := s.deps.Config.Governor.ThresholdScaling; got != config.ThresholdScalingNone {
		t.Fatalf("snake-case curve not applied: %q", got)
	}

	// Empty string means "back to the default" and must be accepted.
	if rec := doPut(s, "/api/config/governor/threshold-scaling", map[string]any{"thresholdScaling": ""}); rec.Code != http.StatusOK {
		t.Fatalf("empty PUT: expected 200, got %d", rec.Code)
	}
	if got := s.deps.Config.Governor.ThresholdScaling; got != "" {
		t.Fatalf("reset curve not applied: %q", got)
	}
	// And the GET resolves the reset value back to the default.
	rec := doOwnerGet(s, "/api/config/governor/threshold-scaling")
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["threshold_scaling"] != config.ThresholdScalingLinear {
		t.Fatalf("reset GET: expected %q, got %q", config.ThresholdScalingLinear, body["threshold_scaling"])
	}
}

// --- stall-replan lane -------------------------------------------------------

func TestGovernorReplanGet_OwnerSeesDefaults(t *testing.T) {
	s := covApiServer(t)
	rec := doOwnerGet(s, "/api/config/governor/replan")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET replan: expected 200, got %d", rec.Code)
	}
	var body struct {
		Enabled         bool `json:"enabled"`
		IntervalS       int  `json:"interval_s"`
		StallThresholdS int  `json:"stall_threshold_s"`
		MaxReplans      int  `json:"max_replans"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// nil Enabled resolves to the effective default (on).
	if !body.Enabled {
		t.Fatalf("default enabled tri-state wrong: %+v", body)
	}
}

func TestGovernorReplanGet_RejectsNonOwner(t *testing.T) {
	s := covApiServer(t)
	if rec := doGetNoRole(s, "/api/config/governor/replan"); rec.Code != http.StatusForbidden {
		t.Fatalf("un-gated GET replan: expected 403, got %d", rec.Code)
	}
}

func TestGovernorReplanPut_RejectsNonOwner(t *testing.T) {
	s := covApiServer(t)
	if rec := doPutNoRole(s, "/api/config/governor/replan", `{"enabled":false}`); rec.Code != http.StatusForbidden {
		t.Fatalf("un-gated PUT replan: expected 403, got %d", rec.Code)
	}
	if s.deps.Config.Governor.Replan.Enabled != nil {
		t.Fatal("rejected PUT still mutated enabled tri-state")
	}
}

func TestGovernorReplanPut_ValidatesBeforeMutating(t *testing.T) {
	s := covApiServer(t)

	// Malformed body → 400.
	if rec := doPutRaw(s, "/api/config/governor/replan", "{nope"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: expected 400, got %d", rec.Code)
	}
	// Each negative field must be refused without applying ANY field from the
	// same request — including valid ones sent alongside it.
	for _, raw := range []string{
		`{"interval_s":-1,"max_replans":7}`,
		`{"stall_threshold_s":-5}`,
		`{"max_replans":-2}`,
	} {
		if rec := doPutRaw(s, "/api/config/governor/replan", raw); rec.Code != http.StatusBadRequest {
			t.Fatalf("negative field %s: expected 400, got %d", raw, rec.Code)
		}
	}
	rp := s.deps.Config.Governor.Replan
	if rp.Enabled != nil || rp.IntervalS != 0 || rp.StallThresholdS != 0 || rp.MaxReplans != 0 {
		t.Fatalf("rejected writes still mutated replan config: %+v", rp)
	}
}

func TestGovernorReplanPut_PartialUpdateLeavesOthersUntouched(t *testing.T) {
	s := covApiServer(t)

	// First write sets two fields.
	rec := doPut(s, "/api/config/governor/replan", map[string]any{"interval_s": 120, "max_replans": 3})
	if rec.Code != http.StatusOK {
		t.Fatalf("first PUT: expected 200, got %d", rec.Code)
	}
	// Second write only disables the lane; earlier values must survive.
	rec = doPut(s, "/api/config/governor/replan", map[string]any{"enabled": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("second PUT: expected 200, got %d", rec.Code)
	}
	rp := s.deps.Config.Governor.Replan
	if rp.Enabled == nil || *rp.Enabled {
		t.Fatalf("enabled not applied: %+v", rp.Enabled)
	}
	if rp.IntervalS != 120 || rp.MaxReplans != 3 {
		t.Fatalf("partial update wiped untouched fields: %+v", rp)
	}
	// Response body reflects the post-write state.
	var body struct {
		Enabled    bool `json:"enabled"`
		IntervalS  int  `json:"interval_s"`
		MaxReplans int  `json:"max_replans"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Enabled || body.IntervalS != 120 || body.MaxReplans != 3 {
		t.Fatalf("PUT response out of sync with config: %+v", body)
	}
}

// --- providers headroom ------------------------------------------------------

func TestProvidersHeadroom_RejectsNonOwner(t *testing.T) {
	s := covApiServer(t)
	if rec := doGetNoRole(s, "/api/providers/headroom"); rec.Code != http.StatusForbidden {
		t.Fatalf("un-gated GET headroom: expected 403, got %d", rec.Code)
	}
}

func TestProvidersHeadroom_NilRotationManagerReportsDisabled(t *testing.T) {
	s := covApiServer(t)
	rec := doOwnerGet(s, "/api/providers/headroom")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET headroom: expected 200, got %d", rec.Code)
	}
	var body struct {
		Enabled   bool              `json:"enabled"`
		Providers []json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Rotation disabled must be a well-formed empty snapshot, not an error.
	if body.Enabled {
		t.Fatal("nil rotation manager reported enabled=true")
	}
	if body.Providers == nil || len(body.Providers) != 0 {
		t.Fatalf("expected empty (non-null) provider list, got %v", body.Providers)
	}
}
