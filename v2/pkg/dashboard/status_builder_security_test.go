package dashboard

import (
	"testing"

	"github.com/kubestellar/hive/v2/pkg/config"
)

func boolPtr(b bool) *bool { return &b }

func TestBuildSecurity_NilConfig(t *testing.T) {
	sec := buildSecurity(nil)
	if sec == nil {
		t.Fatal("buildSecurity(nil) returned nil, want empty struct")
	}
	if sec.TotalAgents != 0 {
		t.Errorf("TotalAgents = %d, want 0", sec.TotalAgents)
	}
	if sec.SandboxedAgents != 0 {
		t.Errorf("SandboxedAgents = %d, want 0", sec.SandboxedAgents)
	}
	if sec.ReviewCapableAgents != 0 {
		t.Errorf("ReviewCapableAgents = %d, want 0", sec.ReviewCapableAgents)
	}
}

func TestBuildSecurity_EmptyAgentsMap(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{},
	}
	sec := buildSecurity(cfg)
	if sec.TotalAgents != 0 {
		t.Errorf("TotalAgents = %d, want 0", sec.TotalAgents)
	}
	if sec.SandboxedAgents != 0 {
		t.Errorf("SandboxedAgents = %d, want 0", sec.SandboxedAgents)
	}
	if sec.ReviewCapableAgents != 0 {
		t.Errorf("ReviewCapableAgents = %d, want 0", sec.ReviewCapableAgents)
	}
}

func TestBuildSecurity_MixedAgents(t *testing.T) {
	cfg := &config.Config{
		AgentSandbox: config.AgentSandboxConfig{Enabled: true},
		Ioscan:       config.IoscanConfig{FailMode: "closed"},
		Intent:       config.IntentConfig{Enforce: true},
		Retro:        config.RetroConfig{Enabled: true},
		OTel:         config.OTelConfig{Enabled: true},
		Review: config.ReviewConfig{
			RequireApproval: true,
			FanOut:          true,
		},
		Agents: map[string]config.AgentConfig{
			"scanner": {
				Enabled: true,
				Role:    "code scanner",
				Sandbox: &config.AgentSandboxOverride{Enabled: boolPtr(true)},
			},
			"reviewer": {
				Enabled:      true,
				Role:         "code reviewer",
				LaneKeywords: []string{"review", "pr"},
				Sandbox:      &config.AgentSandboxOverride{Enabled: boolPtr(false)},
			},
			"planner": {
				Enabled: true,
				Role:    "planning",
				Sandbox: &config.AgentSandboxOverride{Enabled: boolPtr(true)},
			},
		},
	}

	sec := buildSecurity(cfg)

	if sec.TotalAgents != 3 {
		t.Errorf("TotalAgents = %d, want 3", sec.TotalAgents)
	}
	if sec.SandboxedAgents != 2 {
		t.Errorf("SandboxedAgents = %d, want 2 (scanner + planner)", sec.SandboxedAgents)
	}
	if sec.ReviewCapableAgents != 1 {
		t.Errorf("ReviewCapableAgents = %d, want 1 (reviewer)", sec.ReviewCapableAgents)
	}
	if !sec.IntentEnforced {
		t.Error("IntentEnforced should be true")
	}
	if !sec.IoscanEnabled {
		t.Error("IoscanEnabled should be true (nil Enabled ptr defaults on)")
	}
	if sec.IoscanFailMode != "closed" {
		t.Errorf("IoscanFailMode = %q, want \"closed\"", sec.IoscanFailMode)
	}
	if !sec.ReviewRequireApproval {
		t.Error("ReviewRequireApproval should be true")
	}
	if !sec.ReviewFanOut {
		t.Error("ReviewFanOut should be true")
	}
	if !sec.RetroEnabled {
		t.Error("RetroEnabled should be true")
	}
	if !sec.OTelEnabled {
		t.Error("OTelEnabled should be true")
	}
	if !sec.SandboxEnabled {
		t.Error("SandboxEnabled should be true")
	}
}

func TestBuildSecurity_IoscanDefaults(t *testing.T) {
	// nil Enabled pointer means ioscan is ON by default (fail-safe)
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{},
	}
	sec := buildSecurity(cfg)
	if !sec.IoscanEnabled {
		t.Error("IoscanEnabled should default true when Ioscan.Enabled is nil")
	}
	if sec.IoscanFailMode != "open" {
		t.Errorf("IoscanFailMode = %q, want \"open\" (default)", sec.IoscanFailMode)
	}

	// Explicit false disables ioscan
	f := false
	cfg.Ioscan.Enabled = &f
	sec = buildSecurity(cfg)
	if sec.IoscanEnabled {
		t.Error("IoscanEnabled should be false when explicitly disabled")
	}
}

