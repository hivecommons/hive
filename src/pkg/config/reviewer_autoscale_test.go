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
		t.Fatalf("keyword lookup = %q", got)
	}
	// scanner takes no reviews, so naming it resolves to nothing rather
	// than scaling a lane the swarm never dispatches to.
	cfg.Review.Reviewers.Autoscale.Agent = "scanner"
	if got := cfg.ReviewerAutoscaleAgent(); got != "" {
		t.Fatalf("explicit non-reviewer = %q", got)
	}
	cfg.Review.Reviewers.Autoscale.Agent = "ghost"
	if got := cfg.ReviewerAutoscaleAgent(); got != "" {
		t.Fatalf("unknown explicit = %q", got)
	}
	cfg.Review.Reviewers.Autoscale.Agent = ""
	cfg.Agents["reviewer"] = AgentConfig{ID: "reviewer", Enabled: true}
	cfg.Review.ReviewerAgents = []string{"scanner"}
	if got := cfg.ReviewerAutoscaleAgent(); got != "scanner" {
		t.Fatalf("reviewer_agents lookup = %q", got)
	}
	// Listed, so the swarm dispatches to it and it may be named.
	cfg.Review.Reviewers.Autoscale.Agent = "scanner"
	if got := cfg.ReviewerAutoscaleAgent(); got != "scanner" {
		t.Fatalf("explicit listed reviewer = %q", got)
	}
}

// autoscaleTwoReviewerRolesConfig is the shape a pack ships: two agents with
// role "reviewer", of which review.reviewer_agents names only the swarm's
// reviewer. The other works a different queue and receives no review.
func autoscaleTwoReviewerRolesConfig() *Config {
	cfg := &Config{AgentRuntimeConfig: AgentRuntimeConfig{
		Agents: map[string]AgentConfig{
			"adjudicator": {ID: "adjudicator", Enabled: true, Role: "reviewer"},
			"reviewer":    {ID: "reviewer", Enabled: true, Role: "reviewer"},
			"scanner":     {ID: "scanner", Enabled: true},
		},
	}}
	cfg.Review.ReviewerAgents = []string{"reviewer"}
	cfg.Review.Reviewers.Autoscale = ReviewerAutoscaleConfig{Enabled: true, Min: 1, Max: 3}
	return cfg
}

func TestReviewerAutoscaleDefaultsToAnAgentTheSwarmDispatchesTo(t *testing.T) {
	// "adjudicator" sorts before "reviewer" and has the same role, but the
	// reviewer list leaves it out: a replica of it could take no review.
	cfg := autoscaleTwoReviewerRolesConfig()
	if got, reason := cfg.ReviewerAutoscaleLane(); got != "reviewer" || reason != "" {
		t.Fatalf("default lane = %q (%s), want reviewer", got, reason)
	}
	if !cfg.ReviewerAutoscaleEnabled() {
		t.Fatal("autoscale should be on for the swarm's reviewer")
	}

	// The reviewer list is read in its own order, skipping entries that
	// cannot be the lane: unknown, a replica, disabled, on-demand.
	cfg.Agents["off"] = AgentConfig{ID: "off", Enabled: false}
	cfg.Agents["standby"] = AgentConfig{ID: "standby", Enabled: true, OnDemand: true}
	cfg.Review.ReviewerAgents = []string{"ghost", "reviewer-2", "off", "standby", " scanner ", "reviewer"}
	if got := cfg.ReviewerAutoscaleAgent(); got != "scanner" {
		t.Fatalf("first usable listed reviewer = %q, want scanner", got)
	}

	// No reviewer list: the swarm's keyword rule decides, in name order,
	// and an on-demand agent is never dispatched to whatever its role.
	cfg = &Config{AgentRuntimeConfig: AgentRuntimeConfig{
		Agents: map[string]AgentConfig{
			"adjudicator": {ID: "adjudicator", Enabled: true, Role: "reviewer", OnDemand: true},
			"critic":      {ID: "critic", Enabled: true},
			"qa":          {ID: "qa", Enabled: true, LaneKeywords: []string{"Code Review"}},
			"reviewer":    {ID: "reviewer", Enabled: true, Role: "reviewer"},
		},
	}}
	cfg.Review.Reviewers.Autoscale = ReviewerAutoscaleConfig{Enabled: true}
	if got := cfg.ReviewerAutoscaleAgent(); got != "qa" {
		t.Fatalf("keyword lane = %q, want qa", got)
	}

	// Nothing takes reviews: autoscale is not enabled, and says why.
	cfg.Agents = map[string]AgentConfig{
		"critic":  {ID: "critic", Enabled: true},
		"scanner": {ID: "scanner", Enabled: true},
	}
	got, reason := cfg.ReviewerAutoscaleLane()
	if got != "" || !strings.Contains(reason, "review.reviewer_agents") {
		t.Fatalf("lane = %q reason = %q, want none with a reason", got, reason)
	}
	if cfg.ReviewerAutoscaleEnabled() {
		t.Fatal("autoscale must not be enabled with no reviewer to scale")
	}
	if w := ReviewerAutoscaleWarnings(cfg); len(w) != 1 || !strings.Contains(w[0], reason) {
		t.Fatalf("warnings = %v", w)
	}
	var nilCfg *Config
	if got, reason := nilCfg.ReviewerAutoscaleLane(); got != "" || reason == "" {
		t.Fatalf("nil config lane = %q reason = %q", got, reason)
	}
}

