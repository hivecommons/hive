package main

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// resolveWatsonxGateway must prefer the gateway BOTH named and kinded
// "watsonx" (the built-in backend's canonical slot) over a merely
// watsonx-kinded gateway with another name, fall back to kind-only when no
// canonical slot exists, and return nil when no watsonx gateway is configured
// — resolving the wrong gateway would route the built-in watsonx backend
// through someone else's endpoint and key.
func TestResolveWatsonxGateway(t *testing.T) {
	canonical := config.GatewayConfig{Name: "watsonx", Kind: config.GatewayKindWatsonx, Endpoint: "https://canonical"}
	kindOnly := config.GatewayConfig{Name: "ibm-granite", Kind: config.GatewayKindWatsonx, Endpoint: "https://kind-only"}
	other := config.GatewayConfig{Name: "openrouter", Kind: "litellm", Endpoint: "https://other"}

	cases := []struct {
		name     string
		gateways []config.GatewayConfig
		want     string // expected Endpoint, "" for nil
	}{
		{"canonical preferred over kind-only", []config.GatewayConfig{other, kindOnly, canonical}, "https://canonical"},
		{"kind-only fallback", []config.GatewayConfig{other, kindOnly}, "https://kind-only"},
		{"no watsonx gateway", []config.GatewayConfig{other}, ""},
		{"no gateways at all", nil, ""},
		{"case-insensitive match", []config.GatewayConfig{{Name: "WatsonX", Kind: "WATSONX", Endpoint: "https://cased"}}, "https://cased"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Governor: config.GovernorConfig{Gateways: tc.gateways}}
			got := resolveWatsonxGateway(cfg)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("resolveWatsonxGateway = %+v, want nil", got)
				}
				return
			}
			if got == nil || got.Endpoint != tc.want {
				t.Fatalf("resolveWatsonxGateway endpoint = %v, want %q", got, tc.want)
			}
		})
	}
}

// For every non-watsonx kind, resolveGatewayAuth must hand back the resolved
// API key VERBATIM with no extra headers — only watsonx swaps the raw key for
// an IAM bearer and adds the project header. A mutated key or a stray header
// on a litellm/openrouter route would break auth at the upstream.
func TestResolveGatewayAuthNonWatsonxPassesKeyVerbatim(t *testing.T) {
	const keyEnv = "HIVE_TEST_GATEWAY_KEY"
	t.Setenv(keyEnv, "sk-verbatim-key")
	gw := &config.GatewayConfig{Name: "openrouter", Kind: "litellm", APIKeyEnv: keyEnv}

	key, headers := resolveGatewayAuth(gw, "scanner", "openrouter", restoreTestLogger())

	if key != "sk-verbatim-key" {
		t.Errorf("key = %q, want the resolved key verbatim", key)
	}
	if headers != nil {
		t.Errorf("extra headers = %v, want nil for a non-watsonx gateway", headers)
	}
}

// A keyless non-watsonx gateway (some vLLM endpoints) resolves to an empty
// key and still no headers — never a fabricated credential.
func TestResolveGatewayAuthKeylessGateway(t *testing.T) {
	gw := &config.GatewayConfig{Name: "local-vllm", Kind: "vllm"}
	key, headers := resolveGatewayAuth(gw, "scanner", "local-vllm", restoreTestLogger())
	if key != "" || headers != nil {
		t.Errorf("keyless gateway auth = (%q, %v), want empty key and nil headers", key, headers)
	}
}

// litellmLocalProxyURL is the forwarding contract between the Go inference
// translator and the bundled litellm proxy: loopback-only (agents must never
// reach it directly) on the dedicated local-proxy port.
func TestLitellmLocalProxyURLIsLoopback(t *testing.T) {
	url := litellmLocalProxyURL()
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Errorf("litellmLocalProxyURL = %q, want a loopback http URL", url)
	}
	if url != "http://127.0.0.1:18445" {
		t.Errorf("litellmLocalProxyURL = %q, want the pinned local-proxy port 18445 the translator forwards to", url)
	}
}
