package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func reviewHeadsBase() AgentConfig {
	return AgentConfig{
		ID: "code-reviewer", Backend: "claude", Model: "claude-sonnet-5-5", Enabled: true, Role: "reviewer",
		LaunchCmd: "claude --model claude-sonnet-5-5", ReasoningEffort: "high",
		ReviewModels: ReviewModelsConfig{Pool: []ReviewModelPoolEntry{{Backend: "claude", Model: "claude-sonnet-5-5"}}},
		ReviewHeads: []ReviewHead{
			{Backend: "claude", Model: "claude-sonnet-5-5", Alias: "sonnet", Replicas: 2},
			{Backend: "codex", Model: "gpt-6-sol", Alias: "sol"},
		},
	}
}

func TestReviewHeadAliasDefaultsToBackendModelSlug(t *testing.T) {
	for _, tc := range []struct {
		head ReviewHead
		want string
	}{
		{ReviewHead{Backend: "claude", Model: "claude-sonnet-5-5"}, "claude-sonnet-5-5"},
		{ReviewHead{Backend: "codex", Model: "gpt-6-sol"}, "codex-gpt-6-sol"},
		{ReviewHead{Backend: "codex", Model: "GPT 6.Sol"}, "codex-gpt-6-sol"},
		{ReviewHead{Backend: "codex", Model: "gpt-6-sol", Alias: "Sol_6"}, "sol-6"},
	} {
		if got := tc.head.EffectiveAlias(); got != tc.want {
			t.Errorf("EffectiveAlias(%+v) = %q, want %q", tc.head, got, tc.want)
		}
	}
}

func TestExpandAgentReplicasMaterializesReviewHeads(t *testing.T) {
	cfg := &Config{AgentRuntimeConfig: AgentRuntimeConfig{Agents: map[string]AgentConfig{"code-reviewer": reviewHeadsBase()}}}
	if err := cfg.ExpandAgentReplicas(); err != nil {
		t.Fatalf("ExpandAgentReplicas() error = %v", err)
	}
	want := []string{"code-reviewer", "code-reviewer-sonnet-1", "code-reviewer-sonnet-2", "code-reviewer-sol-1"}
	if len(cfg.Agents) != len(want) {
		t.Fatalf("agents = %v, want %v", keys(cfg.Agents), want)
	}
	for _, name := range want {
		if _, ok := cfg.Agents[name]; !ok {
			t.Fatalf("missing agent %s in %v", name, keys(cfg.Agents))
		}
	}
	sonnet2 := cfg.Agents["code-reviewer-sonnet-2"]
	if sonnet2.ReplicaOf != "code-reviewer" || sonnet2.ReviewHead != "code-reviewer-sonnet" || sonnet2.ReplicaIndex != 2 || sonnet2.ReplicaCount != 2 {
		t.Fatalf("sonnet-2 metadata = %+v", sonnet2)
	}
	if sonnet2.LaunchCmd == "" || sonnet2.ReasoningEffort != "high" {
		t.Fatalf("same-backend head should keep backend-specific settings: %+v", sonnet2)
	}
	sol := cfg.Agents["code-reviewer-sol-1"]
	if sol.Backend != "codex" || sol.Model != "gpt-6-sol" || sol.ID != "code-reviewer-sol-1" || sol.BeadsDir != "/data/beads/code-reviewer-sol-1" {
		t.Fatalf("sol head = %+v", sol)
	}
	if sol.LaunchCmd != "" || sol.ReasoningEffort != "" {
		t.Fatalf("cross-backend head kept the base's launch cmd / effort: %+v", sol)
	}
	if sol.ReviewModels.Configured() || len(sol.ReviewHeads) != 0 || sol.Role != "reviewer" || !sol.IsReviewHead() {
		t.Fatalf("head should carry no pool or heads of its own: %+v", sol)
	}
	if base := cfg.Agents["code-reviewer"]; base.IsReviewHead() || base.ReplicaOf != "" {
		t.Fatalf("base marked as head: %+v", base)
	}
	if got := cfg.BaseAgentName("code-reviewer-sol-1"); got != "code-reviewer" {
		t.Fatalf("BaseAgentName = %q", got)
	}

	// Re-expansion is idempotent: derived heads are dropped and rebuilt.
	if err := cfg.ExpandAgentReplicas(); err != nil || len(cfg.Agents) != len(want) {
		t.Fatalf("re-expand: err=%v agents=%v", err, keys(cfg.Agents))
	}
}