func TestReviewerAutoscaleNamedAgentMustTakeReviews(t *testing.T) {
	cfg := autoscaleTwoReviewerRolesConfig()
	cfg.Review.Reviewers.Autoscale.Agent = "adjudicator"
	if got := cfg.ReviewerAutoscaleAgent(); got != "" {
		t.Fatalf("named non-reviewer resolved to %q", got)
	}
	if cfg.ReviewerAutoscaleEnabled() {
		t.Fatal("autoscale must stay off for an agent the swarm does not dispatch to")
	}
	err := cfg.ValidateReviewerAutoscaleAgent()
	if err == nil || !strings.Contains(err.Error(), `"adjudicator"`) || !strings.Contains(err.Error(), "review.reviewer_agents") {
		t.Fatalf("validation = %v, want a refusal naming the agent and the reviewer list", err)
	}
	warnings := ReviewerAutoscaleWarnings(cfg)
	if len(warnings) != 1 || !strings.Contains(warnings[0], `"adjudicator"`) {
		t.Fatalf("warnings = %v, want exactly one naming the agent", warnings)
	}

	cases := []struct {
		name  string
		agent string
		want  string
	}{
		{"unknown", "ghost", "is not a declared agent"},
		{"replica", "reviewer-2", "name the base agent"},
		{"disabled", "off", "is disabled"},
		{"on demand", "standby", "is on-demand"},
	}
	cfg.Agents["off"] = AgentConfig{ID: "off", Enabled: false}
	cfg.Agents["standby"] = AgentConfig{ID: "standby", Enabled: true, OnDemand: true}
	cfg.Agents["reviewer-2"] = AgentConfig{ID: "reviewer-2", Enabled: true, ReplicaOf: "reviewer"}
	cfg.Review.ReviewerAgents = []string{"reviewer", "off", "standby"}
	for _, tc := range cases {
		cfg.Review.Reviewers.Autoscale.Agent = tc.agent
		if err := cfg.ValidateReviewerAutoscaleAgent(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: validation = %v, want %q", tc.name, err, tc.want)
		}
	}

	// With no reviewer list the keyword rule decides, and the refusal says so.
	cfg.Review.ReviewerAgents = nil
	cfg.Review.Reviewers.Autoscale.Agent = "scanner"
	if err := cfg.ValidateReviewerAutoscaleAgent(); err == nil || !strings.Contains(err.Error(), "keywords") {
		t.Fatalf("keyword refusal = %v", err)
	}

	// A reviewer is accepted, and so is anything while autoscale is off.
	cfg.Review.Reviewers.Autoscale.Agent = "reviewer"
	if err := cfg.ValidateReviewerAutoscaleAgent(); err != nil {
		t.Fatalf("reviewer refused: %v", err)
	}
	if w := ReviewerAutoscaleWarnings(cfg); len(w) != 0 {
		t.Fatalf("unexpected warnings: %v", w)
	}
	cfg.Review.Reviewers.Autoscale = ReviewerAutoscaleConfig{Agent: "scanner"}
	if err := cfg.ValidateReviewerAutoscaleAgent(); err != nil {
		t.Fatalf("disabled autoscale refused: %v", err)
	}
	if w := ReviewerAutoscaleWarnings(cfg); len(w) != 0 {
		t.Fatalf("disabled autoscale warned: %v", w)
	}
}