func TestDashboardAgentReviewCapable_DisabledStates(t *testing.T) {
	tests := []struct {
		name string
		a    config.AgentConfig
	}{
		{"disabled", config.AgentConfig{Enabled: false, Role: "reviewer"}},
		{"paused", config.AgentConfig{Enabled: true, Paused: true, Role: "reviewer"}},
		{"on-demand", config.AgentConfig{Enabled: true, OnDemand: true, Role: "reviewer"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if dashboardAgentReviewCapable("test", tt.a, nil) {
				t.Errorf("agent with %s state should not be review-capable", tt.name)
			}
		})
	}
}

func TestDashboardAgentReviewCapable_ExplicitAllowList(t *testing.T) {
	a := config.AgentConfig{Enabled: true, Role: "code reviewer"}

	// Agent in allow list → capable
	if !dashboardAgentReviewCapable("reviewer", a, []string{"reviewer", "auditor"}) {
		t.Error("agent in allow list should be review-capable")
	}

	// Agent NOT in allow list → not capable, even with keyword match
	if dashboardAgentReviewCapable("reviewer", a, []string{"auditor", "inspector"}) {
		t.Error("agent not in allow list should not be review-capable")
	}

	// Whitespace trimming in allow list
	if !dashboardAgentReviewCapable("reviewer", a, []string{" reviewer ", "auditor"}) {
		t.Error("allow list entry with whitespace should still match after trimming")
	}
}

func TestDashboardAgentReviewCapable_KeywordFallback(t *testing.T) {
	tests := []struct {
		name   string
		a      config.AgentConfig
		expect bool
	}{
		{
			name:   "name contains review",
			a:      config.AgentConfig{Enabled: true},
			expect: true,
		},
		{
			name:   "role contains review",
			a:      config.AgentConfig{Enabled: true, Role: "PR Reviewer"},
			expect: true,
		},
		{
			name:   "alias contains review",
			a:      config.AgentConfig{Enabled: true, Aliases: []string{"code-review-bot"}},
			expect: true,
		},
		{
			name:   "lane keyword contains review",
			a:      config.AgentConfig{Enabled: true, LaneKeywords: []string{"review", "audit"}},
			expect: true,
		},
		{
			name:   "detect keyword contains review",
			a:      config.AgentConfig{Enabled: true, DetectKeywords: []string{"needs-review"}},
			expect: true,
		},
		{
			name:   "no review keywords anywhere",
			a:      config.AgentConfig{Enabled: true, Role: "planner", Aliases: []string{"bot"}},
			expect: false,
		},
		{
			name:   "case insensitive match",
			a:      config.AgentConfig{Enabled: true, Role: "Code REVIEW specialist"},
			expect: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// For "name contains review" case, pass a name with "review" in it
			agentName := "scanner"
			if tt.name == "name contains review" {
				agentName = "code-review-agent"
			}
			got := dashboardAgentReviewCapable(agentName, tt.a, nil)
			if got != tt.expect {
				t.Errorf("dashboardAgentReviewCapable(%q, ...) = %v, want %v", agentName, got, tt.expect)
			}
		})
	}
}

func TestBuildSecurity_SandboxGlobalDisabled(t *testing.T) {
	// Global sandbox disabled → no agents are sandboxed even if per-agent is true
	cfg := &config.Config{
		AgentSandbox: config.AgentSandboxConfig{Enabled: false},
		Agents: map[string]config.AgentConfig{
			"scanner": {
				Enabled: true,
				Sandbox: &config.AgentSandboxOverride{Enabled: boolPtr(true)},
			},
		},
	}
	sec := buildSecurity(cfg)
	if sec.SandboxedAgents != 0 {
		t.Errorf("SandboxedAgents = %d, want 0 (global disabled)", sec.SandboxedAgents)
	}
	if sec.SandboxEnabled {
		t.Error("SandboxEnabled should be false when global is disabled")
	}
}

func TestBuildSecurity_ReviewCapableWithAllowList(t *testing.T) {
	cfg := &config.Config{
		Review: config.ReviewConfig{
			ReviewerAgents: []string{"auditor"},
		},
		Agents: map[string]config.AgentConfig{
			// Has "review" in role but NOT in allow list → not capable
			"reviewer": {Enabled: true, Role: "code reviewer"},
			// In allow list → capable
			"auditor": {Enabled: true, Role: "auditing"},
			// Disabled → never capable
			"disabled-reviewer": {Enabled: false, Role: "reviewer"},
		},
	}
	sec := buildSecurity(cfg)
	if sec.ReviewCapableAgents != 1 {
		t.Errorf("ReviewCapableAgents = %d, want 1 (only auditor)", sec.ReviewCapableAgents)
	}
}
