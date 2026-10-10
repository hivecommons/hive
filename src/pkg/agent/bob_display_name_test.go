package agent

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestBobSessionLabelEnvPair pins #11273: bob-backed agents get an optional
// HIVE_BOB_SESSION_LABEL that is separate from the functional agent name,
// which stays in HIVE_AGENT.
func TestBobSessionLabelEnvPair(t *testing.T) {
	tests := []struct {
		name      string
		backend   string
		override  string
		prefix    string
		label     string
		wantPair  bool
		wantValue string
	}{
		{"bob with per-agent label", "bob", "", "", "hive-scanner", true, "hive-scanner"},
		{"bob with global prefix", "bob", "", "hive-", "", true, "hive-scanner"},
		{"bob without label gets no session label", "bob", "", "", "", false, ""},
		{"bob label is trimmed", "bob", "", "", "  hive-scanner ", true, "hive-scanner"},
		{"backend override to bob", "claude", "bob", "", "hive-scanner", true, "hive-scanner"},
		{"claude backend gets no pair", "claude", "", "", "hive-scanner", false, ""},
		{"copilot backend gets no pair", "copilot", "", "", "", false, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{logger: discardLogger(), project: ProjectContext{BobSessionPrefix: tc.prefix}}
			agent := &AgentProcess{
				Name:            "scanner",
				BackendOverride: tc.override,
				Config:          config.AgentConfig{Backend: tc.backend, Bob: config.AgentBobConfig{SessionLabel: tc.label}},
			}

			var found *agentEnvPair
			var hiveAgent string
			for _, p := range m.agentEnvPairs(agent) {
				switch p.Key {
				case "HIVE_BOB_SESSION_LABEL":
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
				t.Errorf("HIVE_BOB_SESSION_LABEL = %q, want %q", found.Value, tc.wantValue)
			}
			if found.Secret {
				t.Error("HIVE_BOB_SESSION_LABEL must not be Secret: it is a label re-applied on every launch")
			}
		})
	}
}
