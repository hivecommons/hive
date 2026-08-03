package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Regression for the LiteLLM inference/entitlement split-brain (post-rotation
// 401s): when an explicit `gateways:` block names a "litellm" gateway with its
// OWN key file (the file the Model Gateways tab writes), inference must resolve
// its key from THAT gateway — the same source the entitlement/probe path uses
// (ResolveGateway(name).ResolveAPIKey()) — not from the legacy Governor.LiteLLM
// key file. Before the fix, inference read the legacy file and sent a stale key
// while entitlement validated the fresh gateway key, causing inference 401s.
func TestResolveLiteLLMInferenceKeyMatchesEntitlement(t *testing.T) {
	dir := t.TempDir()
	// Key A: the fresh key saved via the Gateways tab (the gateway's file).
	gatewayKeyFile := filepath.Join(dir, "gateway_litellm_api_key")
	if err := os.WriteFile(gatewayKeyFile, []byte("sk-gateway-FRESH\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Key B: the stale legacy key (the classic litellm_api_key file).
	legacyKeyFile := filepath.Join(dir, "litellm_api_key")
	if err := os.WriteFile(legacyKeyFile, []byte("sk-legacy-STALE\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	g := GovernorConfig{
		// Legacy block points at the STALE file B.
		LiteLLM: LiteLLMConfig{
			Endpoint:   "https://litellm.example.com",
			APIKeyFile: legacyKeyFile,
		},
		// Explicit gateway named "litellm" points at the FRESH file A — this is
		// what the entitlement/probe path resolves and validates.
		Gateways: []GatewayConfig{
			{Name: "litellm", Kind: GatewayKindLiteLLM, Endpoint: "https://litellm.example.com", APIKeyFile: gatewayKeyFile},
		},
	}

	// Entitlement path (dashboard) resolves the gateway's key.
	entitlementKey := g.ResolveGateway("litellm").ResolveAPIKey()
	if entitlementKey != "sk-gateway-FRESH" {
		t.Fatalf("entitlement key = %q, want the gateway's fresh key", entitlementKey)
	}

	// Inference path MUST resolve the same fresh key (fails before the fix,
	// which returned the legacy stale key).
	inferenceKey := g.ResolveLiteLLMInferenceKey("litellm")
	if inferenceKey != entitlementKey {
		t.Fatalf("inference key = %q, want %q (must match entitlement — one source, no split-brain)", inferenceKey, entitlementKey)
	}
	if inferenceKey == "sk-legacy-STALE" {
		t.Fatal("inference resolved the STALE legacy key — the split-brain 401 bug is back")
	}
}

// No-regression: a classic hive with only the legacy `litellm:` block (NO
// explicit gateways:) must still resolve the inference key from the legacy
// Governor.LiteLLM resolver, including its multi-location file fallback. The fix
// must not change behavior for these hives.
func TestResolveLiteLLMInferenceKeyLegacyFallback(t *testing.T) {
	dir := t.TempDir()
	legacyKeyFile := filepath.Join(dir, "litellm_api_key")
	if err := os.WriteFile(legacyKeyFile, []byte("sk-legacy-ONLY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := GovernorConfig{
		LiteLLM: LiteLLMConfig{
			Endpoint:   "https://litellm.example.com",
			APIKeyFile: legacyKeyFile,
		},
		// No Gateways configured.
	}
	if got := g.ResolveLiteLLMInferenceKey("litellm"); got != "sk-legacy-ONLY" {
		t.Fatalf("no-gateway hive: inference key = %q, want the legacy key %q", got, "sk-legacy-ONLY")
	}
}

// A hive with only the legacy litellm block (no gateways:) must resolve to a
// single implicit gateway named "litellm" so existing agents route unchanged.
func TestResolvedGatewaysBackCompat(t *testing.T) {
	g := GovernorConfig{
		LiteLLM: LiteLLMConfig{
			Endpoint:     "https://litellm.example.com",
			APIKeyEnv:    "HIVE_LITELLM_API_KEY",
			DefaultModel: "gpt-4o",
		},
	}
	gws := g.ResolvedGateways()
	if len(gws) != 1 {
		t.Fatalf("expected 1 synthesized gateway, got %d", len(gws))
	}
	if gws[0].Name != legacyLiteLLMGatewayName {
		t.Fatalf("synthesized gateway name = %q, want %q", gws[0].Name, legacyLiteLLMGatewayName)
	}
	if gws[0].Endpoint != "https://litellm.example.com" || gws[0].DefaultModel != "gpt-4o" {
		t.Fatalf("synthesized gateway did not carry legacy fields: %+v", gws[0])
	}
	// Resolving by the legacy name AND by empty (default) both hit it.
	if g.ResolveGateway("") == nil || g.ResolveGateway("litellm") == nil {
		t.Fatal("default and legacy-name resolution must both find the gateway")
	}
}

// No litellm endpoint and no gateways → nothing resolves (a hive that never
// configured inference). Must not panic or synthesize a bogus gateway.
func TestResolvedGatewaysEmpty(t *testing.T) {
	g := GovernorConfig{}
	if len(g.ResolvedGateways()) != 0 {
		t.Fatal("empty config must resolve to no gateways")
	}
	if g.ResolveGateway("") != nil || g.ResolveGateway("anything") != nil {
		t.Fatal("no gateways → resolution returns nil")
	}
}

// Explicit gateways take precedence over the legacy block, and lookup is
// case-insensitive; empty name returns the first (default).
func TestResolveGatewayExplicit(t *testing.T) {
	g := GovernorConfig{
		LiteLLM: LiteLLMConfig{Endpoint: "https://legacy.example.com"}, // must be IGNORED when gateways present
		Gateways: []GatewayConfig{
			{Name: "openrouter", Kind: GatewayKindOpenRouter, Endpoint: "https://openrouter.ai/api/v1", DefaultModel: "anthropic/claude-opus-4.8"},
			{Name: "corp", Kind: GatewayKindLiteLLM, Endpoint: "https://litellm.internal:4000"},
		},
	}
	if got := g.ResolveGateway(""); got == nil || got.Name != "openrouter" {
		t.Fatalf("empty name must return first gateway (openrouter), got %+v", got)
	}
	if got := g.ResolveGateway("CORP"); got == nil || got.Endpoint != "https://litellm.internal:4000" {
		t.Fatalf("case-insensitive lookup failed: %+v", got)
	}
	if g.ResolveGateway("nope") != nil {
		t.Fatal("unknown gateway must resolve to nil")
	}
	// Legacy block must NOT leak in when explicit gateways are set.
	for _, gw := range g.ResolvedGateways() {
		if gw.Endpoint == "https://legacy.example.com" {
			t.Fatal("legacy litellm endpoint leaked despite explicit gateways")
		}
	}
}

// A gateway name is accepted as a valid agent backend by validate().
func TestGatewayNameValidAsBackend(t *testing.T) {
	c := &Config{
		Project: ProjectConfig{Org: "acme"},
		GitHub:  GitHubConfig{Token: "x"},
		Agents: map[string]AgentConfig{
			"scanner": {Backend: "openrouter"},
		},
		Governor: GovernorConfig{
			Gateways: []GatewayConfig{
				{Name: "openrouter", Endpoint: "https://openrouter.ai/api/v1"},
			},
		},
	}
	if err := c.validate(); err != nil {
		t.Fatalf("gateway name should be a valid backend, got: %v", err)
	}
	// A backend that is neither a known CLI/inference backend nor a gateway fails.
	c.Agents["scanner"] = AgentConfig{Backend: "not-a-thing"}
	if err := c.validate(); err == nil {
		t.Fatal("unknown backend must be rejected")
	}
}
