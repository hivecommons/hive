package proxy

import "net/http"

// recordInferenceResponseError logs an upstream inference error response and
// records it against the route's gateway health.
func (p *GitHubProxy) recordInferenceResponseError(route *InferenceRoute, agentName, upstreamURL string, resp *http.Response, body []byte) {
	// recordInferenceError already emits the operator-action auth warning for
	// a 401. The generic log would file a second fleet report for the same rejection.
	if !isInferenceAuthStatus(resp.StatusCode) {
		p.logger.Error("inference upstream error",
			"agent", agentName,
			"status", resp.StatusCode,
			"url", upstreamURL,
			"server", resp.Header.Get("Server"),
			"body", truncateBytes(body, 500),
		)
	}
	p.recordInferenceError(route, agentName, resp.StatusCode, body)
}
