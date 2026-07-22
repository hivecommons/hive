package config

import "testing"

func TestTrajectoryDefaultOn(t *testing.T) {
	// nil Enabled (key omitted) → enabled
	var tc TrajectoryConfig
	if !tc.IsEnabled() {
		t.Error("omitted trajectory.enabled must default to on")
	}
	// explicit false → disabled
	f := false
	tc.Enabled = &f
	if tc.IsEnabled() {
		t.Error("explicit trajectory.enabled=false must disable")
	}
	// explicit true → enabled
	tr := true
	tc.Enabled = &tr
	if !tc.IsEnabled() {
		t.Error("explicit trajectory.enabled=true must enable")
	}
}

func TestTrajectoryDefaultsApplied(t *testing.T) {
	c := &Config{}
	c.applyDefaults()
	if c.Governor.Trajectory.OnDivergence != "pause" {
		t.Errorf("on_divergence default = %q, want pause", c.Governor.Trajectory.OnDivergence)
	}
}

func TestResolveReviewerAndReady(t *testing.T) {
	// Falls back to LiteLLM block when trajectory endpoint/model are empty.
	g := &GovernorConfig{}
	g.LiteLLM.Endpoint = "https://litellm.example.com"
	g.LiteLLM.DefaultModel = "claude-haiku-4-5"
	ep, _, model := g.ResolveReviewer()
	if ep != "https://litellm.example.com" || model != "claude-haiku-4-5" {
		t.Fatalf("fallback resolve wrong: ep=%q model=%q", ep, model)
	}
	if !g.ReviewerReady() {
		t.Error("reviewer should be ready with litellm endpoint+model")
	}

	// Trajectory endpoint/model override the LiteLLM block (e.g. point at vLLM).
	g.Trajectory.Endpoint = "http://vllm.hive-inference.svc:8000/"
	g.Trajectory.Model = "deepseek-r1-14b"
	ep, _, model = g.ResolveReviewer()
	if ep != "http://vllm.hive-inference.svc:8000" { // trailing slash trimmed
		t.Errorf("trajectory endpoint should win and be trimmed: %q", ep)
	}
	if model != "deepseek-r1-14b" {
		t.Errorf("trajectory model should win: %q", model)
	}

	// Not ready when nothing resolves.
	empty := &GovernorConfig{}
	if empty.ReviewerReady() {
		t.Error("reviewer must not be ready with no endpoint/model")
	}
}
