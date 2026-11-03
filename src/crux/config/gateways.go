package config

import (
	"fmt"
	"strings"
)

// gatewayKind values.
const (
	GatewayKindGroq = "groq"
	GatewayKindVLLM = "vllm"
	GatewayKindLLMD = "llm-d"

	// LegacyLiteLLMGatewayName is the name of the implicit gateway synthesized
	// from the legacy Governor.LiteLLM block when no gateways are configured. It
	// matches the historical "litellm" agent backend so existing agents route
	// unchanged.
	LegacyLiteLLMGatewayName = "litellm"
)

// ResolvedGateways returns the effective gateway list: the explicitly-configured
// Gateways when present, otherwise a single implicit gateway synthesized from the
// legacy LiteLLM block (only if that block has an endpoint). This is what lets a
// hive with no `gateways:` and a classic `litellm:` block keep working, while a
// hive that lists gateways gets exactly those.
func (g AgentRuntimeGovernorConfig) ResolvedGateways() []GatewayConfig {
	if len(g.Gateways) > 0 {
		return g.Gateways
	}
	if g.LiteLLM.Endpoint == "" {
		return nil
	}
	return []GatewayConfig{{
		Name:         LegacyLiteLLMGatewayName,
		Kind:         GatewayKindLiteLLM,
		Endpoint:     g.LiteLLM.Endpoint,
		APIKeyEnv:    g.LiteLLM.APIKeyEnv,
		APIKeyFile:   g.LiteLLM.APIKeyFile,
		DefaultModel: g.LiteLLM.DefaultModel,
		CABundle:     g.LiteLLM.CABundle,
	}}
}

// ResolveGateway looks up a gateway by name in the resolved list. An empty name
// returns the FIRST resolved gateway (the default), so an inference agent that
// names no specific gateway routes through the default one. Returns nil if no
// gateway matches (or none are configured).
func (g AgentRuntimeGovernorConfig) ResolveGateway(name string) *GatewayConfig {
	gws := g.ResolvedGateways()
	if len(gws) == 0 {
		return nil
	}
	if name == "" {
		gw := gws[0]
		return &gw
	}
	for i := range gws {
		if strings.EqualFold(gws[i].Name, name) {
			gw := gws[i]
			return &gw
		}
	}
	return nil
}

// ValidateBackend reports whether backend is a usable agent backend for this
// config, returning a descriptive error naming the supported values when it is
// not. An empty backend is valid (it means "the hive default").
//
// This is the single gate the config write path uses so an unsupported backend
// is refused AT SET TIME with a clear message, instead of being persisted and
// surfacing later as an agent that silently never launches.
func (g AgentRuntimeGovernorConfig) ValidateBackend(backend string) error {
	if backend == "" {
		return nil
	}
	if IsCLIBackend(backend) || IsInferenceBackend(backend) {
		return nil
	}
	for _, gw := range g.ResolvedGateways() {
		if gw.Name != "" && strings.EqualFold(gw.Name, backend) {
			return nil
		}
	}
	var gatewayNames []string
	for _, gw := range g.ResolvedGateways() {
		if gw.Name != "" {
			gatewayNames = append(gatewayNames, gw.Name)
		}
	}
	msg := fmt.Sprintf("unsupported backend %q (supported: %s",
		backend, strings.Join(SupportedBackends(), ", "))
	if len(gatewayNames) > 0 {
		msg += fmt.Sprintf("; or a configured gateway name: %s", strings.Join(gatewayNames, ", "))
	} else {
		msg += "; or the name of a gateway configured under the Model Gateways tab"
	}
	return fmt.Errorf("%s)", msg)
}
