package spoke

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
)

// The live heartbeat must carry each agent's backend-auth canary: the hub's
// auth_health and the providers roll-up both read it from agents[].
func TestApplyBackendAuthRidesHeartbeatAgentSummary(t *testing.T) {
	since := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	proc := &agent.AgentProcess{BackendAuth: agent.BackendAuthState{
		Status: agent.BackendAuthTokenExpired, Since: since, LastError: "401 token expired",
	}}
	act := AgentActivity{Backend: "claude", Enabled: true}
	ApplyBackendAuth(&act, nil, "scanner", proc)
	refreshable := true
	act.BackendAuthRefreshable = &refreshable

	as := NewAgentSummary("scanner", "running", "", act)
	if as.BackendAuthStatus != agent.BackendAuthTokenExpired || as.BackendAuthSince != "2026-10-01T08:00:00Z" || as.BackendAuthLastError != "401 token expired" {
		t.Fatalf("backend auth not carried: %+v", as)
	}
	raw, err := json.Marshal(as)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"backendAuthStatus":"token-expired"`, `"backendAuthSince":`, `"backendAuthRefreshable":true`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("heartbeat agent JSON missing %s: %s", key, raw)
		}
	}
}

func TestApplyBackendAuthOmitsOKAndUnknownRefreshable(t *testing.T) {
	act := AgentActivity{}
	ApplyBackendAuth(&act, nil, "scanner", &agent.AgentProcess{BackendAuth: agent.BackendAuthState{Status: agent.BackendAuthOK}})
	if act.BackendAuthStatus != "" || !act.BackendAuthSince.IsZero() || act.BackendAuthRefreshable != nil {
		t.Fatalf("ok agent must report nothing, got %+v", act)
	}
	raw, err := json.Marshal(NewAgentSummary("scanner", "running", "", act))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "backendAuth") {
		t.Fatalf("ok agent leaked backend-auth keys: %s", raw)
	}
}

func TestProviderStatusForHeartbeat(t *testing.T) {
	refreshable := true
	got := ProviderStatusForHeartbeat([]AgentSummary{
		{Name: "scanner", Backend: "claude", Enabled: true, BackendAuthStatus: agent.BackendAuthTokenExpired, BackendAuthSince: "2026-10-01T08:00:00Z", BackendAuthRefreshable: &refreshable},
		{Name: "reviewer", Backend: "copilot", Enabled: true, BackendAuthStatus: agent.BackendAuthUnreachable},
		{Name: "idle", Backend: "codex"},
	})
	if len(got) != 2 {
		t.Fatalf("want claude + copilot (disabled codex skipped), got %+v", got)
	}
	if got[0].Provider != "claude" || got[0].AuthState != agent.ProviderAuthRefreshable || got[0].Since != "2026-10-01T08:00:00Z" {
		t.Errorf("claude = %+v", got[0])
	}
	if got[1].Provider != "copilot" || got[1].AvailabilityState != agent.ProviderUnavailable {
		t.Errorf("copilot = %+v", got[1])
	}
}
