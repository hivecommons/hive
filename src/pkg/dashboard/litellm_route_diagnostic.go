package dashboard

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// Test Connection probes an endpoint + key the operator points it at. Agents,
// however, send inference wherever the ROUTE RESOLVER says — and on the FMA
// hive those two disagreed: the Model Gateways tab tested gateway "litellm"
// (new endpoint, new key) and reported "Connection OK: 2 models available",
// while backend "litellm" routed to the stale legacy governor.litellm.endpoint
// with the new key and every agent call died with LiteLLM's 401 "Invalid proxy
// server token" (hivecommons/hive#9945). The resolver bug is fixed in
// config.GovernorConfig.ResolveLiteLLMRemoteRoute; this file makes the route VISIBLE in the Test output so any
// future divergence between "what Test checked" and "what agents will do" is
// stated in the result rather than discovered from a kicked agent's terminal.

// legacyLiteLLMSettingsLocation is where the dashboard edits the legacy
// governor.litellm block. Every message that names the "legacy endpoint" says
// where to change it — the standalone LiteLLM tab these messages used to cite
// no longer exists, which left operators unable to act on them (#9945).
const legacyLiteLLMSettingsLocation = "Settings → Model Gateways → Legacy LiteLLM endpoint"

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
		ep, m, src := resolveLiteLLMRouteForDiagnostic(cfg, backend)
		if ep == "" {
			return fmt.Sprintf("Inference route for backend %q: NONE — no endpoint resolves from a gateway named %q, the legacy endpoint governor.litellm.endpoint (%s) or %s; agents on this backend fail with \"502 no inference route\".",
				backend, backend, legacyLiteLLMSettingsLocation, config.LiteLLMEndpointEnv)
		}
		endpoint, model, source = ep, m, src
		key = cfg.Governor.ResolveLiteLLMInferenceKey(backend)
	default:
		gw := cfg.Governor.ResolveGateway(backend)
		if gw == nil || strings.TrimSpace(gw.Endpoint) == "" {
			return ""
		}
		endpoint, model, key = strings.TrimSpace(gw.Endpoint), gw.DefaultModel, gw.ResolveAPIKey()
		source = fmt.Sprintf("gateway %q (Model Gateways tab)", gw.Name)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Agents on backend %q send inference to POST %s", backend, config.LiteLLMChatCompletionsURL(endpoint))
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
			" Make gateway %q (Settings → Model Gateways), the legacy endpoint governor.litellm.endpoint (%s) and %s agree (or clear the one no longer in use), then re-test.",
			strings.TrimSpace(testedEndpoint), config.GatewayKindLiteLLM, legacyLiteLLMSettingsLocation, config.LiteLLMEndpointEnv)
	}
	if testedKey != "" && key != "" && testedKey != key {
		fmt.Fprintf(&b, " ⚠ The key agents send (SHA256 %s) is NOT the key this Test just used (SHA256 %s); check which key store each side reads.",
			shortKeyHash(config.APIKeySHA256(key)), shortKeyHash(config.APIKeySHA256(testedKey)))
	}
	return b.String()
}

// resolveLiteLLMRouteForDiagnostic mirrors inference.ResolveLiteLLMRoute's
// precedence exactly — local_proxy, then config.ResolveLiteLLMRemoteRoute —
// and names the source it chose. pkg/dashboard deliberately does not import
// pkg/inference (import ratchet), so the local-proxy step is restated here;
// the remote decision is the shared config method, not a copy.
func resolveLiteLLMRouteForDiagnostic(cfg *config.Config, backend string) (endpoint, model, source string) {
	lc := cfg.Governor.LiteLLM
	if lc.LocalProxy {
		return config.LocalLiteLLMProxyURL(), lc.DefaultModel, "governor.litellm.local_proxy (bundled local LiteLLM)"
	}
	ep, m, src := cfg.Governor.ResolveLiteLLMRemoteRoute(backend, "")
	switch src {
	case config.LiteLLMRouteGateway:
		gw := cfg.Governor.ExplicitLiteLLMGateway(backend)
		return ep, m, fmt.Sprintf("gateway %q (Model Gateways tab)", gw.Name)
	case config.LiteLLMRouteLegacyEnv:
		return ep, m, "the " + config.LiteLLMEndpointEnv + " environment variable (overrides governor.litellm.endpoint)"
	case config.LiteLLMRouteLegacyYAML:
		return ep, m, "the legacy governor.litellm block (" + legacyLiteLLMSettingsLocation + ")"
	}
	return "", "", ""
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

// appendInferenceRouteForForm adds the route line to a probe run from the
// UNSAVED gateway form (discover). The form's endpoint and key are not yet
// what agents use, so no divergence warning is raised; the line simply shows
// the route in force right now, identical to what the saved-gateway Test
// prints for the same configuration.
func (s *Server) appendInferenceRouteForForm(gw config.GatewayConfig, probe map[string]interface{}) {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return
	}
	line := inferenceRouteDiagnostic(s.deps.Config, gw.Name, "", "")
	if line == "" {
		return
	}
	text, _ := probe["diagnostic"].(string)
	probe["diagnostic"] = text + "\n" + line
	probe["route"] = line
}
