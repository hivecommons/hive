package dashboard

import (
	"testing"

	"github.com/kubestellar/hive/v2/pkg/config"
)

func TestBuildSecurity_NilConfig(t *testing.T) {
	sec := buildSecurity(nil)
	if sec == nil {
		t.Fatal("buildSecurity(nil) returned nil, want non-nil zero struct")
	}
	if sec.TotalAgents != 0 || sec.SandboxedAgents != 0 || sec.ReviewCapableAgents != 0 {
		t.Fatalf("nil config should yield zero counts, got total=%d sandboxed=%d review=%d",
			sec.TotalAgents, sec.SandboxedAgents, sec.ReviewCapableAgents)
	}
	if sec.IoscanFailMode != "" {
		t.Fatalf("nil config should yield empty IoscanFailMode, got %q", sec.IoscanFailMode)
	}
}

func TestBuildSecurity_EmptyAgents(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{},
	}
	sec := buildSecurity(cfg)
	if sec.TotalAgents != 0 {
		t.Fatalf("empty agents: TotalAgents = %d, want 0", sec.TotalAgents)
	}
	if sec.SandboxedAgents != 0 {
		t.Fatalf("empty agents: SandboxedAgents = %d, want 0", sec.SandboxedAgents)
	}
	if sec.ReviewCapableAgents != 0 {
		t.Fatalf("empty agents: ReviewCapableAgents = %d, want 0", sec.ReviewCapableAgents)
	}
	// Default ioscan fail mode is "open"
	if sec.IoscanFailMode != "open" {
		t.Fatalf("empty config: IoscanFailMode = %q, want \"open\"", sec.IoscanFailMode)
	}
}

func TestBuildSecurity_MixedAgents(t *testing.T) {
	sandboxTrue := true
	cfg := &config.Config{
		AgentSandbox: config.AgentSandboxConfig{Enabled: true},
		Ioscan:       config.IoscanConfig{FailMode: "closed", Canaries: true},
		Intent:       config.IntentConfig{Enforce: true},
		Review: config.ReviewConfig{
			RequireApproval: true,
			FanOut:          true,
		},
		Retro: config.RetroConfig{Enabled: true},
		OTel:  config.OTelConfig{Enabled: true},
		Agents: map[string]config.AgentConfig{
			"reviewer": {
				Enabled: true,
				Role:    "code reviewer",
				Sandbox: &config.AgentSandboxOverride{Enabled: &sandboxTrue},
			},
			"scanner": {
				Enabled:      true,
				Role:         "scanner",
				LaneKeywords: []string{"security", "review-gate"},
				Sandbox:      &config.AgentSandboxOverride{Enabled: &sandboxTrue},
			},
			"builder": {
				Enabled: true,
				Role:    "builder",
			},
			"paused-reviewer": {
				Enabled: true,
				Paused:  true,
				Role:    "code review specialist",
			},
		},
	}

	sec := buildSecurity(cfg)

	if sec.TotalAgents != 4 {
		t.Fatalf("TotalAgents = %d, want 4", sec.TotalAgents)
	}
	if sec.SandboxedAgents != 2 {
		t.Fatalf("SandboxedAgents = %d, want 2 (reviewer + scanner)", sec.SandboxedAgents)
	}
	// reviewer: role contains "review" → capable
	// scanner: LaneKeywords contains "review-gate" → capable
	// builder: no review keyword → not capable
	// paused-reviewer: paused → not capable despite role
	if sec.ReviewCapableAgents != 2 {
		t.Fatalf("ReviewCapableAgents = %d, want 2 (reviewer + scanner)", sec.ReviewCapableAgents)
	}
	if !sec.IntentEnforced {
		t.Fatal("IntentEnforced should be true")
	}
	if !sec.IoscanEnabled {
		t.Fatal("IoscanEnabled should be true (nil Enabled defaults on)")
	}
	if sec.IoscanFailMode != "closed" {
		t.Fatalf("IoscanFailMode = %q, want \"closed\"", sec.IoscanFailMode)
	}
	if !sec.IoscanCanaries {
		t.Fatal("IoscanCanaries should be true")
	}
	if !sec.ReviewRequireApproval {
		t.Fatal("ReviewRequireApproval should be true")
	}
	if !sec.ReviewFanOut {
		t.Fatal("ReviewFanOut should be true")
	}
	if !sec.RetroEnabled {
		t.Fatal("RetroEnabled should be true")
	}
	if !sec.OTelEnabled {
		t.Fatal("OTelEnabled should be true")
	}
	if !sec.SandboxEnabled {
		t.Fatal("SandboxEnabled should be true")
	}
}

