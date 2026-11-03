package config

// ResolveLiteLLMInferenceKey resolves the API key an agent should present when
// its inference routes through the legacy "litellm" backend. It MUST agree with
// the key the entitlement/probe path validates (dashboard gateways.go/cost.go/
// openrouter.go, which use ResolveGateway(name).ResolveAPIKey()) — otherwise a
// key rotation performed via the Model Gateways tab updates only the gateway key
// file, entitlement passes, but inference keeps sending the stale legacy key and
// 401s.
//
// Resolution rule:
//   - When an EXPLICIT `gateways:` block is configured, resolve the key from the
//     gateway matching this backend (its own api_key_file — the file the Model
//     Gateways tab writes), exactly as entitlement does. One source, no drift.
//   - When NO explicit gateways are configured, fall back to the legacy
//     Governor.LiteLLM resolver, which consults the k8s Secret mount and PVC copy
//     in addition to api_key_file. The synthetic gateway from ResolvedGateways
//     lacks that multi-location fallback, so we must not use it here — preserving
//     today's behavior for classic single-`litellm:`-block hives.
//
// backend is the agent's backend name (typically "litellm"); it selects which
// explicit gateway to consult.
func (g GovernorConfig) ResolveLiteLLMInferenceKey(backend string) string {
	if len(g.Gateways) > 0 {
		if gw := g.ResolveGateway(backend); gw != nil {
			return gw.ResolveAPIKey()
		}
	}
	return g.LiteLLM.ResolveAPIKey()
}
