package proxy

// seedContextWindow fills an unset MaxContextLen from a window discovered
// earlier for the same endpoint and model (hivecommons/hive#19913), so the
// agent launched right after its route is set can be told its window. It
// returns a copy when it seeds, never mutating the caller's route, and
// reports whether the route still needs the background probe: true whenever
// the caller gave no window, so a seeded value is refreshed too.
func (ir *inferenceRouter) seedContextWindow(route *InferenceRoute) (*InferenceRoute, bool) {
	if route.MaxContextLen != 0 {
		return route, false
	}
	if cached := ir.cachedContextWindow(route.Endpoint, route.Model); cached > 0 {
		seeded := *route
		seeded.MaxContextLen = cached
		return &seeded, true
	}
	return route, true
}

// InferenceContextWindow returns the context window, in tokens, known for the
// agent's current inference route (discovered from the endpoint's /v1/models
// or seeded from an earlier discovery of the same model) and the route's
// model. 0 and "" when the agent has no route; window 0 when none is known.
// The agent manager passes the window to the Claude CLI at launch, so a
// window first learned by the background probe reaches the agent on its next
// launch (hivecommons/hive#19913).
func (p *GitHubProxy) InferenceContextWindow(agentName string) (window int, model string) {
	if route := p.inference.Get(agentName); route != nil {
		return route.MaxContextLen, route.Model
	}
	return 0, ""
}
