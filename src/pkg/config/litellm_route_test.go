package config

import "testing"

func TestResolveLiteLLMRemoteRoute_ExplicitGatewayBeatsStaleLegacy(t *testing.T) {
	t.Setenv(LiteLLMEndpointEnv, "https://env-old.example")
	g := GovernorConfig{
		LiteLLM: LiteLLMConfig{Endpoint: "https://yaml-old.example", DefaultModel: "old-model"},
		Gateways: []GatewayConfig{{
			Name: "litellm", Kind: GatewayKindLiteLLM,
			Endpoint: " https://litellm.cb.ete.res.ibm.com ", DefaultModel: "rits/zai-org/glm-5-3",
		}},
	}
	ep, model, src := g.ResolveLiteLLMRemoteRoute("litellm", "")
	if ep != "https://litellm.cb.ete.res.ibm.com" || model != "rits/zai-org/glm-5-3" || src != LiteLLMRouteGateway {
		t.Fatalf("got (%q, %q, %v), want explicit gateway route", ep, model, src)
	}
	if _, model, _ := g.ResolveLiteLLMRemoteRoute("LiteLLM", "requested"); model != "requested" {
		t.Fatalf("requested model not preserved: %q", model)
	}
	if gw := g.ExplicitLiteLLMGateway("litellm"); gw == nil || gw.Endpoint != "https://litellm.cb.ete.res.ibm.com" {
		t.Fatalf("ExplicitLiteLLMGateway = %+v", gw)
	}
}

func TestResolveLiteLLMRemoteRoute_LegacySources(t *testing.T) {
	g := GovernorConfig{
		LiteLLM:  LiteLLMConfig{Endpoint: "https://yaml.example", DefaultModel: "yaml-model"},
		Gateways: []GatewayConfig{{Name: "litellm", Kind: GatewayKindLiteLLM}}, // no endpoint → falls back
	}
	t.Setenv(LiteLLMEndpointEnv, "")
	if ep, model, src := g.ResolveLiteLLMRemoteRoute("litellm", ""); ep != "https://yaml.example" || model != "yaml-model" || src != LiteLLMRouteLegacyYAML {
		t.Fatalf("yaml: got (%q, %q, %v)", ep, model, src)
	}
	if g.ExplicitLiteLLMGateway("litellm") != nil {
		t.Fatal("endpoint-less gateway must not count as explicit")
	}
	t.Setenv(LiteLLMEndpointEnv, "https://env.example")
	if ep, _, src := g.ResolveLiteLLMRemoteRoute("litellm", ""); ep != "https://env.example" || src != LiteLLMRouteLegacyEnv {
		t.Fatalf("env: got (%q, %v)", ep, src)
	}
	none := GovernorConfig{}
	t.Setenv(LiteLLMEndpointEnv, "")
	if ep, model, src := none.ResolveLiteLLMRemoteRoute("litellm", "m"); ep != "" || model != "m" || src != LiteLLMRouteNone {
		t.Fatalf("none: got (%q, %q, %v)", ep, model, src)
	}
	if none.ExplicitLiteLLMGateway("litellm") != nil {
		t.Fatal("no gateways → nil")
	}
}

func TestLiteLLMChatCompletionsURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://gw.example":     "https://gw.example/v1/chat/completions",
		"https://gw.example/":    "https://gw.example/v1/chat/completions",
		"https://gw.example/v1":  "https://gw.example/v1/chat/completions",
		"https://gw.example/v1/": "https://gw.example/v1/chat/completions",
	} {
		if got := LiteLLMChatCompletionsURL(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
	if LocalLiteLLMProxyURL() != "http://127.0.0.1:18445" {
		t.Fatal(LocalLiteLLMProxyURL())
	}
}
