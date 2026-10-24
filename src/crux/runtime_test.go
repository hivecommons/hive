package crux

import (
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/agent"
	"github.com/hivecommons/hive/crux/config"
)

func TestFromManager_NilManagerIsNilRuntime(t *testing.T) {
	var m *agent.Manager
	if rt := FromManager(m); rt != nil {
		t.Fatalf("FromManager(nil) = %#v, want a nil interface so `rt != nil` guards keep working", rt)
	}
}

func TestFromManager_WrapsManagerUnchanged(t *testing.T) {
	m := agent.NewManager(map[string]config.AgentConfig{}, slog.Default(), agent.ProjectContext{})
	rt := FromManager(m)
	if rt == nil {
		t.Fatal("FromManager(non-nil) returned nil")
	}
	if got, ok := rt.(*agent.Manager); !ok || got != m {
		t.Fatalf("FromManager returned %T, want the same *agent.Manager", rt)
	}
	if _, err := rt.GetStatus("no-such-agent"); err == nil {
		t.Fatal("GetStatus on an unknown agent should fail through the Runtime surface as it does on the manager")
	}
}

// TestValueAliasesTrackAgent pins that the façade's constants are the ones
// the manager writes (both sides alias pkg/crux/types), so a consumer
// comparing crux.StateRunning against a Process.State can never drift.
func TestValueAliasesTrackAgent(t *testing.T) {
	if StateRunning != agent.StateRunning || StatePaused != agent.StatePaused || StateStopped != agent.StateStopped {
		t.Fatal("process state constants diverged from pkg/agent")
	}
	if KickPhasePending != agent.KickPhasePending || KickPhaseFailed != agent.KickPhaseFailed {
		t.Fatal("kick phase constants diverged from pkg/agent")
	}
	if StartFailureCredentialRejected != agent.StartFailureCredentialRejected {
		t.Fatal("start failure constants diverged from pkg/agent")
	}
	var p Process
	p.State = StateRunning
	if p.State != agent.StateRunning {
		t.Fatal("Process alias does not share agent.AgentProcess's State type")
	}
	p.KickOutcome = KickOutcome{Kind: KickOutcomeEnded, KickAt: time.Unix(1, 0)}
	kickAt := time.Unix(1, 0)
	if !p.KickOutcome.Settled(&kickAt) || p.KickOutcome.Kind != agent.KickOutcomeEnded {
		t.Fatal("KickOutcome alias does not share agent.AgentProcess's KickOutcome type")
	}
	p.RestartEvents = []RestartEvent{{Reason: "x"}}
	p.TurnLoss = TurnLoss{Recent: []TurnInterruption{{Reason: "restart"}}}
	p.KickHistory = []KickRecord{{Agent: "a"}}
	p.SubAgentModels = []SubAgentModel{{Model: "m"}}
	p.StartFailureClass = string(StartFailureNoOutput)
	if p.StartFailureClass != string(agent.StartFailureNoOutput) || !(KickDispatch{Phase: KickPhasePending}).Pending() {
		t.Fatal("start-failure or kick-dispatch aliases diverged from pkg/agent")
	}
}

// TestProviderStatusAliasesTrackTypes pins that the provider-status surface
// re-exported here is the same one pkg/agent aliases, so a hub comparing
// crux.BackendAuthOK against a Process.BackendAuth.Status the runtime wrote
// can never drift.
func TestProviderStatusAliasesTrackTypes(t *testing.T) {
	if BackendAuthOK != agent.BackendAuthOK || BackendAuthForbidden != agent.BackendAuthForbidden {
		t.Fatal("backend auth constants diverged from pkg/agent")
	}
	if ProviderAuthNeedsLogin != agent.ProviderAuthNeedsLogin || ProviderAvailUnknown != agent.ProviderAvailUnknown {
		t.Fatal("provider state constants diverged from pkg/agent")
	}
	var p Process
	p.BackendAuth = BackendAuthState{Status: BackendAuthQuota}
	if p.BackendAuth.Status != agent.BackendAuthQuota {
		t.Fatal("BackendAuthState alias does not share agent.AgentProcess's BackendAuth type")
	}
	got := RollUpProviderStatus([]ProviderAgentEvidence{{Agent: "a", Provider: "copilot", Enabled: true, AuthStatus: BackendAuthUnlicensed}})
	want := agent.RollUpProviderStatus([]agent.ProviderAgentEvidence{{Agent: "a", Provider: "copilot", Enabled: true, AuthStatus: BackendAuthUnlicensed}})
	if len(got) != 1 || len(want) != 1 || got[0].AuthState != want[0].AuthState || got[0].Evidence != want[0].Evidence {
		t.Fatalf("RollUpProviderStatus = %+v, agent wrapper = %+v", got, want)
	}
	if got[0].AuthState != ProviderAuthNeedsLogin {
		t.Fatalf("AuthState = %q, want %q", got[0].AuthState, ProviderAuthNeedsLogin)
	}
}

func TestFatalNetworkErrorPatternsIsACopy(t *testing.T) {
	got := FatalNetworkErrorPatterns()
	if len(got) != len(agent.FatalNetworkErrorPatterns) || len(got) == 0 {
		t.Fatalf("FatalNetworkErrorPatterns = %v, want the runtime's %v", got, agent.FatalNetworkErrorPatterns)
	}
	got[0] = "mutated"
	if agent.FatalNetworkErrorPatterns[0] == "mutated" {
		t.Fatal("FatalNetworkErrorPatterns must return a copy")
	}
}
