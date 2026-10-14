package dashboard

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/inference"
)

// Test Connection probes an endpoint + key the operator points it at. Agents,
// however, send inference wherever the ROUTE RESOLVER says — and on the FMA
// hive those two disagreed: the Model Gateways tab tested gateway "litellm"
// (new endpoint, new key) and reported "Connection OK: 2 models available",
// while backend "litellm" routed to the stale legacy governor.litellm.endpoint
// with the new key and every agent call died with LiteLLM's 401 "Invalid proxy
// server token" (hivecommons/hive#9945). The resolver bug is fixed in
// pkg/inference; this file makes the route VISIBLE in the Test output so any
// future divergence between "what Test checked" and "what agents will do" is
// stated in the result rather than discovered from a kicked agent's terminal.

// inferenceRouteDiagnostic describes, in one operator-readable line, the
// request agents on backend will actually make: the exact POST URL the
// translator builds, the model, which header carries the key and its SHA-256
// prefix, and which configuration source the route came from. When
// testedEndpoint / testedKey (what the probe just exercised) differ from the
// route, it appends an explicit warning that a passing Test does not prove
// inference works, and says what to reconcile. Returns "" when no route can be
// described (unknown backend) so callers can append it unconditionally.
// Never includes key material — only the hash prefix.
func inferenceRouteDiagnostic(cfg *config.Config, backend, testedEndpoint, testedKey string) string {
	if cfg == nil {
		return ""
	}
	backend = strings.TrimSpace(backend)
	var endpoint, model, key, source string
	switch {
	case backend == "" || strings.EqualFold(backend, config.GatewayKindLiteLLM):
		backend = config.GatewayKindLiteLLM
		ep, m, ok := inference.ResolveLiteLLMRoute(cfg, backend, "")
		if !ok {
			return fmt.Sprintf("Inference route for backend %q: NONE — no endpoint resolves from a gateway named %q, governor.litellm.endpoint or %s; agents on this backend fail with \"502 no inference route\".",
				backend, backend, config.LiteLLMEndpointEnv)
		}
		endpoint, model = ep, m
		key = cfg.Governor.ResolveLiteLLMInferenceKey(backend)
		source = liteLLMRouteSource(cfg, backend)
	default:
		gw := cfg.Governor.ResolveGateway(backend)
		if gw == nil || strings.TrimSpace(gw.Endpoint) == "" {
			return ""
		}
		endpoint, model, key = strings.TrimSpace(gw.Endpoint), gw.DefaultModel, gw.ResolveAPIKey()
		source = fmt.Sprintf("gateway %q (Model Gateways tab)", gw.Name)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Agents on backend %q send inference to POST %s", backend, inference.ChatCompletionsURL(endpoint))
	if model != "" {
		fmt.Fprintf(&b, " (model %s)", model)
	}
	if key == "" {
		b.WriteString(" with no Authorization header")
	} else {
		fmt.Fprintf(&b, " with Authorization: Bearer (key SHA256 %s)", shortKeyHash(config.APIKeySHA256(key)))
	}
	fmt.Fprintf(&b, " — route source: %s.", source)

	if testedEndpoint != "" && !sameGatewayEndpoint(testedEndpoint, endpoint) {
		fmt.Fprintf(&b, " ⚠ That is NOT the endpoint this Test just exercised (%s): a passing Test here does not prove agent inference works."+
			" Make gateway %q, governor.litellm.endpoint and %s agree (or clear the one no longer in use), then re-test.",
			strings.TrimSpace(testedEndpoint), config.GatewayKindLiteLLM, config.LiteLLMEndpointEnv)
	}
	if testedKey != "" && key != "" && testedKey != key {
		fmt.Fprintf(&b, " ⚠ The key agents send (SHA256 %s) is NOT the key this Test just used (SHA256 %s); check which key store each side reads.",
			shortKeyHash(config.APIKeySHA256(key)), shortKeyHash(config.APIKeySHA256(testedKey)))
	}
	return b.String()
}

// liteLLMRouteSource names the configuration source ResolveLiteLLMRoute chose
// for the built-in litellm backend, mirroring its precedence exactly.
func liteLLMRouteSource(cfg *config.Config, backend string) string {
	lc := cfg.Governor.LiteLLM
	if lc.LocalProxy {
		return "governor.litellm.local_proxy (bundled local LiteLLM)"
	}
	if gw := inference.ExplicitLiteLLMGateway(cfg, backend); gw != nil {
		return fmt.Sprintf("gateway %q (Model Gateways tab)", gw.Name)
	}
	if ep := strings.TrimSpace(lc.Endpoint); ep == "" || lc.ResolveEndpoint() != ep {
		return "the " + config.LiteLLMEndpointEnv + " environment variable (overrides governor.litellm.endpoint)"
	}
	return "the legacy governor.litellm block (LiteLLM tab)"
}

// appendInferenceRouteDiagnostic adds the route line to a gateway probe's
// diagnostic text so the Model Gateways tab Test shows, under "Connection OK"
// or "Connection failed", where agents using that gateway will really send
// inference and with which key.
func (s *Server) appendInferenceRouteDiagnostic(gw config.GatewayConfig, probe map[string]interface{}, testedKey string) {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return
	}
	line := inferenceRouteDiagnostic(s.deps.Config, gw.Name, gw.Endpoint, testedKey)
	if line == "" {
		return
	}
	text, _ := probe["diagnostic"].(string)
	probe["diagnostic"] = text + "\n" + line
	probe["route"] = line
}
