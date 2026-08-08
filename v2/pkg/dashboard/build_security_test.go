package dashboard

import (
	"testing"

	"github.com/kubestellar/hive/v2/pkg/config"
)

func TestBuildSecurity_NilConfig(t *testing.T) {
	sec := buildSecurity(nil)
	if sec == nil {
		t.Fatal("buildSecurity(nil) must not return nil")
	}
	if sec.TotalAgents != 0 || sec.SandboxedAgents != 0 || sec.ReviewCapableAgents != 0 {
		t.Fatalf("nil config should yield zero counters: %+v", sec)
	}
}

func TestBuildSecurity_EmptyConfig(t *testing.T) {
	cfg := &config.Config{}
	sec := buildSecurity(cfg)
	// nil *bool Enabled defaults ON per F11 (fail-safe: scanning on unless explicit opt-out)
	if !sec.IoscanEnabled {
		t.Fatal("ioscan should default ON for zero-value config (nil *bool = enabled)")
	}
	if sec.IoscanFailMode != "open" {
		t.Fatalf("default fail mode should be open, got %q", sec.IoscanFailMode)
	}
	if sec.TotalAgents != 0 {
		t.Fatalf("empty agents map should give TotalAgents=0, got %d", sec.TotalAgents)
	}
}

func TestBuildSecurity_IoscanExplicitlyDisabled(t *testing.T) {
	disabled := false
	cfg := &config.Config{
		Ioscan: config.IoscanConfig{Enabled: &disabled},
	}
	sec := buildSecurity(cfg)
	if sec.IoscanEnabled {
		t.Fatal("ioscan should be disabled when explicitly set to false")
	}
}

func TestBuildSecurity_FullyConfigured(t *testing.T) {
	enabled := true
	cfg := &config.Config{
		Intent: config.IntentConfig{Enforce: true},
		Ioscan: config.IoscanConfig{Enabled: &enabled, FailMode: "closed", Canaries: true},
		Review: config.ReviewConfig{
			RequireApproval: true,
			FanOut:          true,
			ReviewerAgents:  []string{"reviewer"},
		},
		Retro:        config.RetroConfig{Enabled: true},
		OTel:         config.OTelConfig{Enabled: true},
		AgentSandbox: config.AgentSandboxConfig{Enabled: true},
		Agents: map[string]config.AgentConfig{
			"reviewer": {Enabled: true, Role: "reviewer", Sandbox: &config.AgentSandboxOverride{Enabled: &enabled}},
			"scanner":  {Enabled: true, Role: "scanner", Sandbox: &config.AgentSandboxOverride{Enabled: &enabled}},
			"quality":  {Enabled: true, Role: "quality"},
		},
	}
	sec := buildSecurity(cfg)

	if !sec.IntentEnforced {
		t.Fatal("intent should be enforced")
	}
	if !sec.IoscanEnabled || sec.IoscanFailMode != "closed" || !sec.IoscanCanaries {
		t.Fatalf("ioscan: enabled=%v failMode=%s canaries=%v", sec.IoscanEnabled, sec.IoscanFailMode, sec.IoscanCanaries)
	}
	if !sec.ReviewRequireApproval || !sec.ReviewFanOut {
		t.Fatal("review fields not propagated")
	}
	if !sec.RetroEnabled || !sec.OTelEnabled || !sec.SandboxEnabled {
		t.Fatal("retro/otel/sandbox not propagated")
	}
	if sec.TotalAgents != 3 {
		t.Fatalf("TotalAgents = %d, want 3", sec.TotalAgents)
	}
	if sec.SandboxedAgents != 2 {
		t.Fatalf("SandboxedAgents = %d, want 2 (reviewer+scanner)", sec.SandboxedAgents)
	}
	if sec.ReviewCapableAgents != 1 {
		t.Fatalf("ReviewCapableAgents = %d, want 1 (only reviewer in allow-list)", sec.ReviewCapableAgents)
	}
}

func TestBuildSecurity_SandboxRequiresGlobalGate(t *testing.T) {
	enabled := true
	cfg := &config.Config{
		AgentSandbox: config.AgentSandboxConfig{Enabled: false},
		Agents: map[string]config.AgentConfig{
			"scanner": {Enabled: true, Sandbox: &config.AgentSandboxOverride{Enabled: &enabled}},
		},
	}
	sec := buildSecurity(cfg)
	if sec.SandboxedAgents != 0 {
		t.Fatalf("global sandbox disabled: SandboxedAgents should be 0, got %d", sec.SandboxedAgents)
	}
}

func TestDashboardAgentReviewCapable_ExplicitAllowList(t *testing.T) {
	allowed := []string{"reviewer", "code-review"}
	cases := []struct {
		name string
		ac   config.AgentConfig
		want bool
	}{
		{name: "reviewer", ac: config.AgentConfig{Enabled: true}, want: true},
		{name: "code-review", ac: config.AgentConfig{Enabled: true}, want: true},
		{name: "scanner", ac: config.AgentConfig{Enabled: true}, want: false},
		{name: "reviewer", ac: config.AgentConfig{Enabled: false}, want: false},
		{name: "reviewer", ac: config.AgentConfig{Enabled: true, Paused: true}, want: false},
		{name: "reviewer", ac: config.AgentConfig{Enabled: true, OnDemand: true}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name+"_"+boolStr(tc.want), func(t *testing.T) {
			got := dashboardAgentReviewCapable(tc.name, tc.ac, allowed)
			if got != tc.want {
				t.Fatalf("dashboardAgentReviewCapable(%q, %+v, %v) = %v, want %v",
					tc.name, tc.ac, allowed, got, tc.want)
			}
		})
	}
}

func TestDashboardAgentReviewCapable_KeywordFallback(t *testing.T) {
	cases := []struct {
		desc string
		name string
		ac   config.AgentConfig
		want bool
	}{
		{
			desc: "role contains review",
			name: "myagent",
			ac:   config.AgentConfig{Enabled: true, Role: "code-reviewer"},
			want: true,
		},
		{
			desc: "alias contains review",
			name: "bot",
			ac:   config.AgentConfig{Enabled: true, Role: "scanner", Aliases: []string{"pr-review"}},
			want: true,
		},
		{
			desc: "lane keyword contains review",
			name: "bot",
			ac:   config.AgentConfig{Enabled: true, Role: "scanner", LaneKeywords: []string{"review-lane"}},
			want: true,
		},
		{
			desc: "detect keyword contains review",
			name: "bot",
			ac:   config.AgentConfig{Enabled: true, Role: "scanner", DetectKeywords: []string{"needs-review"}},
			want: true,
		},
		{
			desc: "name contains review",
			name: "code-review-bot",
			ac:   config.AgentConfig{Enabled: true, Role: "scanner"},
			want: true,
		},
		{
			desc: "case insensitive",
			name: "MyReviewer",
			ac:   config.AgentConfig{Enabled: true, Role: "scanner"},
			want: true,
		},
		{
			desc: "no review keyword anywhere",
			name: "scanner",
			ac:   config.AgentConfig{Enabled: true, Role: "scanner"},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got := dashboardAgentReviewCapable(tc.name, tc.ac, nil)
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
