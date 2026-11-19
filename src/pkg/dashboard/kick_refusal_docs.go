package dashboard

import (
	"net/http"
	"strings"
)

// Kick refusal reasons. Each one has a section (and anchor) in
// src/docs/kick-refusals.md; kickRefusalAnchors is the single map from reason
// to anchor, and TestKickRefusalReasonsHaveDocsAnchors fails when a reason is
// added to kickRefusalReasons without one.
const (
	kickRefusalCheck            = "kick_refusal"
	kickRefusalAgentDeclined    = "agent_declined"
	kickRefusalStartupDeferred  = "startup_deferred"
	kickRefusalPaused           = "paused"
	kickRefusalFailedStart      = "failed_start"
	kickRefusalNotRunning       = "not_running"
	kickRefusalProviderBackoff  = "provider_backoff"
	kickRefusalRestartHold      = "restart_hold"
	kickRefusalRestartCancelled = "restart_cancelled"
	kickRefusalDeliveryBusy     = "delivery_busy"
	kickRefusalNoSession        = "no_session"
	kickRefusalSandboxBlocked   = "sandbox_blocked"
	kickRefusalUnknownAgent     = "unknown_agent"
)

// kickRefusalDocsPage is the published page every kick refusal link targets.
const kickRefusalDocsPage = "https://github.com/hivecommons/hive/blob/v5/src/docs/kick-refusals.md"

// kickRefusalReasons lists every reason the docs page documents.
var kickRefusalReasons = []string{
	kickRefusalCheck,
	kickRefusalAgentDeclined,
	kickRefusalStartupDeferred,
	kickRefusalPaused,
	kickRefusalFailedStart,
	kickRefusalNotRunning,
	kickRefusalProviderBackoff,
	kickRefusalRestartHold,
	kickRefusalRestartCancelled,
	kickRefusalDeliveryBusy,
	kickRefusalNoSession,
	kickRefusalSandboxBlocked,
	kickRefusalUnknownAgent,
}

// kickRefusalAnchors maps each reason to its heading anchor on the docs page.
var kickRefusalAnchors = map[string]string{
	kickRefusalCheck:            "kick_refusal",
	kickRefusalAgentDeclined:    "agent_declined",
	kickRefusalStartupDeferred:  "startup_deferred",
	kickRefusalPaused:           "paused",
	kickRefusalFailedStart:      "failed_start",
	kickRefusalNotRunning:       "not_running",
	kickRefusalProviderBackoff:  "provider_backoff",
	kickRefusalRestartHold:      "restart_hold",
	kickRefusalRestartCancelled: "restart_cancelled",
	kickRefusalDeliveryBusy:     "delivery_busy",
	kickRefusalNoSession:        "no_session",
	kickRefusalSandboxBlocked:   "sandbox_blocked",
	kickRefusalUnknownAgent:     "unknown_agent",
}

// kickRefusalDocsURL returns the docs link for a reason, or "" if unknown.
func kickRefusalDocsURL(reason string) string {
	anchor, ok := kickRefusalAnchors[reason]
	if !ok {
		return ""
	}
	return kickRefusalDocsPage + "#" + anchor
}

// kickRefusalMatchers classify a kick error message; first match wins, so the
// more specific phrases come first.
var kickRefusalMatchers = []struct {
	reason string
	phrase string
}{
	{kickRefusalRestartHold, "kick held after restart"},
	{kickRefusalRestartCancelled, "kick cancelled by restart"},
	{kickRefusalRestartCancelled, "restarted while the kick was waiting"},
	{kickRefusalRestartCancelled, "cancelled: agent restarted"},
	{kickRefusalDeliveryBusy, "agent turn in progress"},
	{kickRefusalDeliveryBusy, "kick delivery already in progress"},
	{kickRefusalProviderBackoff, "blocked: inference"},
	{kickRefusalStartupDeferred, "deferred until startup completes"},
	{kickRefusalSandboxBlocked, "sandbox"},
	{kickRefusalPaused, "it is paused"},
	{kickRefusalFailedStart, "blocked after repeated failed starts"},
	{kickRefusalFailedStart, "it failed to start"},
	{kickRefusalFailedStart, "failed to enter running state"},
	{kickRefusalNotRunning, "it is stopped"},
	{kickRefusalNotRunning, "has not been started yet"},
	{kickRefusalNotRunning, "cannot be kicked: it is in state"},
	{kickRefusalNoSession, "tmux session"},
	{kickRefusalNoSession, "did not become ready"},
	{kickRefusalNoSession, "failed to restart crashed agent"},
	{kickRefusalNoSession, "disappeared"},
	{kickRefusalUnknownAgent, "not found"},
}

// classifyKickError maps a kick delivery error message to a reason, or "".
func classifyKickError(msg string) string {
	lower := strings.ToLower(msg)
	for _, m := range kickRefusalMatchers {
		if strings.Contains(lower, m.phrase) {
			return m.reason
		}
	}
	return ""
}

// classifyKickRefusalFlag maps the agent's KickRefusalReason (set when the
// kick-refused flag is raised) to a reason: either the startup deferral or a
// line the model wrote.
func classifyKickRefusalFlag(reason string) string {
	if classifyKickError(reason) == kickRefusalStartupDeferred {
		return kickRefusalStartupDeferred
	}
	return kickRefusalAgentDeclined
}

// kickErrorDocsURL is the docs link for a kick error message, or "".
func kickErrorDocsURL(msg string) string {
	return kickRefusalDocsURL(classifyKickError(msg))
}

// jsonKickError is jsonError plus a docs_url field when the message matches a
// documented kick refusal reason. Existing clients ignore the extra field.
func jsonKickError(w http.ResponseWriter, msg string, code int) {
	body := map[string]interface{}{"ok": false, "error": msg}
	if u := kickErrorDocsURL(msg); u != "" {
		body["docs_url"] = u
	}
	jsonStatusResponse(w, code, body)
}
