package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// resolveLiteLLMInferenceRoute is the route-install decision tree for the
// built-in "litellm" backend. It shipped inline in main() with no coverage,
// which is exactly how the #5393 outage happened: a hive configured ONLY
// through the Model Gateways tab (explicit gateway named "litellm", legacy
// governor.litellm block empty) resolved its key and CA bundle from that
// gateway but its ENDPOINT from the empty legacy block, so no route was ever
// installed and every agent call died "502 no inference route" while the
// Gateways tab Test button passed. 231ca4b fixed it; this table pins the fix.
//
// The `gateway fallback` cases below FAIL against pre-231ca4b behavior (delete
// the gateway-fallback block in resolveLiteLLMInferenceRoute and they go red
// with ok=false), which is the point — a test that passes on both the fixed and
// the broken code would guard nothing.
func TestResolveLiteLLMInferenceRoute(t *testing.T) {
	// The bundled local proxy's loopback URL is not a literal here: it must
	// track litellmLocalProxyURL(), which owns the port.
	localProxy := litellmLocalProxyURL()

	cases := []struct {
		name           string
		litellm        config.LiteLLMConfig
		gateways       []config.GatewayConfig
		backend        string
		requestedModel string

		wantEndpoint string
		wantModel    string
		wantOK       bool
	}{
		{
			// Classic hive: legacy block populated, no gateways. The legacy
			// endpoint is used directly and its default_model fills in.
			name:         "legacy endpoint used directly",
			litellm:      config.LiteLLMConfig{Endpoint: "https://legacy.example", DefaultModel: "legacy-model"},
			backend:      "litellm",
			wantEndpoint: "https://legacy.example",
			wantModel:    "legacy-model",
			wantOK:       true,
		},
		{
			// An agent that names a model keeps it; the legacy default_model
			// is only a fallback, never an override.
			name:           "requested model beats legacy default",
			litellm:        config.LiteLLMConfig{Endpoint: "https://legacy.example", DefaultModel: "legacy-model"},
			backend:        "litellm",
			requestedModel: "asked-for-this",
			wantEndpoint:   "https://legacy.example",
			wantModel:      "asked-for-this",
			wantOK:         true,
		},
		{
			// local_proxy wins over BOTH the configured legacy endpoint and
			// any gateway: the Go translator forwards to the bundled proxy on
			// loopback. Losing this would silently send traffic upstream.
			name: "local proxy overrides configured legacy endpoint",
			litellm: config.LiteLLMConfig{
				Endpoint: "https://legacy.example", DefaultModel: "legacy-model", LocalProxy: true,
			},
			gateways:     []config.GatewayConfig{{Name: "litellm", Endpoint: "https://gateway.example"}},
			backend:      "litellm",
			wantEndpoint: localProxy,
			wantModel:    "legacy-model",
			wantOK:       true,
		},
		{
			name:         "local proxy with empty legacy block still routes",
			litellm:      config.LiteLLMConfig{LocalProxy: true},
			backend:      "litellm",
			wantEndpoint: localProxy,
			wantModel:    "",
			wantOK:       true,
		},
		{
			// THE #5393 CASE. Legacy block entirely empty, one explicit
			// gateway named "litellm" from the Model Gateways tab. Pre-231ca4b
			// this returned no endpoint and installed no route -> 502.
			name:         "gateway fallback when legacy endpoint empty",
			litellm:      config.LiteLLMConfig{},
			gateways:     []config.GatewayConfig{{Name: "litellm", Kind: config.GatewayKindLiteLLM, Endpoint: "https://gateway.example", DefaultModel: "gateway-model"}},
			backend:      "litellm",
			wantEndpoint: "https://gateway.example",
			wantModel:    "gateway-model",
			wantOK:       true,
		},
		{
			// The fallback must inherit the GATEWAY's default_model, not the
			// (empty) legacy one — an empty model on the route is its own 4xx.
			name:           "gateway fallback keeps an explicitly requested model",
			litellm:        config.LiteLLMConfig{},
			gateways:       []config.GatewayConfig{{Name: "litellm", Endpoint: "https://gateway.example", DefaultModel: "gateway-model"}},
			backend:        "litellm",
			requestedModel: "asked-for-this",
			wantEndpoint:   "https://gateway.example",
			wantModel:      "asked-for-this",
			wantOK:         true,
		},
		{
			// Gateway name match is case-insensitive (ResolveGateway uses
			// EqualFold), so a tab-entered "LiteLLM" still backs the backend.
			name:         "gateway fallback matches name case-insensitively",
			litellm:      config.LiteLLMConfig{},
			gateways:     []config.GatewayConfig{{Name: "LiteLLM", Endpoint: "https://cased.example", DefaultModel: "cased-model"}},
			backend:      "litellm",
			wantEndpoint: "https://cased.example",
			wantModel:    "cased-model",
			wantOK:       true,
		},
		{
			// A gateway exists but carries no endpoint: there is nothing to
			// fall back TO, so this must fail honestly rather than install a
			// route with an empty endpoint.
			name:         "gateway present but endpoint empty is not a route",
			litellm:      config.LiteLLMConfig{},
			gateways:     []config.GatewayConfig{{Name: "litellm", DefaultModel: "gateway-model"}},
			backend:      "litellm",
			wantEndpoint: "",
			wantModel:    "",
			wantOK:       false,
		},
		{
			// A gateway list that does not name this backend must NOT be
			// borrowed — routing litellm through someone else's endpoint and
			// key is worse than no route.
			name:         "unrelated gateway is not borrowed",
			litellm:      config.LiteLLMConfig{},
			gateways:     []config.GatewayConfig{{Name: "openrouter", Endpoint: "https://openrouter.example", DefaultModel: "or-model"}},
			backend:      "litellm",
			wantEndpoint: "",
			wantModel:    "",
			wantOK:       false,
		},
		{
			// Nothing configured anywhere: the honest failure. ok=false tells
			// main() to warn and install NO route. The contract is an explicit
			// false, NOT a silently empty endpoint string.
			name:         "nothing configured yields no route",
			litellm:      config.LiteLLMConfig{},
			backend:      "litellm",
			wantEndpoint: "",
			wantModel:    "",
			wantOK:       false,
		},
		{
			// On failure the requested model is handed back unchanged so the
			// caller's warning names what the agent actually asked for.
			name:           "no route preserves the requested model for the warning",
			litellm:        config.LiteLLMConfig{},
			backend:        "litellm",
			requestedModel: "asked-for-this",
			wantEndpoint:   "",
			wantModel:      "asked-for-this",
			wantOK:         false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// HIVE_LITELLM_ENDPOINT is consulted by ResolveEndpoint and would
			// leak a real environment into these cases.
			t.Setenv(config.LiteLLMEndpointEnv, "")

			cfg := &config.Config{Governor: config.GovernorConfig{
				LiteLLM:  tc.litellm,
				Gateways: tc.gateways,
			}}

			endpoint, model, ok := resolveLiteLLMInferenceRoute(cfg, tc.backend, tc.requestedModel)

			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (endpoint=%q model=%q)", ok, tc.wantOK, endpoint, model)
			}
			if endpoint != tc.wantEndpoint {
				t.Errorf("endpoint = %q, want %q", endpoint, tc.wantEndpoint)
			}
			if model != tc.wantModel {
				t.Errorf("model = %q, want %q", model, tc.wantModel)
			}
			// The 502 this path exists to prevent: ok must never be true with
			// nothing to route to.
			if ok && endpoint == "" {
				t.Errorf("ok=true with an empty endpoint — that installs a dead route (502 no inference route)")
			}
		})
	}
}

// The env var overrides the yaml endpoint, and local_proxy still beats it.
// ResolveEndpoint owns this precedence; pinning it here keeps the route-level
// tree honest about which source it consulted.
func TestResolveLiteLLMInferenceRouteEnvEndpoint(t *testing.T) {
	t.Setenv(config.LiteLLMEndpointEnv, "https://from-env.example")
	cfg := &config.Config{Governor: config.GovernorConfig{
		LiteLLM:  config.LiteLLMConfig{Endpoint: "https://from-yaml.example", DefaultModel: "legacy-model"},
		Gateways: []config.GatewayConfig{{Name: "litellm", Endpoint: "https://gateway.example"}},
	}}

	endpoint, model, ok := resolveLiteLLMInferenceRoute(cfg, "litellm", "")
	if !ok || endpoint != "https://from-env.example" || model != "legacy-model" {
		t.Fatalf("env endpoint: got (%q, %q, %v), want (https://from-env.example, legacy-model, true)", endpoint, model, ok)
	}
}
