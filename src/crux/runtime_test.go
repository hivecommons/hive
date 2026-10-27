package crux_test

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/agent"
)

// TestValueAliasesTrackAgent pins that the façade's constants are the ones
// the manager writes (crux defines them, agent aliases them), so a consumer
// comparing crux.StateRunning against a Process.State can never drift.
func TestValueAliasesTrackAgent(t *testing.T) {
	if crux.StateRunning != agent.StateRunning || crux.StatePaused != agent.StatePaused || crux.StateStopped != agent.StateStopped {
		t.Fatal("process state constants diverged from pkg/agent")
	}
	if crux.KickPhasePending != agent.KickPhasePending || crux.KickPhaseFailed != agent.KickPhaseFailed {
		t.Fatal("kick phase constants diverged from pkg/agent")
	}
	if crux.StartFailureCredentialRejected != agent.StartFailureCredentialRejected {
		t.Fatal("start failure constants diverged from pkg/agent")
	}
	var p crux.Process
	p.State = crux.StateRunning
	if p.State != agent.StateRunning {
		t.Fatal("Process.State does not share agent.AgentProcess's State type")
	}
	p.KickOutcome = crux.KickOutcome{Kind: crux.KickOutcomeEnded, KickAt: time.Unix(1, 0)}
	kickAt := time.Unix(1, 0)
	if !p.KickOutcome.Settled(&kickAt) || p.KickOutcome.Kind != agent.KickOutcomeEnded {
		t.Fatal("KickOutcome alias does not share agent.AgentProcess's KickOutcome type")
	}
	p.RestartEvents = []crux.RestartEvent{{Reason: "x"}}
	p.TurnLoss = crux.TurnLoss{Recent: []crux.TurnInterruption{{Reason: "restart"}}}
	p.KickHistory = []crux.KickRecord{{Agent: "a"}}
	p.SubAgentModels = []crux.SubAgentModel{{Model: "m"}}
	p.StartFailureClass = string(crux.StartFailureNoOutput)
	if p.StartFailureClass != string(agent.StartFailureNoOutput) || !(crux.KickDispatch{Phase: crux.KickPhasePending}).Pending() {
		t.Fatal("start-failure or kick-dispatch aliases diverged from pkg/agent")
	}
}

// TestProviderStatusAliasesTrackTypes pins that the provider-status surface
// re-exported here is the same one pkg/agent aliases, so a hub comparing
// crux.BackendAuthOK against a Process.BackendAuth.Status the runtime wrote
// can never drift.
func TestProviderStatusAliasesTrackTypes(t *testing.T) {
	if crux.BackendAuthOK != agent.BackendAuthOK || crux.BackendAuthForbidden != agent.BackendAuthForbidden {
		t.Fatal("backend auth constants diverged from pkg/agent")
	}
	if crux.ProviderAuthNeedsLogin != agent.ProviderAuthNeedsLogin || crux.ProviderAvailUnknown != agent.ProviderAvailUnknown {
		t.Fatal("provider state constants diverged from pkg/agent")
	}
	var p crux.Process
	p.BackendAuth = crux.BackendAuthState{Status: crux.BackendAuthQuota}
	if p.BackendAuth.Status != agent.BackendAuthQuota {
		t.Fatal("BackendAuthState alias does not share agent.AgentProcess's BackendAuth type")
	}
	got := crux.RollUpProviderStatus([]crux.ProviderAgentEvidence{{Agent: "a", Provider: "copilot", Enabled: true, AuthStatus: crux.BackendAuthUnlicensed}})
	want := agent.RollUpProviderStatus([]agent.ProviderAgentEvidence{{Agent: "a", Provider: "copilot", Enabled: true, AuthStatus: crux.BackendAuthUnlicensed}})
	if len(got) != 1 || len(want) != 1 || got[0].AuthState != want[0].AuthState || got[0].Evidence != want[0].Evidence {
		t.Fatalf("RollUpProviderStatus = %+v, agent wrapper = %+v", got, want)
	}
	if got[0].AuthState != crux.ProviderAuthNeedsLogin {
		t.Fatalf("AuthState = %q, want %q", got[0].AuthState, crux.ProviderAuthNeedsLogin)
	}
}

func TestFatalNetworkErrorPatternsIsACopy(t *testing.T) {
	got := crux.FatalNetworkErrorPatterns()
	if len(got) != len(agent.FatalNetworkErrorPatterns) || len(got) == 0 {
		t.Fatalf("FatalNetworkErrorPatterns = %v, want the runtime's %v", got, agent.FatalNetworkErrorPatterns)
	}
	got[0] = "mutated"
	if agent.FatalNetworkErrorPatterns[0] == "mutated" {
		t.Fatal("FatalNetworkErrorPatterns must return a copy")
	}
}
