package dashboard

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/issueclaim"
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
		"formalEnabled":      true,
		"claimsEnabled":      true,
		"claimsTtlS":         7200,
	}); rec.Code != http.StatusOK {
		t.Fatalf("features ok: %d", rec.Code)
	}

	cfg := s.deps.Config
	if !cfg.Governor.Claims.Enabled || cfg.Governor.Claims.TTLS != 7200 {
		t.Errorf("claims = %+v, want enabled with ttl_s 7200", cfg.Governor.Claims)
	}
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
	if !cfg.Quality.Formal {
		t.Errorf("formal verification toggle not enabled")
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

func TestCovGov_FeaturesFormalRoundTripAndGate(t *testing.T) {
	s := covApiServer(t)
	level := config.FormalQualityMinACMMLevel
	s.deps.Config.ACMMLevel = &level

	if rec := putFeatures(s, map[string]any{"formalEnabled": true}, func(r *http.Request) {
		r.Header.Set("X-Hive-Role", "read-write")
	}); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner PUT formalEnabled = %d, want 403", rec.Code)
	}

	if rec := doPut(s, "/api/config/governor/features", map[string]any{"formalEnabled": true}); rec.Code != http.StatusOK {
		t.Fatalf("formal toggle PUT: %d — %s", rec.Code, rec.Body.String())
	}
	if !s.deps.Config.Quality.Formal {
		t.Fatal("quality.formal was not persisted into config")
	}

	rec := doOwnerGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET governor config: %d — %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Features struct {
			FormalEnabled      bool `json:"formalEnabled"`
			FormalAvailable    bool `json:"formalAvailable"`
			FormalMinACMMLevel int  `json:"formalMinACMMLevel"`
			ACMMLevel          int  `json:"acmmLevel"`
		} `json:"features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding governor payload: %v", err)
	}
	if !payload.Features.FormalEnabled || !payload.Features.FormalAvailable {
		t.Fatalf("formal payload = %+v, want enabled and available", payload.Features)
	}
	if payload.Features.FormalMinACMMLevel != config.FormalQualityMinACMMLevel || payload.Features.ACMMLevel != level {
		t.Fatalf("formal gate payload = %+v, want min/acmm %d", payload.Features, level)
	}

	low := config.FormalQualityMinACMMLevel - 1
	s.deps.Config.ACMMLevel = &low
	rec = doOwnerGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET low-ACMM governor config: %d — %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding low-ACMM governor payload: %v", err)
	}
	if !payload.Features.FormalEnabled || payload.Features.FormalAvailable {
		t.Fatalf("low-ACMM formal payload = %+v, want persisted enabled but unavailable", payload.Features)
	}
}

// review.plan_match.enabled (#8317) follows the formal toggle's contract:
// owner-only, absent key leaves it alone, explicit false turns it off, and
// the GET reports it so the Features tab can prefill the switch.
func TestCovGov_FeaturesPlanMatchToggle(t *testing.T) {
	s := covApiServer(t)
	if s.deps.Config.Review.PlanMatch.Enabled {
		t.Fatal("plan_match must default off")
	}
	if rec := putFeatures(s, map[string]any{"planMatchEnabled": true}, func(r *http.Request) {
		r.Header.Set("X-Hive-Role", "read-write")
	}); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner PUT planMatchEnabled = %d, want 403", rec.Code)
	}
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"planMatchEnabled": true}); rec.Code != http.StatusOK {
		t.Fatalf("plan_match toggle PUT: %d — %s", rec.Code, rec.Body.String())
	}
	if !s.deps.Config.Review.PlanMatch.Enabled {
		t.Fatal("review.plan_match.enabled was not persisted into config")
	}
	// A PUT that does not mention the key leaves it as it was.
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"retroEnabled": true}); rec.Code != http.StatusOK {
		t.Fatalf("unrelated PUT: %d", rec.Code)
	}
	if !s.deps.Config.Review.PlanMatch.Enabled {
		t.Fatal("absent planMatchEnabled key cleared the toggle")
	}

	rec := doOwnerGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET governor config: %d — %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Features struct {
			PlanMatchEnabled bool `json:"planMatchEnabled"`
		} `json:"features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding governor payload: %v", err)
	}
	if !payload.Features.PlanMatchEnabled {
		t.Fatalf("features payload does not report plan_match on: %s", rec.Body.String())
	}

	if rec := doPut(s, "/api/config/governor/features", map[string]any{"planMatchEnabled": false}); rec.Code != http.StatusOK {
		t.Fatalf("plan_match off PUT: %d", rec.Code)
	}
	if s.deps.Config.Review.PlanMatch.Enabled {
		t.Fatal("explicit false did not turn plan_match off")
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

func TestGovernorFeaturesRotationRoundTrip(t *testing.T) {
	s := covApiServer(t)

	if rec := doPut(s, "/api/config/governor/features", map[string]any{
		"rotationEnabled":            true,
		"rotationThresholdPct":       72,
		"rotationHighVolumeCadenceS": 900,
		"rotationProviders": map[string]any{
			"github": map[string]any{
				"class":             "subscription",
				"backends":          []string{"copilot"},
				"monthly_allowance": 1000,
			},
			"anthropic": map[string]any{
				"class":    "metered",
				"backends": []string{"claude"},
			},
		},
		"rotationAgents": map[string]string{"worker": "T1", "planner": "T2"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("rotation features update status = %d; body=%q", rec.Code, rec.Body.String())
	}

	rot := s.deps.Config.Governor.Rotation
	if !rot.Enabled || rot.ThresholdPct != 72 || rot.HighVolumeCadenceS != 900 {
		t.Fatalf("rotation scalars = %+v", rot)
	}
	if got := rot.Providers["github"].Backends; len(got) != 1 || got[0] != "copilot" {
		t.Fatalf("github backends = %v", got)
	}
	if rot.Providers["github"].MonthlyAllowance != 1000 || rot.Providers["github"].Class != "subscription" {
		t.Fatalf("github provider config = %+v", rot.Providers["github"])
	}
	if rot.AgentTiers["worker"] != "T1" || rot.AgentTiers["planner"] != "T2" {
		t.Fatalf("agent tiers = %+v", rot.AgentTiers)
	}

	rec := doOwnerGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("governor config status = %d; body=%q", rec.Code, rec.Body.String())
	}
	var payload struct {
		Features struct {
			RotationEnabled            bool                      `json:"rotationEnabled"`
			RotationThresholdPct       int                       `json:"rotationThresholdPct"`
			RotationHighVolumeCadenceS int                       `json:"rotationHighVolumeCadenceS"`
			RotationProviders          map[string]map[string]any `json:"rotationProviders"`
			RotationAgents             map[string]string         `json:"rotationAgents"`
		} `json:"features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("invalid governor JSON: %v", err)
	}
	if !payload.Features.RotationEnabled || payload.Features.RotationThresholdPct != 72 || payload.Features.RotationHighVolumeCadenceS != 900 {
		t.Fatalf("rotation features response = %+v", payload.Features)
	}
	if payload.Features.RotationAgents["worker"] != "T1" {
		t.Fatalf("rotation agents response = %+v", payload.Features.RotationAgents)
	}
	if payload.Features.RotationProviders["github"]["class"] != "subscription" {
		t.Fatalf("rotation providers response = %+v", payload.Features.RotationProviders)
	}
}

func TestGovernorFeaturesRotationDefaults(t *testing.T) {
	s := covApiServer(t)

	rec := doOwnerGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("governor config status = %d; body=%q", rec.Code, rec.Body.String())
	}
	var payload struct {
		Features struct {
			RotationThresholdPct       int `json:"rotationThresholdPct"`
			RotationHighVolumeCadenceS int `json:"rotationHighVolumeCadenceS"`
		} `json:"features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("invalid governor JSON: %v", err)
	}
	if payload.Features.RotationThresholdPct != 85 || payload.Features.RotationHighVolumeCadenceS != 1800 {
		t.Fatalf("rotation defaults = %+v", payload.Features)
	}
}

func TestGovernorFeaturesRotationValidation(t *testing.T) {
	s := covApiServer(t)
	cases := []struct {
		name string
		body map[string]any
	}{
		{name: "threshold zero", body: map[string]any{"rotationThresholdPct": 0}},
		{name: "threshold too high", body: map[string]any{"rotationThresholdPct": 101}},
		{name: "bad tier", body: map[string]any{"rotationAgents": map[string]string{"worker": "T4"}}},
		{name: "unknown backend", body: map[string]any{
			"rotationProviders": map[string]any{
				"github": map[string]any{"class": "subscription", "backends": []string{"unknown-cli"}},
			},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := doPut(s, "/api/config/governor/features", tc.body); rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
			}
		})
	}
}

// #8380: the issue-claims toggle follows the formalEnabled pattern — off by
// default, owner-writable, reported on the governor GET with the effective
// TTL, and a negative TTL is refused.
func TestGovernorFeatures_IssueClaimsToggle(t *testing.T) {
	s := covApiServer(t)

	rec := doOwnerGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET governor config: %d — %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Features struct {
			ClaimsEnabled bool `json:"claimsEnabled"`
			ClaimsTTLS    int  `json:"claimsTtlS"`
		} `json:"features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding governor payload: %v", err)
	}
	if payload.Features.ClaimsEnabled {
		t.Fatal("claims must report off by default")
	}
	if payload.Features.ClaimsTTLS != int(issueclaim.DefaultTTL.Seconds()) {
		t.Fatalf("claimsTtlS = %d, want the default %d", payload.Features.ClaimsTTLS, int(issueclaim.DefaultTTL.Seconds()))
	}

	if rec := doPut(s, "/api/config/governor/features", map[string]any{"claimsTtlS": -1}); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative ttl accepted: %d", rec.Code)
	}
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"claimsEnabled": true, "claimsTtlS": 600}); rec.Code != http.StatusOK {
		t.Fatalf("PUT claims: %d — %s", rec.Code, rec.Body.String())
	}
	rec = doOwnerGet(s, "/api/config/governor")
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding governor payload: %v", err)
	}
	if !payload.Features.ClaimsEnabled || payload.Features.ClaimsTTLS != 600 {
		t.Fatalf("claims payload = %+v, want enabled with 600s", payload.Features)
	}
}
