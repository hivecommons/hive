package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const generalAdvancedPath = "/api/config/governor/general-advanced"

// The governor eval-loop pacing keys round-trip through
// PUT /api/config/governor/general-advanced; invalid values are rejected
// without changing anything, a cleared field restores the default, and
// thresholds_source is reported read-only.
func TestGovernorLoopSettingsParityBatch7RoundTrip(t *testing.T) {
	s := covApiServer(t)
	g := &s.deps.Config.Governor
	g.EvalIntervalS = 300
	g.EvalIntervalMaxS = 1800
	g.EvalIntervalWebhookS = 900
	g.ConserveIntervalMultiplier = 2
	g.OptionalSweepEveryNCycles = 1
	before := *g

	for _, bad := range []map[string]any{
		{"eval_interval_max_s": -1},
		{"eval_interval_max_s": 86401},
		{"eval_interval_webhook_s": -1},
		{"conserve_interval_multiplier": -1},
		{"optional_sweep_every_n_cycles": -1},
		{"provider_budget_probe_interval_s": -1},
		{"provider_budget_probe_interval_s": 86401},
		{"eval_interval_max_s": 200},
		{"eval_interval_s": 2000},
		{"eval_interval_s": 2000, "eval_interval_max_s": 0},
		{"explain_mode": "brief", "conserve_interval_multiplier": -3},
	} {
		if rec := doPut(s, generalAdvancedPath, bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("%v: expected 400, got %d — %s", bad, rec.Code, rec.Body.String())
		}
	}
	if g.EvalIntervalS != before.EvalIntervalS || g.EvalIntervalMaxS != before.EvalIntervalMaxS ||
		g.ConserveIntervalMultiplier != before.ConserveIntervalMultiplier || g.ExplainMode != before.ExplainMode {
		t.Fatal("a rejected PUT changed the config")
	}

	rec := doPut(s, generalAdvancedPath, map[string]any{
		"eval_interval_max_s":              3600,
		"eval_interval_webhook_s":          600,
		"conserve_interval_multiplier":     3,
		"optional_sweep_every_n_cycles":    2,
		"provider_budget_probe_interval_s": 900,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("general-advanced PUT: %d — %s", rec.Code, rec.Body.String())
	}
	if g.EvalIntervalMaxS != 3600 || g.EvalIntervalWebhookS != 600 || g.ConserveIntervalMultiplier != 3 ||
		g.OptionalSweepEveryNCycles != 2 || g.ProviderBudget.ProbeIntervalS != 900 {
		t.Fatalf("loop settings not applied: %+v", *g)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]float64{
		"eval_interval_max_s":              3600,
		"eval_interval_webhook_s":          600,
		"conserve_interval_multiplier":     3,
		"optional_sweep_every_n_cycles":    2,
		"provider_budget_probe_interval_s": 900,
	} {
		if resp[key] != want {
			t.Errorf("response %s = %v, want %v", key, resp[key], want)
		}
	}

	// A partial PUT changes only what it carries.
	if rec := doPut(s, generalAdvancedPath, map[string]any{"explain_mode": "brief"}); rec.Code != http.StatusOK {
		t.Fatalf("partial PUT: %d — %s", rec.Code, rec.Body.String())
	}
	if g.EvalIntervalMaxS != 3600 || g.ConserveIntervalMultiplier != 3 || g.ExplainMode != "brief" {
		t.Fatalf("partial PUT changed unrelated keys: %+v", *g)
	}

	// Clearing restores the defaults applyDefaults would set at Load.
	if rec := doPut(s, generalAdvancedPath, map[string]any{
		"eval_interval_max_s":              0,
		"eval_interval_webhook_s":          0,
		"conserve_interval_multiplier":     0,
		"optional_sweep_every_n_cycles":    0,
		"provider_budget_probe_interval_s": 0,
	}); rec.Code != http.StatusOK {
		t.Fatalf("clearing loop settings: %d — %s", rec.Code, rec.Body.String())
	}
	if g.EvalIntervalMaxS != 1800 || g.EvalIntervalWebhookS != 900 || g.ConserveIntervalMultiplier != 2 ||
		g.OptionalSweepEveryNCycles != 1 || g.ProviderBudget.ProbeIntervalS != 0 {
		t.Fatalf("cleared loop settings did not restore defaults: %+v", *g)
	}

	g.ThresholdsSource = "pack"
	rec = doOwnerGet(s, generalAdvancedPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("general-advanced GET: %d", rec.Code)
	}
	resp = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["thresholds_source"] != "pack" {
		t.Fatalf("thresholds_source = %v, want pack", resp["thresholds_source"])
	}
	if _, ok := resp["loop_defaults"].(map[string]any); !ok {
		t.Fatalf("loop_defaults missing: %v", resp["loop_defaults"])
	}
	// thresholds_source is display-only: the PUT does not accept it.
	if rec := doPut(s, generalAdvancedPath, map[string]any{"thresholds_source": ""}); rec.Code != http.StatusOK {
		t.Fatalf("unknown-field PUT: %d — %s", rec.Code, rec.Body.String())
	}
	if g.ThresholdsSource != "pack" {
		t.Fatal("general-advanced PUT wrote thresholds_source")
	}
}

// The merger queue label round-trips through PUT /api/config/governor/labels
// and is reported, with its default, by GET /api/config/governor.
func TestGovernorAutomergeLabelParityBatch7(t *testing.T) {
	s := covApiServer(t)
	for _, bad := range []map[string]any{
		{"automerge": "<b>lgtm</b>"},
		{"automerge": "lgtm,approved"},
	} {
		if rec := doPut(s, "/api/config/governor/labels", bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("%v: expected 400, got %d", bad, rec.Code)
		}
	}
	if s.deps.Config.Governor.Labels.AutoMerge != "" {
		t.Fatal("a rejected PUT changed the automerge label")
	}
	if rec := doPut(s, "/api/config/governor/labels", map[string]any{"automerge": " approved "}); rec.Code != http.StatusOK {
		t.Fatalf("labels PUT: %d — %s", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Governor.Labels.AutoMerge; got != "approved" {
		t.Fatalf("automerge = %q, want approved", got)
	}

	rec := doGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("governor GET: %d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["automergeLabel"] != "approved" || got["automergeLabelDefault"] != "lgtm" {
		t.Fatalf("automergeLabel = %v, default = %v", got["automergeLabel"], got["automergeLabelDefault"])
	}

	if rec := doPut(s, "/api/config/governor/labels", map[string]any{"automerge": ""}); rec.Code != http.StatusOK {
		t.Fatalf("clear PUT: %d — %s", rec.Code, rec.Body.String())
	}
	if s.deps.Config.Governor.Labels.AutoMerge != "" {
		t.Fatal("clearing the automerge label did not restore the default")
	}
}

// The claim mirroring keys and the rotation headroom source round-trip
// through the Features PUT.
func TestGovernorFeaturesParityBatch7RoundTrip(t *testing.T) {
	s := covApiServer(t)
	if rec := doPut(s, opsFeaturesPath, map[string]any{"rotationHeadroomSource": "nope"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad headroom source: expected 400, got %d", rec.Code)
	}
	if s.deps.Config.Governor.Rotation.HeadroomSource != "" {
		t.Fatal("a rejected PUT changed the headroom source")
	}

	if rec := doPut(s, opsFeaturesPath, map[string]any{
		"claimsComment":             false,
		"claimsLabel":               false,
		"claimsEscalateAfterClaims": 4,
		"rotationHeadroomSource":    " CCLeft ",
	}); rec.Code != http.StatusOK {
		t.Fatalf("features PUT: %d — %s", rec.Code, rec.Body.String())
	}
	c := s.deps.Config.Governor.Claims
	if c.CommentEnabled() || c.LabelEnabled() || c.EscalateAfterClaims != 4 || c.EscalationThreshold() != 4 {
		t.Fatalf("claims settings not applied: %+v", c)
	}
	if got := s.deps.Config.Governor.Rotation.HeadroomSource; got != "ccleft" {
		t.Fatalf("headroom source = %q, want ccleft", got)
	}

	rec := doGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("governor GET: %d", rec.Code)
	}
	var got struct {
		Features map[string]any `json:"features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	f := got.Features
	if f["claimsComment"] != false || f["claimsLabel"] != false || f["claimsEscalateAfterClaims"] != float64(4) ||
		f["rotationHeadroomSource"] != "ccleft" {
		t.Fatalf("features response: claimsComment=%v claimsLabel=%v escalate=%v headroom=%v",
			f["claimsComment"], f["claimsLabel"], f["claimsEscalateAfterClaims"], f["rotationHeadroomSource"])
	}

	// Negative turns the stall gate off; builtin is stored as the default.
	if rec := doPut(s, opsFeaturesPath, map[string]any{"claimsEscalateAfterClaims": -5, "rotationHeadroomSource": "builtin"}); rec.Code != http.StatusOK {
		t.Fatalf("reset PUT: %d — %s", rec.Code, rec.Body.String())
	}
	c = s.deps.Config.Governor.Claims
	if c.EscalateAfterClaims != -1 || c.EscalationThreshold() != 0 || c.CommentEnabled() {
		t.Fatalf("reset PUT did not reset only what it carried: %+v", c)
	}
	if s.deps.Config.Governor.Rotation.HeadroomSource != "" {
		t.Fatal("builtin was not stored as the default")
	}
}

// Every governor key this batch exposes renders with its YAML-key hint next
// to a control bound to the right save path.
func TestParityBatch7SettingsRenderEveryKey(t *testing.T) {
	html := indexHTML(t)

	loop := extractJSFunction(t, html, "renderGovLoopPacing")
	for _, c := range []struct{ yaml, control string }{
		{"governor.eval_interval_max_s", `num('eval_interval_max_s'`},
		{"governor.eval_interval_webhook_s", `num('eval_interval_webhook_s'`},
		{"governor.conserve_interval_multiplier", `num('conserve_interval_multiplier'`},
		{"governor.optional_sweep_every_n_cycles", `num('optional_sweep_every_n_cycles'`},
		{"governor.provider_budget.probe_interval_s", `num('provider_budget_probe_interval_s'`},
	} {
		if !strings.Contains(loop, c.control) || !strings.Contains(loop, "'"+c.yaml+"'") {
			t.Errorf("%s has no loop-pacing control with its YAML hint (%s)", c.yaml, c.control)
		}
	}
	if !strings.Contains(loop, `data-arg0="general-advanced"`) {
		t.Error("loop-pacing fields do not stage into the general-advanced section")
	}
	if !strings.Contains(loop, "yamlHint('governor.thresholds_source')") || strings.Contains(loop, `data-arg1="thresholds_source"`) {
		t.Error("governor.thresholds_source must render read-only with its YAML hint")
	}

	general := extractJSFunction(t, html, "renderGovGeneral")
	for _, yaml := range []string{"governor.eval_interval_s", "governor.explain_mode"} {
		if !strings.Contains(general, "yamlHint('"+yaml+"')") {
			t.Errorf("%s has no YAML-key hint on the General tab", yaml)
		}
	}
	if !strings.Contains(general, "renderGovLoopPacing(") {
		t.Error("the General tab does not render the loop-pacing block")
	}

	labels := extractJSFunction(t, html, "renderGovLabels")
	if !strings.Contains(labels, "yamlHint('governor.labels.automerge')") || !strings.Contains(labels, `data-arg0="labels" data-arg1="automerge"`) {
		t.Error("governor.labels.automerge has no Labels-tab control with its YAML hint")
	}

	features := extractJSFunction(t, html, "renderGovFeatures")
	for _, c := range []struct{ yaml, control string }{
		{"governor.claims.comment", `data-section="features" data-key="claimsComment"`},
		{"governor.claims.label", `data-section="features" data-key="claimsLabel"`},
		{"governor.claims.escalate_after_claims", `data-arg1="claimsEscalateAfterClaims" data-arg-types="s,s,N"`},
		{"governor.rotation.headroom_source", `data-arg1="rotationHeadroomSource" data-arg-types="s,s,v"`},
	} {
		if !strings.Contains(features, "yamlHint('"+c.yaml+"')") {
			t.Errorf("%s has no YAML-key hint", c.yaml)
		}
		if !strings.Contains(features, c.control) {
			t.Errorf("%s has no Features control (%s)", c.yaml, c.control)
		}
	}

	// Existing controls that gained a YAML hint in this batch.
	for _, yaml := range []string{
		"governor.cadence_scope",
		"governor.threshold_scaling",
		"governor.health.model_lock",
		"governor.sensing.ttl_seconds",
		"governor.sensing.pullback_seconds",
		"governor.budget.usd",
	} {
		if !strings.Contains(html, "yamlHint('"+yaml+"')") {
			t.Errorf("%s has no YAML-key hint on its existing control", yaml)
		}
	}
}
