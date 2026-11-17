package hub

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
)

// #18732: a spoke that verified its Claude credential against the provider and
// was refused reports BackendAuthRevoked per agent. The hub must turn that into
// one named verdict with one named fix, ahead of the generic login-stuck leg,
// and must not wait for the pane-based login grace to elapse.
func TestRollupCountsProviderRevokedLogins(t *testing.T) {
	now := time.Date(2026, 11, 17, 14, 30, 0, 0, time.UTC)
	fresh := now.Add(-time.Minute).UTC().Format(time.RFC3339) // inside the login grace
	agents := []AgentSummary{
		{Name: "quality", State: agentStateRunning, Backend: "claude", Enabled: true, ExpectedActive: true,
			NeedsLogin: true, StartedAt: fresh, BackendAuthStatus: crux.BackendAuthRevoked},
		{Name: "scanner", State: agentStateRunning, Backend: "claude", Enabled: true, ExpectedActive: true,
			NeedsLogin: true, StartedAt: fresh, BackendAuthStatus: crux.BackendAuthRevoked},
		{Name: "guide", State: agentStateRunning, Backend: "claude", Enabled: true, ExpectedActive: true,
			StartedAt: fresh},
		{Name: "", BackendAuthStatus: crux.BackendAuthRevoked}, // blank names are skipped
	}
	r := rollupAgents(agents, hiveBlockers{}, 3, now)
	if r.LoginRevoked != 2 {
		t.Fatalf("LoginRevoked = %d, want 2 (grace must not hide a provider-verified revocation)", r.LoginRevoked)
	}
}

func TestHealthVerdictNamesRevokedClaudeLogin(t *testing.T) {
	now := time.Date(2026, 11, 17, 14, 30, 0, 0, time.UTC)
	e := RegistryEntry{Online: true, ACMMLevel: 4, DashboardURL: "https://spoke.example.com/"}

	// Inside the pane grace: no Problems yet, but the spoke already knows.
	v := hiveHealthFor(e, agentFleetRollup{Expected: 2, Running: 2, Known: 3, LoginRevoked: 2}, okApp(), 5, now)
	if v.State != HealthStateRed {
		t.Fatalf("state = %q, want red", v.State)
	}
	if v.Reason != "Claude login revoked: sign in again — 2 agent(s) at the login prompt" {
		t.Fatalf("reason = %q", v.Reason)
	}
	if v.Remediation == nil || !strings.Contains(v.Remediation.Action, "Sign in to Claude again") {
		t.Fatalf("remediation = %+v, want the sign-in hint", v.Remediation)
	}
	if v.Remediation.Link != "https://spoke.example.com" {
		t.Fatalf("link = %q, want the spoke dashboard", v.Remediation.Link)
	}

	// Past the grace, every problem is login-stuck: the revoked wording still
	// wins over the generic "stuck at login — re-login needed", because it
	// says WHY re-login is the only fix.
	v = hiveHealthFor(e, agentFleetRollup{Expected: 2, Running: 0, Known: 3, Problems: 2, LoginStuck: 2, LoginRevoked: 2}, okApp(), 5, now)
	if v.State != HealthStateRed || !strings.HasPrefix(v.Reason, "Claude login revoked") {
		t.Fatalf("state=%q reason=%q", v.State, v.Reason)
	}

	// Without the provider verdict the pre-#18732 wording is unchanged.
	v = hiveHealthFor(e, agentFleetRollup{Expected: 2, Running: 0, Known: 3, Problems: 2, LoginStuck: 2}, okApp(), 5, now)
	if v.Reason != "2 agent(s) stuck at login — re-login needed" {
		t.Fatalf("reason = %q", v.Reason)
	}
}
