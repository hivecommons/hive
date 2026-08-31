package client

import (
	"context"
	"fmt"
	"net/url"
)

// Agent lifecycle states, as returned in AgentActionResult.State.
//
// The enum is transcribed from dashboard/openapi.json (`state` on both
// /api/pause/{agent} and /api/resume/{agent}) and matches the server constants
// pauseStatePaused / pauseStateRunning (pkg/dashboard/api.go:1633). Note the
// asymmetry with Status: the resumed state is "running", not "resumed".
const (
	AgentStatePaused  = "paused"
	AgentStateRunning = "running"
)

// AgentActionResult is the response shared by POST /api/pause/{agent} and
// POST /api/resume/{agent}.
//
// Fields mirror dashboard/openapi.json exactly, and the handler agrees
// field-for-field: pauseToggleResponse (pkg/dashboard/api.go:1651) writes
// precisely ok/status/agent/changed/state and nothing else. Nothing here is
// invented or guessed from a live response.
type AgentActionResult struct {
	// OK is the handler's blanket success flag. It is true on every 200 —
	// including a no-op — so it is NOT the field that tells a caller whether
	// anything happened. Changed is.
	OK bool `json:"ok"`

	// Status names the operation that ran: "paused" or "resumed". It reports
	// which endpoint answered, not the resulting state — a no-op resume returns
	// status "resumed" with state "running". Read State for the state.
	Status string `json:"status"`

	// Agent is the name the server acted on, which is NOT always the string
	// passed in: handlePause/handleResume put the path value through
	// resolveAgentParam (pkg/dashboard/api.go:417), so an agent addressed by ID
	// comes back as its resolved name. A caller that displays what it acted on
	// should display this rather than echoing its own argument.
	Agent string `json:"agent"`

	// Changed distinguishes a real transition from a no-op. Pausing an already
	// paused agent deliberately does NOT re-pause it — that would clobber the
	// original PausedAt/reason/trigger — and reports changed=false instead, so
	// a UI showing a stale "running" can correct itself rather than believe it
	// just paused a running agent. Same in mirror for resume.
	Changed bool `json:"changed"`

	// State is the agent's authoritative state AFTER the call, one of
	// AgentStatePaused or AgentStateRunning. It is populated on the no-op path
	// too, so it is always the value to render — including, and especially,
	// when Changed is false.
	State string `json:"state"`
}

// KickResult is the response from POST /api/kick/{agent}.
//
// It mirrors dashboard/openapi.json exactly. Unlike AgentActionResult, the kick
// response has no resulting lifecycle state.
//
// The endpoint is ASYNCHRONOUS (#5325): it answers 202 once the message is
// queued, so Status is "queued" — or "in-flight" when a delivery for the same
// agent was already running and this call was deduplicated to guarantee the
// prompt is typed exactly once. Neither value asserts the message reached the
// CLI. A definitive precondition failure (unknown, paused, stopped, no tmux
// session) still comes back as an APIError with 400; delivery success or
// failure is read from GET /api/kick/{agent}/status.
type KickResult struct {
	Status string `json:"status"`
	Agent  string `json:"agent"`
}

// Queued reports whether the kick was accepted for delivery by this call, as
// opposed to being folded into a delivery that was already in flight.
func (r KickResult) Queued() bool { return r.Status == "queued" }

// kickRequest is the prompt-bearing form of the optional kick request body.
// The published operation also accepts a legacy message field when prompt is
// empty, but callers of KickAgent supply a prompt, so sending message here
// would duplicate the same value in a lower-precedence field.
type kickRequest struct {
	Prompt string `json:"prompt"`
}

// Paused reports whether the agent is paused after the call, per State.
//
// State is a string on the wire because that is what the spec and handler
// carry; this saves every caller from open-coding the comparison against the
// wrong constant ("resumed", the Status value, is the tempting mistake).
func (r AgentActionResult) Paused() bool { return r.State == AgentStatePaused }

// PauseAgent pauses one agent via POST /api/pause/{agent}.
//
// Pausing an already-paused agent is a no-op, not an error: the call returns
// Changed=false with the current State. Callers should branch on Changed rather
// than assuming a 200 means a transition occurred.
//
// OWNER-ONLY. The handler is gated by requireOwnerRole, so this returns an
// APIError with 403 for a non-owner — use IsForbidden to tell that apart from a
// hive that is merely unreachable. On a self-hosted hive the TUI's
// HIVE_DASHBOARD_TOKEN clears that gate: the local proxy strips client identity
// headers and injects X-Hive-Internal, which authenticate() maps to a verified
// owner (pkg/dashboard/server.go). On a hub-proxied hive the role comes from
// the hub's per-user grant instead, so a read-only user gets a genuine 403.
func (c *Client) PauseAgent(ctx context.Context, agent string) (AgentActionResult, error) {
	return c.agentAction(ctx, "/api/pause/", agent)
}

// ResumeAgent resumes one paused agent via POST /api/resume/{agent}.
//
// The mirror of PauseAgent in every respect: resuming an agent that is not
// paused returns Changed=false with the current State, and the endpoint is
// owner-only.
func (c *Client) ResumeAgent(ctx context.Context, agent string) (AgentActionResult, error) {
	return c.agentAction(ctx, "/api/resume/", agent)
}

// KickAgent sends a prompt to one running agent via POST /api/kick/{agent}.
//
// An empty prompt deliberately sends no request body. That reaches the
// server's auto-generated-message path, which builds a kick from the agent's
// last actionable item. A non-empty prompt is sent in the preferred prompt
// field; the server enforces the operation's 10000-character limit.
func (c *Client) KickAgent(ctx context.Context, agent, prompt string) (KickResult, error) {
	const prefix = "/api/kick/"
	if agent == "" {
		return KickResult{}, fmt.Errorf("POST %s: agent name is required", prefix)
	}

	var body any
	if prompt != "" {
		body = kickRequest{Prompt: prompt}
	}

	var result KickResult
	path := prefix + url.PathEscape(agent)
	if err := c.postJSON(ctx, path, body, &result); err != nil {
		return KickResult{}, err
	}
	return result, nil
}

// agentAction is the shared body of PauseAgent and ResumeAgent, which differ
// only in their path prefix.
func (c *Client) agentAction(ctx context.Context, prefix, agent string) (AgentActionResult, error) {
	if agent == "" {
		// The path parameter is required. An empty name would address
		// "/api/pause/", which matches no route and comes back as a 404 — an
		// error that describes the routing table rather than the mistake. Fail
		// on the caller's actual error instead, and without a network round
		// trip.
		return AgentActionResult{}, fmt.Errorf("POST %s: agent name is required", prefix)
	}
	// PathEscape, not raw interpolation: the name lands in a path segment, and
	// an unescaped separator would silently retarget the request at a different
	// route. Ordinary agent names need no escaping, which is exactly why this
	// has to be deliberate rather than left to the common case.
	var result AgentActionResult
	path := prefix + url.PathEscape(agent)
	if err := c.postJSON(ctx, path, nil, &result); err != nil {
		return AgentActionResult{}, err
	}
	return result, nil
}
