package config

import (
	"sort"
	"strings"
	"testing"
	"time"
)

func autoscaleTestConfig(enabled bool, replicas int) *Config {
	cfg := &Config{AgentRuntimeConfig: AgentRuntimeConfig{
		Agents: map[string]AgentConfig{
			"scanner":  {ID: "scanner", Enabled: true},
			"reviewer": {ID: "reviewer", Enabled: true, Role: "reviewer", Replicas: replicas},
		},
	}}
	cfg.Review.Reviewers.Autoscale = ReviewerAutoscaleConfig{Enabled: enabled, Min: 1, Max: 4}
	return cfg
}

func agentNames(cfg *Config) []string {
	out := make([]string, 0, len(cfg.Agents))
	for n := range cfg.Agents {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func TestReviewerAutoscaleDefaults(t *testing.T) {
	var a ReviewerAutoscaleConfig
	if a.EffectiveMin() != DefaultReviewerAutoscaleMin || a.EffectiveMax() != DefaultReviewerAutoscaleMax {
		t.Fatalf("band = %d..%d", a.EffectiveMin(), a.EffectiveMax())
	}
	if a.EffectiveCooldown() != DefaultReviewerAutoscaleCooldown || a.EffectiveScaleUpCycles() != DefaultReviewerAutoscaleScaleUpCycles || a.EffectiveScaleDownCycles() != DefaultReviewerAutoscaleScaleDownCycles {
		t.Fatalf("cycle defaults wrong: %+v", a)
	}
	a.Max = MaxAgentReplicas + 3
	if a.EffectiveMax() != MaxAgentReplicas {
		t.Fatalf("max not capped: %d", a.EffectiveMax())
	}
	a.Cooldown = "90s"
	if a.EffectiveCooldown() != 90*time.Second {
		t.Fatalf("cooldown = %s", a.EffectiveCooldown())
	}
}

func TestReviewerAutoscaleValidate(t *testing.T) {
	cases := []struct {
		name string
		cfg  ReviewerAutoscaleConfig
		want string
	}{
		{"disabled anything goes", ReviewerAutoscaleConfig{Min: 9, Max: 1}, ""},
		{"min above max", ReviewerAutoscaleConfig{Enabled: true, Min: 3, Max: 2}, "min 3 is greater than max 2"},
		{"max above cap", ReviewerAutoscaleConfig{Enabled: true, Max: MaxAgentReplicas + 1}, "exceeds the replica cap"},
		{"bad cooldown", ReviewerAutoscaleConfig{Enabled: true, Cooldown: "soon"}, "cooldown"},
		{"ok", ReviewerAutoscaleConfig{Enabled: true, Min: 1, Max: 4, Cooldown: "5m"}, ""},
	}
	for _, tc := range cases {
		err := tc.cfg.Validate()
		if tc.want == "" && err != nil {
			t.Fatalf("%s: unexpected %v", tc.name, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestReviewerAutoscaleAgentResolution(t *testing.T) {
	cfg := autoscaleTestConfig(true, 1)
	if got := cfg.ReviewerAutoscaleAgent(); got != "reviewer" {
		t.Fatalf("role lookup = %q", got)
	}
	cfg.Review.Reviewers.Autoscale.Agent = "scanner"
	if got := cfg.ReviewerAutoscaleAgent(); got != "scanner" {
		t.Fatalf("explicit = %q", got)
	}
	cfg.Review.Reviewers.Autoscale.Agent = "ghost"
	if got := cfg.ReviewerAutoscaleAgent(); got != "" {
		t.Fatalf("unknown explicit = %q", got)
	}
	cfg.Review.Reviewers.Autoscale.Agent = ""
	cfg.Agents["reviewer"] = AgentConfig{ID: "reviewer", Enabled: true}
	cfg.Review.ReviewerAgents = []string{"scanner"}
	if got := cfg.ReviewerAutoscaleAgent(); got != "scanner" {
		t.Fatalf("reviewer_agents fallback = %q", got)
	}
}

func TestExpandAgentReplicasUsesAutoscaleActiveCount(t *testing.T) {
	cfg := autoscaleTestConfig(true, 2)
	if err := cfg.ExpandAgentReplicas(); err != nil {
		t.Fatal(err)
	}
	if got := agentNames(cfg); strings.Join(got, " ") != "reviewer reviewer-2 scanner" {
		t.Fatalf("initial expansion = %v", got)
	}
	if err := cfg.SetReviewerAutoscaleActive(4); err != nil {
		t.Fatal(err)
	}
	if got := agentNames(cfg); strings.Join(got, " ") != "reviewer reviewer-2 reviewer-3 reviewer-4 scanner" {
		t.Fatalf("scaled up = %v", got)
	}
	r4 := cfg.Agents["reviewer-4"]
	if r4.ReplicaOf != "reviewer" || r4.ReplicaIndex != 4 || r4.ReplicaCount != 4 || r4.Role != "reviewer" || r4.BeadsDir != "/data/beads/reviewer-4" {
		t.Fatalf("reviewer-4 = %+v", r4)
	}
	// The declared value is what persists; only the runtime count moved.
	if cfg.Agents["reviewer"].Replicas != 2 {
		t.Fatalf("declared replicas changed: %d", cfg.Agents["reviewer"].Replicas)
	}
	if got := cfg.ReviewerAutoscaleReplicas(); strings.Join(got, " ") != "reviewer reviewer-2 reviewer-3 reviewer-4" {
		t.Fatalf("lane = %v", got)
	}
	if err := cfg.SetReviewerAutoscaleActive(1); err != nil {
		t.Fatal(err)
	}
	if got := agentNames(cfg); strings.Join(got, " ") != "reviewer scanner" {
		t.Fatalf("scaled down = %v", got)
	}
	// Out-of-band targets clamp into the band.
	if err := cfg.SetReviewerAutoscaleActive(9); err != nil || cfg.ReviewerAutoscaleActive != 4 {
		t.Fatalf("clamp: active=%d err=%v", cfg.ReviewerAutoscaleActive, err)
	}
}

func TestExpandAgentReplicasAutoscaleClampsDeclared(t *testing.T) {
	cfg := autoscaleTestConfig(true, 5)
	if err := cfg.ExpandAgentReplicas(); err != nil {
		t.Fatal(err)
	}
	if got := agentNames(cfg); strings.Join(got, " ") != "reviewer reviewer-2 reviewer-3 reviewer-4 scanner" {
		t.Fatalf("declared above max = %v", got)
	}
	cfg = autoscaleTestConfig(false, 5)
	cfg.ReviewerAutoscaleActive = 2
	if err := cfg.ExpandAgentReplicas(); err != nil {
		t.Fatal(err)
	}
	if got := agentNames(cfg); strings.Join(got, " ") != "reviewer reviewer-2 reviewer-3 reviewer-4 reviewer-5 scanner" {
		t.Fatalf("autoscale off ignores active: %v", got)
	}
}
