package agent

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestBobDisplayNameEnvPair pins #11273: bob-backed agents get a reporting
// label in HIVE_BOB_DISPLAY_NAME that is separate from the functional agent
// name, which stays in HIVE_AGENT.
func TestBobDisplayNameEnvPair(t *testing.T) {
	tests := []struct {
		name      string
		backend   string
		override  string
		label     string
		wantPair  bool
		wantValue string
	}{
		{"bob with label", "bob", "", "hive-scanner", true, "hive-scanner"},
		{"bob without label falls back to agent name", "bob", "", "", true, "scanner"},
		{"bob label is trimmed", "bob", "", "  hive-scanner ", true, "hive-scanner"},
		{"backend override to bob", "claude", "bob", "hive-scanner", true, "hive-scanner"},
		{"claude backend gets no pair", "claude", "", "hive-scanner", false, ""},
		{"copilot backend gets no pair", "copilot", "", "", false, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{logger: discardLogger()}
			agent := &AgentProcess{
				Name:            "scanner",
				BackendOverride: tc.override,
				Config:          config.AgentConfig{Backend: tc.backend, BobDisplayName: tc.label},
			}

			var found *agentEnvPair
			var hiveAgent string
			for _, p := range m.agentEnvPairs(agent) {
				switch p.Key {
				case "HIVE_BOB_DISPLAY_NAME":
					pair := p
					found = &pair
				case "HIVE_AGENT":
					hiveAgent = p.Value
				}
			}
			if hiveAgent != "scanner" {
				t.Errorf("HIVE_AGENT = %q, want functional name %q", hiveAgent, "scanner")
			}
			if tc.wantPair != (found != nil) {
				t.Fatalf("HIVE_BOB_DISPLAY_NAME present = %v, want %v", found != nil, tc.wantPair)
			}
			if found == nil {
				return
			}
			if found.Value != tc.wantValue {
				t.Errorf("HIVE_BOB_DISPLAY_NAME = %q, want %q", found.Value, tc.wantValue)
			}
			if found.Secret {
				t.Error("HIVE_BOB_DISPLAY_NAME must not be Secret: it is a label re-applied on every launch")
			}
		})
	}
}
