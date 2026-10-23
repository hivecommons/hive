package config

import (
	"strings"
	"testing"
)

func TestResolvedGatewaysFallsBackToLegacyLiteLLM(t *testing.T) {
	var g AgentRuntimeGovernorConfig
	if got := g.ResolvedGateways(); got != nil {
		t.Fatalf("no gateways, no litellm: %v", got)
	}
	g.LiteLLM.Endpoint = "http://litellm:4000"
	g.LiteLLM.DefaultModel = "m"
	gws := g.ResolvedGateways()
	if len(gws) != 1 || gws[0].Name != LegacyLiteLLMGatewayName || gws[0].Kind != GatewayKindLiteLLM || gws[0].DefaultModel != "m" {
		t.Fatalf("synthesized gateway = %+v", gws)
	}
	g.Gateways = []GatewayConfig{{Name: "or", Kind: GatewayKindOpenRouter}}
	if gws := g.ResolvedGateways(); len(gws) != 1 || gws[0].Name != "or" {
		t.Fatalf("explicit gateways win: %+v", gws)
	}
}

func TestResolveGateway(t *testing.T) {
	var g AgentRuntimeGovernorConfig
	if g.ResolveGateway("") != nil {
		t.Fatal("nothing configured: want nil")
	}
	g.Gateways = []GatewayConfig{{Name: "first"}, {Name: "Second"}}
	if gw := g.ResolveGateway(""); gw == nil || gw.Name != "first" {
		t.Fatalf("empty name = %+v, want the default", gw)
	}
	if gw := g.ResolveGateway("second"); gw == nil || gw.Name != "Second" {
		t.Fatalf("case-insensitive lookup = %+v", gw)
	}
	if g.ResolveGateway("third") != nil {
		t.Fatal("unknown name: want nil")
	}
}

func TestValidateBackend(t *testing.T) {
	var g AgentRuntimeGovernorConfig
	for _, ok := range []string{"", "claude", "litellm"} {
		if err := g.ValidateBackend(ok); err != nil {
			t.Fatalf("%q: %v", ok, err)
		}
	}
	err := g.ValidateBackend("nope")
	if err == nil || !strings.Contains(err.Error(), "Model Gateways tab") {
		t.Fatalf("no gateways: %v", err)
	}
	g.Gateways = []GatewayConfig{{Name: "Corp"}, {Name: ""}}
	if err := g.ValidateBackend("corp"); err != nil {
		t.Fatalf("gateway name: %v", err)
	}
	err = g.ValidateBackend("nope")
	if err == nil || !strings.Contains(err.Error(), "configured gateway name: Corp") {
		t.Fatalf("with gateways: %v", err)
	}
}
