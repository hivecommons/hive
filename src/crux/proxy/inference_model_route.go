package proxy

import "strings"

// routeWithEntitledModel returns the route to actually forward with: when the
// gateway's entitled model set is known (key-info probe or a prior team-scope
// 403) and the route's stored model differs from an entitled id only by
// provider-prefix / dot-hyphen spelling drift, a COPY of the route carrying
// the exact entitled id is returned. LiteLLM matches the request model string
// verbatim, so without this a stored copilot-spelled id (e.g.
// "claude-sonnet-4.6" vs entitled "aws/claude-sonnet-4-6") 403s on every call
// forever (#4400) — while the dashboard's tolerant preselect matcher shows the
// agent on a perfectly valid model. Self-healing: the first 403 teaches the
// entitled set (recordInferenceError), so the next request resolves.
//
// When the stored model is NOT entitled at all — verbatim or by drift — and
// the backend's DefaultModel IS, the request goes out for the default instead
// (hivecommons/hive#9945). The FMA hive's agents carried a `model:
// claude-sonnet-4-6` written for their original copilot backend into a
// switch to the litellm gateway whose default_model was the only model the
// team could use; the per-agent value won, and every call 403'd forever with
// "team not allowed to access model" while the operator stared at a gateway
// card showing the right default. The gateway's own entitlement signal (the
// key-info probe or that very 403) is what authorises the substitution, so a
// model the team can't use is never sent when one it can use is configured.
// The stored route is still untouched: a key/team change that re-entitles the
// per-agent model takes effect on the next request.
//
// The stored route is never mutated: the rewrite is per-request, so a
// key/team change that re-entitles the verbatim id takes effect immediately,
// and concurrent requests race on nothing. Unknown entitlements, verbatim
// matches, and ambiguous candidates all pass the route through unchanged.
func (p *GitHubProxy) routeWithEntitledModel(route *InferenceRoute, agentName string) *InferenceRoute {
	// A blank per-agent selection inherits the configured backend default,
	// even before an entitlement probe has completed. Keep the stored snapshot
	// untouched, as with entitlement rewrites below.
	if route != nil && strings.TrimSpace(route.Model) == "" && strings.TrimSpace(route.DefaultModel) != "" {
		rewritten := *route
		rewritten.Model = route.DefaultModel
		route = &rewritten
	}
	if route == nil || route.Backend != "litellm" || p.entitlements == nil {
		return route
	}
	entitled, _, known := p.EntitledModels(route.Endpoint)
	if !known {
		return route
	}
	if entitledModelListed(entitled, route.Model) {
		return route
	}
	if resolved, ok := resolveEntitledModelID(entitled, route.Model); ok {
		p.logger.Info("inference model resolved to entitled gateway id",
			"agent", agentName, "endpoint", route.Endpoint,
			"configured", route.Model, "resolved", resolved)
		rewritten := *route
		rewritten.Model = resolved
		return &rewritten
	}
	fallback := strings.TrimSpace(route.DefaultModel)
	if fallback == "" || fallback == route.Model {
		return route
	}
	if !entitledModelListed(entitled, fallback) {
		resolved, ok := resolveEntitledModelID(entitled, fallback)
		if !ok {
			return route
		}
		fallback = resolved
	}
	p.logger.Warn("inference model not entitled on gateway; sending the backend default model instead",
		"agent", agentName, "backend", route.Backend, "endpoint", route.Endpoint,
		"configured", route.Model, "default", fallback,
		"remedy", "clear the agent's model (or pick one of the gateway's entitled models) so the agent uses the gateway default")
	rewritten := *route
	rewritten.Model = fallback
	return &rewritten
}

// entitledModelListed reports whether model appears verbatim in entitled.
func entitledModelListed(entitled []string, model string) bool {
	for _, id := range entitled {
		if id == model {
			return true
		}
	}
	return false
}
