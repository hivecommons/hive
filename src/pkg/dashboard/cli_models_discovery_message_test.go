package dashboard

import (
	"strings"
	"testing"
)

func TestDiscoveryDegradedMessage(t *testing.T) {
	tests := []struct {
		name, backend, err string
		wantRecovery       bool
	}{
		{"copilot not authenticated", "copilot", "sdk helper: exit status 1 (stderr: Not authenticated. Please authenticate first.)", true},
		{"copilot other error", "copilot", "502 bad gateway", false},
		{"other backend auth error", "gemini", "Not authenticated", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := discoveryDegradedMessage(tc.backend, "static-fallback", tc.err)
			if !strings.HasPrefix(got, "model discovery degraded for "+tc.backend) {
				t.Errorf("message lost its keyword prefix: %q", got)
			}
			if has := strings.Contains(got, "credential_missing") && strings.Contains(got, "dashboard device-flow login"); has != tc.wantRecovery {
				t.Errorf("recovery attribution = %v, want %v (%q)", has, tc.wantRecovery, got)
			}
		})
	}
}
