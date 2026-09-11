// Package agent: backend_auth.go tracks each agent's backend-auth canary
// state (#6558). #6500 showed a hosted hive whose every agent was dead on
// "You are not licensed to use Copilot" for hours before a human happened to
// open a Terminal and read it — there was no per-agent signal exposed to the
// dashboard or the hub that said "this agent's backend auth is broken" versus
// any other reason a kick might be quiet. This file derives that signal from
// the SAME classifyProviderError verdict the inference-kick watchdog already
// computes (pane_classify.go), so there is exactly one place that decides
// what an agent pane's error text means.
package agent

import (
	"strings"
	"time"
)

// BackendAuth status values. These are the coarse states an operator or the
// hub's fleet aggregator needs to distinguish; classifyProviderError's finer
// error classes (rate_limit, overloaded, retrying, ...) are transient and
// deliberately do NOT change this state — only failures that mean "this
// agent's backend will not authenticate again without an operator" do.
const (
	BackendAuthOK           = "ok"
	BackendAuthUnlicensed   = "unlicensed"
	BackendAuthTokenExpired = "token-expired"
	BackendAuthUnreachable  = "unreachable"
	BackendAuthQuota        = "quota"
)

// backendAuthErrorLimit caps how much of the offending pane line is retained
// in BackendAuthState.LastError, matching the limit classifyProviderError's
// callers already apply to LastError elsewhere.
const backendAuthErrorLimit = 200

// BackendAuthState is the per-agent backend-auth canary (#6558): the spoke's
// own record of whether this agent's inference backend currently
// authenticates, when that became true, and the offending line if not. It is
// exposed verbatim in the per-agent status JSON and in the heartbeat sent to
// the hub. Zero value (Status == "") means "never classified an auth failure"
// — read as ok, identical to an explicit BackendAuthOK.
type BackendAuthState struct {
	Status    string    `json:"status,omitempty"`
	Since     time.Time `json:"since,omitempty"`
	LastError string    `json:"lastError,omitempty"`
}

// classifyBackendAuthStatus maps a classifyProviderError verdict onto the
// coarser BackendAuth states. ok=false means the class is transient (or
// unrecognised) and must NOT change the current BackendAuth state — a
// retryable 5xx or rate limit is not evidence the backend's auth is broken.
func classifyBackendAuthStatus(class, line string) (status string, ok bool) {
	switch class {
	case "auth":
		// classifyProviderError's "auth" class covers both the explicit
		// "not licensed to use copilot" text (a revoked/expired seat — #6500)
		// and a bare 401/403 status. The license wording is unambiguous;
		// everything else in this class is a rejected credential, which reads
		// as an expired token rather than a licensing problem.
		if strings.Contains(strings.ToLower(line), "not licensed") {
			return BackendAuthUnlicensed, true
		}
		return BackendAuthTokenExpired, true
	case "quota":
		return BackendAuthQuota, true
	case "api_error":
		// Covers classifyProviderError's generic api_error class, including
		// the literal "inference backend unreachable" text a self-hosted LLM
		// gateway renders (#6489's shape).
		return BackendAuthUnreachable, true
	default:
		return "", false
	}
}

// markBackendAuthLocked records a backend-auth failure. Called with the same
// lock already held by the provider-error tracking beside it (Manager.mu via
// markProviderErrorLocked), so no separate lock guards BackendAuth.
//
// Since is preserved across repeated observations of the SAME status: it
// marks when the agent FIRST entered this failure state, which is what the
// hub's fleet-wide-down threshold measures. A status change (e.g. quota to
// unlicensed) resets the clock, because it is a different failure.
func (a *AgentProcess) markBackendAuthLocked(status, line string, now time.Time) {
	if a.BackendAuth.Status == status {
		a.BackendAuth.LastError = safeBackendAuthDetail(line)
		return
	}
	a.BackendAuth = BackendAuthState{
		Status:    status,
		Since:     now,
		LastError: safeBackendAuthDetail(line),
	}
}

// clearBackendAuthLocked resets BackendAuth to ok. Called whenever the
// inference-kick watchdog finds no provider error on a pane it just checked
// (clearProviderErrorLocked's caller) — the spoke's only evidence of "the
// backend authenticated on the last check", which is what #6558 asked to
// treat as a successful turn.
func (a *AgentProcess) clearBackendAuthLocked(now time.Time) {
	if a.BackendAuth.Status == "" || a.BackendAuth.Status == BackendAuthOK {
		return
	}
	a.BackendAuth = BackendAuthState{Status: BackendAuthOK, Since: now}
}

func safeBackendAuthDetail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > backendAuthErrorLimit {
		return s[:backendAuthErrorLimit]
	}
	return s
}
