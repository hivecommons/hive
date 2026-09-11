package dashboard

import (
	"strings"
	"testing"
)

// TestGooseOpenAIEndpointEnvironmentIsForwarded pins the endpoint half of
// Goose's OpenAI-compatible provider configuration. Forwarding only the API
// key/provider/model leaves Goose using api.openai.com's default endpoint even
// when the contributor explicitly configured a local inference server.
func TestGooseOpenAIEndpointEnvironmentIsForwarded(t *testing.T) {
	src := justfileSource(t)
	forwardIdx := strings.Index(src, "for name in ANTHROPIC_API_KEY")
	if forwardIdx < 0 {
		t.Fatal("the provider-env forwarding list was not found")
	}
	line := src[forwardIdx:]
	if end := strings.Index(line, "\n"); end > 0 {
		line = line[:end]
	}

	for _, name := range []string{"OPENAI_HOST", "OPENAI_BASE_PATH"} {
		if !strings.Contains(line, name) {
			t.Errorf("%s is not forwarded into the Goose contributor container (#6400)", name)
		}
	}
}
