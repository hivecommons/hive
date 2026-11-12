package config

import (
	"fmt"
	"strings"
)

// governor.reviewer_backend values: how the one-shot reviewer turns (the
// "Let hive decide" decider and the dashboard's grounded chat answers) reach
// a model.
const (
	// ReviewerBackendAuto (the default) uses the OpenAI-compatible reviewer
	// endpoint when one resolves, and otherwise the Claude CLI when the
	// hive's Claude login is present.
	ReviewerBackendAuto = "auto"
	// ReviewerBackendHTTP uses only the reviewer endpoint
	// (governor.trajectory or governor.litellm). No CLI fallback.
	ReviewerBackendHTTP = "http"
	// ReviewerBackendClaudeCLI runs the turn through the Claude CLI on the
	// hive's own Claude login, with every tool off, even when an endpoint is
	// configured.
	ReviewerBackendClaudeCLI = "claude-cli"
)

// Reviewer turn paths, as ResolveReviewerTurn reports them and the dashboard
// shows them.
const (
	ReviewerPathHTTP      = "http"
	ReviewerPathClaudeCLI = "claude-cli"
	ReviewerPathNone      = "none"
)

// ReviewerBackends lists the accepted governor.reviewer_backend values.
func ReviewerBackends() []string {
	return []string{ReviewerBackendAuto, ReviewerBackendHTTP, ReviewerBackendClaudeCLI}
}

// ReviewerBackendOrDefault returns governor.reviewer_backend, trimmed and
// lowercased, with empty meaning auto.
func (g *GovernorConfig) ReviewerBackendOrDefault() string {
	b := strings.ToLower(strings.TrimSpace(g.ReviewerBackend))
	if b == "" {
		return ReviewerBackendAuto
	}
	return b
}

// validateReviewerBackend refuses a governor.reviewer_backend value that
// names no path.
func (g *GovernorConfig) validateReviewerBackend() error {
	b := g.ReviewerBackendOrDefault()
	for _, ok := range ReviewerBackends() {
		if b == ok {
			return nil
		}
	}
	return fmt.Errorf("governor.reviewer_backend must be one of %s, got %q", strings.Join(ReviewerBackends(), ", "), g.ReviewerBackend)
}

// ReviewerTurnRoute is where one reviewer turn goes. Path is http,
// claude-cli or none. Ready is false when the turn cannot run, and Reason
// then says why in words an operator can act on. Endpoint and APIKey are set
// only for the http path; Model is the model the turn asks for (empty on the
// claude-cli path means the CLI's own default model).
type ReviewerTurnRoute struct {
	Path     string
	Ready    bool
	Reason   string
	Endpoint string
	APIKey   string
	Model    string
}

// ResolveReviewerTurn picks the path for a one-shot reviewer turn.
// modelOverride is the feature's own model (decisions.delegate.llm.model,
// chat.grounded_answers.model); empty keeps the reviewer model.
// claudeSignedIn says whether the hive's Claude login is present (the
// operator's long-lived token or the dashboard sign-in), which the caller
// reads the same way the dashboard's Claude sign-in status does.
//
// With an endpoint resolved, auto takes the HTTP path exactly as before this
// setting existed. Only without one does it fall back to the Claude CLI. An
// explicit http never falls back, and an explicit claude-cli never uses the
// endpoint.
func (g *GovernorConfig) ResolveReviewerTurn(modelOverride string, claudeSignedIn bool) ReviewerTurnRoute {
	return g.ResolveReviewerTurnVia(g.ReviewerBackendOrDefault(), modelOverride, claudeSignedIn)
}

// ResolveReviewerTurnVia is ResolveReviewerTurn with the backend given
// instead of read from governor.reviewer_backend, for a turn that only
// supports one path (the workflow-spec refinement is HTTP only).
func (g *GovernorConfig) ResolveReviewerTurnVia(backend, modelOverride string, claudeSignedIn bool) ReviewerTurnRoute {
	endpoint, apiKey, model := g.ResolveReviewer()
	if m := strings.TrimSpace(modelOverride); m != "" {
		model = m
	}
	model = strings.TrimSpace(model)
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")

	httpRoute := func() ReviewerTurnRoute {
		r := ReviewerTurnRoute{Path: ReviewerPathHTTP, Endpoint: endpoint, APIKey: apiKey, Model: model}
		switch {
		case endpoint == "":
			r.Path, r.Endpoint, r.APIKey = ReviewerPathNone, "", ""
			r.Reason = "no reviewer endpoint is configured (governor.trajectory.endpoint or governor.litellm)"
		case model == "":
			r.Reason = "the reviewer endpoint has no model (set governor.trajectory.model or governor.litellm.default_model, or the feature's own model)"
		default:
			r.Ready = true
		}
		return r
	}
	cliRoute := func() ReviewerTurnRoute {
		r := ReviewerTurnRoute{Path: ReviewerPathClaudeCLI, Model: model, Ready: claudeSignedIn}
		if !claudeSignedIn {
			r.Reason = "Claude is not signed in on this hive (sign in from the dashboard, or set HIVE_CLAUDE_OAUTH_TOKEN), so the Claude CLI cannot run the turn"
		}
		return r
	}

	switch backend {
	case ReviewerBackendHTTP:
		return httpRoute()
	case ReviewerBackendClaudeCLI:
		return cliRoute()
	case ReviewerBackendAuto:
		if endpoint != "" {
			return httpRoute()
		}
		if claudeSignedIn {
			return cliRoute()
		}
		return ReviewerTurnRoute{Path: ReviewerPathNone, Model: model,
			Reason: "no reviewer endpoint is configured (governor.trajectory or governor.litellm) and Claude is not signed in, so there is no model to ask"}
	default:
		return ReviewerTurnRoute{Path: ReviewerPathNone, Model: model,
			Reason: fmt.Sprintf("governor.reviewer_backend %q is not one of %s", backend, strings.Join(ReviewerBackends(), ", "))}
	}
}