func TestDashboardAgentReviewCapable(t *testing.T) {
	tests := []struct {
		name    string
		agent   string
		cfg     config.AgentConfig
		allowed []string
		want    bool
	}{
		{
			name:  "disabled agent never capable",
			agent: "reviewer",
			cfg:   config.AgentConfig{Enabled: false, Role: "reviewer"},
			want:  false,
		},
		{
			name:  "paused agent never capable",
			agent: "reviewer",
			cfg:   config.AgentConfig{Enabled: true, Paused: true, Role: "reviewer"},
			want:  false,
		},
		{
			name:  "on-demand agent never capable",
			agent: "reviewer",
			cfg:   config.AgentConfig{Enabled: true, OnDemand: true, Role: "reviewer"},
			want:  false,
		},
		{
			name:    "explicit allow-list includes agent",
			agent:   "scanner",
			cfg:     config.AgentConfig{Enabled: true, Role: "scanner"},
			allowed: []string{"scanner", "reviewer"},
			want:    true,
		},
		{
			name:    "explicit allow-list excludes agent",
			agent:   "builder",
			cfg:     config.AgentConfig{Enabled: true, Role: "builder"},
			allowed: []string{"scanner", "reviewer"},
			want:    false,
		},
		{
			name:    "allow-list with whitespace trimmed",
			agent:   "reviewer",
			cfg:     config.AgentConfig{Enabled: true, Role: "ci"},
			allowed: []string{"  reviewer  "},
			want:    true,
		},
		{
			name:  "keyword in Role",
			agent: "code-review",
			cfg:   config.AgentConfig{Enabled: true, Role: "code reviewer"},
			want:  true,
		},
		{
			name:  "keyword in agent name",
			agent: "review-bot",
			cfg:   config.AgentConfig{Enabled: true, Role: "helper"},
			want:  true,
		},
		{
			name:  "keyword in Aliases",
			agent: "scanner",
			cfg:   config.AgentConfig{Enabled: true, Role: "scanner", Aliases: []string{"pr-reviewer"}},
			want:  true,
		},
		{
			name:  "keyword in LaneKeywords",
			agent: "gate",
			cfg:   config.AgentConfig{Enabled: true, Role: "gate", LaneKeywords: []string{"review-lane"}},
			want:  true,
		},
		{
			name:  "keyword in DetectKeywords",
			agent: "detect",
			cfg:   config.AgentConfig{Enabled: true, Role: "detect", DetectKeywords: []string{"needs-review"}},
			want:  true,
		},
		{
			name:  "case insensitive keyword match",
			agent: "bot",
			cfg:   config.AgentConfig{Enabled: true, Role: "CODE REVIEW SPECIALIST"},
			want:  true,
		},
		{
			name:  "no keyword match at all",
			agent: "builder",
			cfg:   config.AgentConfig{Enabled: true, Role: "build runner", Aliases: []string{"ci"}, LaneKeywords: []string{"build"}},
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dashboardAgentReviewCapable(tt.agent, tt.cfg, tt.allowed)
			if got != tt.want {
				t.Errorf("dashboardAgentReviewCapable(%q, ...) = %v, want %v", tt.agent, got, tt.want)
			}
		})
	}
}
