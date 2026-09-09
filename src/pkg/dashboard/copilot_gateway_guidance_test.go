package dashboard

import (
	"strings"
	"testing"
)

// The Model Gateways tab is a plausible dead end for an operator trying to
// configure Copilot (or Claude, Codex, Gemini, …) for inference: those agent
// CLI backends are intentionally absent because they are subscription CLI
// backends, not Model Gateways, but the distinction used to exist only in
// repository documentation that the tab did not link (#6319, #6410). Keep
// the explanation, the complete dashboard setup path, and the documentation
// escape hatch at the point where that confusion occurs.
func TestModelGatewaysExplainsHowToConfigureCopilot(t *testing.T) {
	html := indexHTML(t)
	cases := []struct {
		name    string
		snippet string
	}{
		{"guidance is rendered on the tab", "data-copilot-backend-help"},
		{"names the CLI backends", "Looking for Copilot, Claude, Codex, or Gemini?"},
		{"distinguishes the backend types", "Copilot is a subscription CLI backend, not a Model Gateway"},
		{"covers the other CLI backends too", "the same is true for Claude, Codex, Gemini, and other\n            agent CLI backends"},
		{"names the pinning control", "<strong>CLI Pinned</strong>"},
		{"names the backend picker", "<strong>Copilot</strong> under\n            <strong>CLI Pin Value</strong>"},
		{"names the authentication action", "use <strong>Login</strong> on\n            that agent's card"},
		{"links the setup guide", `href="https://github.com/hivecommons/hive/blob/v4/docs/inference-backends.md" target="_blank" rel="noopener noreferrer"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(html, tc.snippet) {
				t.Errorf("Model Gateways Copilot guidance is missing %q", tc.snippet)
			}
		})
	}

	guidance := strings.Index(html, "data-copilot-backend-help")
	addGateway := strings.Index(html, "data-action=\"openGatewayForm\"")
	if guidance < 0 || addGateway < 0 || guidance > addGateway {
		t.Error("Copilot guidance must appear before the gateway actions where an operator could continue down the wrong setup path")
	}
}
