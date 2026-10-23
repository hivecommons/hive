package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/crux"
)

func TestBuildProviderStatusRollsUpEnabledAgents(t *testing.T) {
	refreshable := true
	cfg := &config.Config{AgentRuntimeConfig: config.AgentRuntimeConfig{
		Agents: map[string]config.AgentConfig{
			"scanner":  {Enabled: true},
			"reviewer": {Enabled: true},
			"idle":     {Enabled: false},
		},
	}}
	agents := []FrontendAgent{
		{Name: "scanner", GovBackend: "claude", BackendAuthStatus: crux.BackendAuthTokenExpired, BackendAuthSince: "2026-10-01T08:00:00Z", BackendAuthRefreshable: &refreshable},
		{Name: "reviewer-2", ReplicaBase: "reviewer", GovBackend: "copilot", BackendAuthStatus: crux.BackendAuthQuota},
		{Name: "idle", GovBackend: "codex", BackendAuthStatus: crux.BackendAuthUnlicensed},
	}
	got := buildProviderStatus(agents, cfg)
	if len(got) != 2 {
		t.Fatalf("want claude + copilot (disabled codex skipped), got %+v", got)
	}
	if got[0].Provider != "claude" || got[0].AuthState != crux.ProviderAuthRefreshable || got[0].Since != "2026-10-01T08:00:00Z" {
		t.Errorf("claude = %+v", got[0])
	}
	if got[1].Provider != "copilot" || got[1].AuthState != crux.ProviderAuthUsable || got[1].AvailabilityState != crux.ProviderQuotaExhausted {
		t.Errorf("replica copilot = %+v", got[1])
	}

	off := false
	cfg.Providers.Status.Enabled = &off
	if got := buildProviderStatus(agents, cfg); got != nil {
		t.Fatalf("disabled provider status must omit the block, got %+v", got)
	}
}
