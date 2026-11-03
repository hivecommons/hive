package dashboard

import (
	"strings"
	"testing"
)

func TestDiscoveryDegradedMessage(t *testing.T) {
	got := discoveryDegradedMessage("copilot", "static-fallback")
	if !strings.HasPrefix(got, "model discovery degraded for copilot") || !strings.Contains(got, "static-fallback") {
		t.Errorf("message lost its keyword prefix or served state: %q", got)
	}
}

func TestIsCopilotCredentialMissing(t *testing.T) {
	tests := []struct {
		name, backend, err string
		want               bool
	}{
		{"copilot not authenticated", "copilot", "sdk helper: exit status 1 (stderr: Not authenticated. Please authenticate first.)", true},
		{"copilot other error", "copilot", "502 bad gateway", false},
		{"other backend auth error", "gemini", "Not authenticated", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCopilotCredentialMissing(tc.backend, tc.err); got != tc.want {
				t.Errorf("isCopilotCredentialMissing(%q, %q) = %v, want %v", tc.backend, tc.err, got, tc.want)
			}
		})
	}
}
