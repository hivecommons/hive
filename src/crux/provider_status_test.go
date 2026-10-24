package crux

import "testing"

func TestRollUpProviderStatusDelegatesToTypes(t *testing.T) {
	got := RollUpProviderStatus([]ProviderAgentEvidence{
		{Agent: "scanner", Provider: "copilot", Enabled: true, AuthStatus: BackendAuthUnlicensed},
		{Agent: "outreach", Provider: "gemini", Enabled: true},
	})
	if len(got) != 2 || got[0].Provider != "copilot" || got[1].Provider != "gemini" {
		t.Fatalf("RollUpProviderStatus = %+v, want sorted [copilot gemini]", got)
	}
	if got[0].AuthState != ProviderAuthNeedsLogin {
		t.Errorf("copilot auth_state = %q, want %q", got[0].AuthState, ProviderAuthNeedsLogin)
	}
	if got[1].AuthState != ProviderAuthUsable || got[1].AvailabilityState != ProviderAvailable {
		t.Errorf("gemini = %+v, want usable and available", got[1])
	}
	if RollUpProviderStatus(nil) != nil {
		t.Error("RollUpProviderStatus(nil) should be empty")
	}
}
