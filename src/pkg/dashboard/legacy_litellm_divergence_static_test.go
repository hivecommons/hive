package dashboard

import (
	"os"
	"strings"
	"testing"
)

// TestLegacyLiteLLMDivergenceStaticWiring pins the #12090 warning: the legacy
// governor.litellm block on the Model Gateways tab must flag — and open —
// when a gateway named "litellm" points at a different URL, and a litellm
// gateway save must mirror its endpoint into the loaded legacy config so the
// warning clears without a reload.
func TestLegacyLiteLLMDivergenceStaticWiring(t *testing.T) {
	html, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read static index: %v", err)
	}
	s := string(html)
	for _, want := range []string{
		"function legacyLiteLLMDivergence(litellm)",
		"function renderLegacyLiteLLMDivergence(diverged)",
		"function refreshLegacyLiteLLMDivergence()",
		`<details id="legacy-litellm-section" class="card-inset mt-4"${diverged ? ' open' : ''}>`,
		`<div id="legacy-litellm-divergence">${renderLegacyLiteLLMDivergence(diverged)}</div>`,
		"data-legacy-litellm-diverged",
		"is not the <code>litellm</code> gateway listed above",
		"or clear it, then press Save.",
		"String(g.name || '').toLowerCase() === 'litellm'",
		"_configState.data.litellm = Object.assign({}, _configState.data.litellm || {}, { endpoint: payload.endpoint });",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("static dashboard missing %q", want)
		}
	}
	// loadGateways must re-evaluate the warning once the async list arrives.
	loadIdx := strings.Index(s, "async function loadGateways() {")
	if loadIdx < 0 {
		t.Fatal("loadGateways not found")
	}
	body := s[loadIdx:]
	if end := strings.Index(body, "function renderGatewaysList()"); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "refreshLegacyLiteLLMDivergence();") {
		t.Fatal("loadGateways does not refresh the legacy LiteLLM divergence warning")
	}
}
