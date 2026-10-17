package agent

import (
	"strings"
	"testing"
	"time"
)

func findProvider(t *testing.T, got []ProviderStatus, provider string) ProviderStatus {
	t.Helper()
	for _, p := range got {
		if p.Provider == provider {
			return p
		}
	}
	t.Fatalf("provider %q missing from %+v", provider, got)
	return ProviderStatus{}
}

func TestRollUpProviderStatusSeparatesAuthFromAvailability(t *testing.T) {
	since := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	got := RollUpProviderStatus([]ProviderAgentEvidence{
		{Agent: "scanner", Provider: "copilot", Enabled: true, AuthStatus: BackendAuthUnlicensed, AuthSince: since},
		{Agent: "reviewer", Provider: "copilot", Enabled: true},
		{Agent: "outreach", Provider: "gemini", Enabled: true, AuthStatus: BackendAuthUnreachable, AuthSince: since},
	})
	if len(got) != 2 || got[0].Provider != "copilot" || got[1].Provider != "gemini" {
		t.Fatalf("want sorted [copilot gemini], got %+v", got)
	}

	copilot := got[0]
	if copilot.AuthState != ProviderAuthNeedsLogin {
		t.Errorf("unlicensed copilot auth_state = %q, want %q", copilot.AuthState, ProviderAuthNeedsLogin)
	}
	if copilot.AvailabilityState != ProviderAvailable {
		t.Errorf("copilot availability_state = %q, want %q (the healthy agent proves the provider serves)", copilot.AvailabilityState, ProviderAvailable)
	}
	if !strings.Contains(copilot.NextAction, "licence") {
		t.Errorf("unlicensed next_action must name the seat, got %q", copilot.NextAction)
	}
	if copilot.Since != "2026-10-01T12:00:00Z" {
		t.Errorf("since = %q", copilot.Since)
	}
	if len(copilot.AffectedAgents) != 1 || copilot.AffectedAgents[0] != "scanner" {
		t.Errorf("affected_agents = %v, want [scanner]", copilot.AffectedAgents)
	}
	if copilot.Evidence != "1 of 2 enabled agents: unlicensed×1" {
		t.Errorf("evidence = %q", copilot.Evidence)
	}

	// Provider unavailable is NOT "not authenticated".
	gemini := got[1]
	if gemini.AvailabilityState != ProviderUnavailable || gemini.AuthState != ProviderAuthUnknown {
		t.Errorf("unreachable gemini = auth %q / availability %q, want unknown / unavailable", gemini.AuthState, gemini.AvailabilityState)
	}
	if strings.Contains(strings.ToLower(gemini.NextAction), "log in") {
		t.Errorf("unreachable next_action must not prescribe a login: %q", gemini.NextAction)
	}
}

func TestRollUpProviderStatusTokenExpiredUsesRefreshable(t *testing.T) {
	for _, tc := range []struct {
		name        string
		refreshable *bool
		want        string
	}{
		{"refresh grant still valid", boolPtr(true), ProviderAuthRefreshable},
		{"refresh grant gone", boolPtr(false), ProviderAuthNeedsLogin},
		{"refreshability unknown", nil, ProviderAuthNeedsLogin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := RollUpProviderStatus([]ProviderAgentEvidence{
				{Agent: "scanner", Provider: "claude", Enabled: true, AuthStatus: BackendAuthTokenExpired, Refreshable: tc.refreshable},
			})
			p := findProvider(t, got, "claude")
			if p.AuthState != tc.want {
				t.Fatalf("auth_state = %q, want %q", p.AuthState, tc.want)
			}
			if p.AvailabilityState != ProviderAvailUnknown {
				t.Errorf("availability_state = %q, want unknown: an auth failure is no availability evidence", p.AvailabilityState)
			}
			if tc.want == ProviderAuthRefreshable && !strings.Contains(p.NextAction, "Restart") {
				t.Errorf("refreshable next_action = %q, want a restart", p.NextAction)
			}
		})
	}
}

func TestRollUpProviderStatusQuotaAndForbidden(t *testing.T) {
	got := RollUpProviderStatus([]ProviderAgentEvidence{
		{Agent: "a", Provider: "codex", Enabled: true, AuthStatus: BackendAuthQuota},
		{Agent: "b", Provider: "copilot", Enabled: true, AuthStatus: BackendAuthForbidden},
		{Agent: "c", Provider: "copilot", Enabled: true},
	})
	codex := findProvider(t, got, "codex")
	if codex.AuthState != ProviderAuthUsable || codex.AvailabilityState != ProviderQuotaExhausted {
		t.Errorf("quota = %q / %q, want usable / quota-exhausted", codex.AuthState, codex.AvailabilityState)
	}
	copilot := findProvider(t, got, "copilot")
	if copilot.AuthState != ProviderAuthUnknown {
		t.Errorf("forbidden auth_state = %q, want unknown (never convicted as a logout)", copilot.AuthState)
	}
	if !strings.Contains(copilot.NextAction, "no attributable cause") {
		t.Errorf("forbidden next_action = %q", copilot.NextAction)
	}
}

func TestRollUpProviderStatusSkipsDisabledAndHealthy(t *testing.T) {
	if got := RollUpProviderStatus([]ProviderAgentEvidence{
		{Agent: "idle", Provider: "kiro", Enabled: false, AuthStatus: BackendAuthTokenExpired},
		{Agent: "legacy", Enabled: true},
	}); got != nil {
		t.Fatalf("disabled agents and agents without a provider must not roll up, got %+v", got)
	}
	got := RollUpProviderStatus([]ProviderAgentEvidence{{Agent: "x", Provider: "Claude", Enabled: true}})
	p := findProvider(t, got, "claude")
	if p.AuthState != ProviderAuthUsable || p.AvailabilityState != ProviderAvailable || p.NextAction != "" || p.AffectedAgents != nil {
		t.Fatalf("healthy provider = %+v", p)
	}
}

func TestRollUpProviderStatusLoginPrompt(t *testing.T) {
	got := RollUpProviderStatus([]ProviderAgentEvidence{
		{Agent: "a", Provider: "claude", Enabled: true, NeedsLogin: true, Refreshable: boolPtr(true)},
		{Agent: "b", Provider: "copilot", Enabled: true, NeedsLogin: true},
	})
	if p := findProvider(t, got, "claude"); p.AuthState != ProviderAuthRefreshable {
		t.Errorf("claude login prompt with usable grant = %q, want refreshable", p.AuthState)
	}
	if p := findProvider(t, got, "copilot"); p.AuthState != ProviderAuthNeedsLogin {
		t.Errorf("copilot login prompt = %q, want needs-login", p.AuthState)
	}
}