func TestReviewHeadsAreStrippedOnSave(t *testing.T) {
	cfg := &Config{AgentRuntimeConfig: AgentRuntimeConfig{Agents: map[string]AgentConfig{"code-reviewer": reviewHeadsBase()}}}
	if err := cfg.ExpandAgentReplicas(); err != nil {
		t.Fatal(err)
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if strings.Contains(text, "code-reviewer-sol-1") || strings.Contains(text, "code-reviewer-sonnet-1") {
		t.Fatalf("derived heads persisted:\n%s", text)
	}
	if !strings.Contains(text, "review_heads:") || !strings.Contains(text, "alias: sol") {
		t.Fatalf("review_heads lost on save:\n%s", text)
	}
}

func TestReviewHeadsValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		heads []ReviewHead
		other map[string]AgentConfig
		want  string
	}{
		{"missing model", []ReviewHead{{Backend: "codex"}}, nil, "backend and model are required"},
		{"duplicate alias", []ReviewHead{{Backend: "codex", Model: "a", Alias: "x"}, {Backend: "claude", Model: "b", Alias: "x"}}, nil, "used twice"},
		{"replica cap", []ReviewHead{{Backend: "codex", Model: "a", Replicas: MaxAgentReplicas + 1}}, nil, "replicas must be between"},
		{"default name too long for the group limit", []ReviewHead{{Backend: "claude", Model: "claude-sonnet-5-5"}}, nil, "set a shorter alias"},
		{"explicit alias too long", []ReviewHead{{Backend: "codex", Model: "a", Alias: "a-very-long-head-alias"}}, nil, "32-character group limit"},
		{"collides with declared", []ReviewHead{{Backend: "codex", Model: "a", Alias: "x"}}, map[string]AgentConfig{"code-reviewer-x-1": {}}, "already exists"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agents := map[string]AgentConfig{"code-reviewer": {Backend: "claude", Model: "m", ReviewHeads: tc.heads}}
			for k, v := range tc.other {
				agents[k] = v
			}
			cfg := &Config{AgentRuntimeConfig: AgentRuntimeConfig{Agents: agents}}
			if err := cfg.ExpandAgentReplicas(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestEffectiveHeadMaxRetriesFallsBackToMaxRedispatch(t *testing.T) {
	if got := (ReviewConfig{}).EffectiveHeadMaxRetries(); got != DefaultMaxRedispatch {
		t.Fatalf("default = %d, want %d", got, DefaultMaxRedispatch)
	}
	if got := (ReviewConfig{MaxRedispatch: 5}).EffectiveHeadMaxRetries(); got != 5 {
		t.Fatalf("max_redispatch fallback = %d", got)
	}
	if got := (ReviewConfig{MaxRedispatch: 5, HeadMaxRetries: 2}).EffectiveHeadMaxRetries(); got != 2 {
		t.Fatalf("explicit = %d", got)
	}
	cfg := &Config{Review: ReviewConfig{HeadMaxRetries: -1}}
	if err := cfg.validateReviewHeads(); err == nil {
		t.Fatal("negative head_max_retries accepted")
	}
}

func keys(m map[string]AgentConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func reviewHeadsConfig(reviewerAgents []string, agents map[string]AgentConfig) *Config {
	cfg := &Config{AgentRuntimeConfig: AgentRuntimeConfig{Agents: agents}}
	cfg.Review.ReviewerAgents = reviewerAgents
	return cfg
}

func TestExpectedReviewHeadsFollowReviewerAdmission(t *testing.T) {
	base := reviewHeadsBase()
	other := AgentConfig{ID: "security-reviewer", Backend: "codex", Model: "m", Enabled: true, Role: "reviewer",
		ReviewHeads: []ReviewHead{{Backend: "codex", Model: "m", Alias: "sec"}}}
	cfg := reviewHeadsConfig([]string{"code-reviewer"}, map[string]AgentConfig{"code-reviewer": base, "security-reviewer": other})
	if err := cfg.ExpandAgentReplicas(); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(cfg.ExpectedReviewHeads(), ",")
	if got != "code-reviewer-sol,code-reviewer-sonnet" {
		t.Fatalf("expected heads = %q; heads of a base reviewer_agents does not name must not be waited on", got)
	}
	cfg.Review.ReviewerAgents = nil
	if got := cfg.ExpectedReviewHeads(); len(got) != 3 {
		t.Fatalf("without reviewer_agents every reviewer base's heads are expected, got %v", got)
	}
	cfg.Review.CombinedPerspectives = true
	if got := cfg.ExpectedReviewHeads(); got != nil {
		t.Fatalf("combined perspectives expect heads: %v", got)
	}
}

func TestReviewHeadsRejectMixedReviewerSet(t *testing.T) {
	base := reviewHeadsBase()
	plain := AgentConfig{ID: "security-reviewer", Backend: "codex", Model: "m", Enabled: true, Role: "reviewer"}
	cfg := reviewHeadsConfig([]string{"code-reviewer", "security-reviewer"}, map[string]AgentConfig{"code-reviewer": base, "security-reviewer": plain})
	if err := cfg.ExpandAgentReplicas(); err != nil {
		t.Fatal(err)
	}
	err := cfg.validateReviewHeads()
	if err == nil || !strings.Contains(err.Error(), "security-reviewer do not") {
		t.Fatalf("mixed reviewers accepted: %v", err)
	}
	// Leaving the plain lane out of reviewer_agents, or running combined
	// perspectives (heads unused), is fine.
	cfg.Review.ReviewerAgents = []string{"code-reviewer"}
	if err := cfg.validateReviewHeads(); err != nil {
		t.Fatalf("heads-only reviewer set rejected: %v", err)
	}
	cfg.Review.ReviewerAgents = []string{"code-reviewer", "security-reviewer"}
	cfg.Review.CombinedPerspectives = true
	if err := cfg.validateReviewHeads(); err != nil {
		t.Fatalf("combined perspectives rejected: %v", err)
	}
	// Without reviewer_agents the review token decides: a plain lane whose
	// name carries it is still a mixed set.
	cfg.Review.CombinedPerspectives = false
	cfg.Review.ReviewerAgents = nil
	if err := cfg.validateReviewHeads(); err == nil {
		t.Fatal("mixed reviewers by review token accepted")
	}
	delete(cfg.Agents, "security-reviewer")
	if err := cfg.validateReviewHeads(); err != nil {
		t.Fatalf("heads-only set by review token rejected: %v", err)
	}
}
