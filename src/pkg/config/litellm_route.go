package config

import (
	"strings"
)

// ExplicitLiteLLMGateway returns the gateway from an EXPLICIT `gateways:`
// block that backs backend (matched by name, case-insensitively) when it has
// an endpoint, else nil. The synthetic gateway ResolvedGateways derives from
// the legacy block never qualifies — only an operator-written entry does. This
// is the same test ResolveLiteLLMInferenceKey applies before trusting a
// gateway's key, so endpoint and key can never be read from different places.
func (g GovernorConfig) ExplicitLiteLLMGateway(backend string) *GatewayConfig {
	if len(g.Gateways) == 0 {
		return nil
	}
	gw := g.ResolveGateway(backend)
	if gw == nil || strings.TrimSpace(gw.Endpoint) == "" {
		return nil
	}
	gw.Endpoint = strings.TrimSpace(gw.Endpoint)
	return gw
}

// ResolveLiteLLMRemoteRoute resolves the REMOTE endpoint and default model
// agents on the built-in "litellm" backend send inference to — everything in
// the route decision except governor.litellm.local_proxy, which the inference
// package layers on top because it owns the local proxy's port.
//
// Resolution order:
//
//  1. the EXPLICIT gateway named by this backend, when a `gateways:` block is
//     configured and it carries an endpoint. This MUST win over the legacy
//     block because the KEY already does: ResolveLiteLLMInferenceKey resolves
//     the bearer from that gateway whenever explicit gateways exist. Before
//     this step existed the endpoint came from step 2 first, so a hive whose
//     legacy governor.litellm.endpoint still named an OLD gateway while the
//     Model Gateways tab pointed gateway "litellm" at a NEW one sent the NEW
//     key to the OLD endpoint: the Gateways tab Test passed ("Connection
//     OK"), the hub row flipped to "inference backend auth failed (401):
//     invalid proxy token", and every agent call died with LiteLLM's "Invalid
//     proxy server token" naming the hash of the correct key
//     (hivecommons/hive#9945, FMA hive). Endpoint, default model, key and CA
//     bundle now all come from the one gateway the operator tested.
//  2. the legacy governor.litellm block (HIVE_LITELLM_ENDPOINT, then yaml),
//     whose default_model supplies the model. This is the whole story for
//     hives with no `gateways:` block, and the fallback when the explicit
//     gateway for this backend has no endpoint (#5393 in reverse).
//
// requestedModel is returned as model when non-empty; otherwise the default
// of whichever source supplied the endpoint. source is LiteLLMRouteNone and
// endpoint "" when nothing resolves: the caller must install NO route rather
// than invent one.
func (g GovernorConfig) ResolveLiteLLMRemoteRoute(backend, requestedModel string) (endpoint, model string, source LiteLLMRouteSource) {
	model = requestedModel
	if gw := g.ExplicitLiteLLMGateway(backend); gw != nil {
		if model == "" {
			model = gw.DefaultModel
		}
		return gw.Endpoint, model, LiteLLMRouteGateway
	}
	lc := g.LiteLLM
	endpoint = strings.TrimSpace(lc.ResolveEndpoint())
	if endpoint == "" {
		return "", requestedModel, LiteLLMRouteNone
	}
	if model == "" {
		model = lc.DefaultModel
	}
	if endpoint != strings.TrimSpace(lc.Endpoint) {
		return endpoint, model, LiteLLMRouteLegacyEnv
	}
	return endpoint, model, LiteLLMRouteLegacyYAML
}