// A hive.yaml written before the agent had to be a reviewer still loads:
// autoscale stays off for it and one warning says why.
func TestLoadKeepsConfigNamingNonReviewerAutoscaleAgent(t *testing.T) {
	path := writeTempConfig(t, `
project:
  org: acme
  repos:
    - widgets
  ai_author: bot
  primary_repo: widgets
github:
  token: ghp_test
agents:
  adjudicator:
    backend: claude
    enabled: true
    role: reviewer
    replicas: 2
  reviewer:
    backend: claude
    enabled: true
    role: reviewer
review:
  reviewer_agents: [reviewer]
  reviewers:
    autoscale:
      enabled: true
      agent: adjudicator
      max: 3
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ReviewerAutoscaleEnabled() {
		t.Fatal("autoscale must be off for a named agent the swarm does not dispatch to")
	}
	if got := cfg.ReviewerAutoscaleReplicas(); got != nil {
		t.Fatalf("lane = %v, want none", got)
	}
	// Every agent keeps its declared replicas.
	if got := agentNames(cfg); strings.Join(got, " ") != "adjudicator adjudicator-2 reviewer" {
		t.Fatalf("agents = %v", got)
	}
	warnings := ReviewerAutoscaleWarnings(cfg)
	if len(warnings) != 1 || !strings.Contains(warnings[0], `"adjudicator"`) || !strings.Contains(warnings[0], "review.reviewer_agents") {
		t.Fatalf("warnings = %v, want exactly one naming the agent and the reviewer list", warnings)
	}
}

// The controller's replica count belongs to the lane it was set for. Moving
// the lane to another agent puts the old one back on its declared replicas
// and starts the new one from its own.
func TestExpandAgentReplicasAutoscaleCountStaysWithItsLane(t *testing.T) {
	cfg := autoscaleTwoReviewerRolesConfig()
	cfg.Review.ReviewerAgents = []string{"adjudicator", "reviewer"}
	cfg.Review.Reviewers.Autoscale.Agent = "adjudicator"
	reviewer := cfg.Agents["reviewer"]
	reviewer.Replicas = 2
	cfg.Agents["reviewer"] = reviewer
	if err := cfg.SetReviewerAutoscaleActive(3); err != nil {
		t.Fatal(err)
	}
	if got := agentNames(cfg); strings.Join(got, " ") != "adjudicator adjudicator-2 adjudicator-3 reviewer reviewer-2 scanner" {
		t.Fatalf("old lane scaled = %v", got)
	}
	if cfg.ReviewerAutoscaleActiveAgent != "adjudicator" {
		t.Fatalf("active agent = %q", cfg.ReviewerAutoscaleActiveAgent)
	}

	cfg.Review.Reviewers.Autoscale.Agent = "reviewer"
	if err := cfg.ExpandAgentReplicas(); err != nil {
		t.Fatal(err)
	}
	if got := agentNames(cfg); strings.Join(got, " ") != "adjudicator reviewer reviewer-2 scanner" {
		t.Fatalf("after the lane moved = %v, want the old lane on 1 and the new lane on its declared 2", got)
	}
	if got := cfg.ReviewerAutoscaleReplicas(); strings.Join(got, " ") != "reviewer reviewer-2" {
		t.Fatalf("lane = %v", got)
	}

	if err := cfg.SetReviewerAutoscaleActive(3); err != nil {
		t.Fatal(err)
	}
	if cfg.ReviewerAutoscaleActiveAgent != "reviewer" || cfg.Agents["reviewer"].ReplicaCount != 3 {
		t.Fatalf("new lane target: agent=%q count=%d", cfg.ReviewerAutoscaleActiveAgent, cfg.Agents["reviewer"].ReplicaCount)
	}
	snapshot, err := cfg.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ReviewerAutoscaleActive != 3 || snapshot.ReviewerAutoscaleActiveAgent != "reviewer" {
		t.Fatalf("snapshot lost the lane target: %d %q", snapshot.ReviewerAutoscaleActive, snapshot.ReviewerAutoscaleActiveAgent)
	}

	if err := cfg.ResetReviewerAutoscaleActive(); err != nil {
		t.Fatal(err)
	}
	if cfg.ReviewerAutoscaleActive != 0 || cfg.ReviewerAutoscaleActiveAgent != "" {
		t.Fatalf("reset left %d %q", cfg.ReviewerAutoscaleActive, cfg.ReviewerAutoscaleActiveAgent)
	}
	if got := agentNames(cfg); strings.Join(got, " ") != "adjudicator reviewer reviewer-2 scanner" {
		t.Fatalf("after reset = %v", got)
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
