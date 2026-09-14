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
	// BackendAuthForbidden is an auth-class rejection whose specific cause the
	// pane text does NOT reveal — a bare HTTP 403 with no "not licensed" wording
	// and no credential-rejection wording (#6500). The Copilot enterprise API
	// returns exactly this: its entitlement failure is an HTTP 403 whose body is
	// "unauthorized: not licensed to use Copilot", and when a CLI renders only
	// the bare status the code cannot tell a revoked seat from a stale token
	// from an org policy. Reporting "token-expired" here — as this file did
	// before — asserts a credential expiry the code never verified and sends an
	// operator to re-login, which cannot fix an entitlement 403. That confident
	// wrong verdict IS the "false claim of lack of credentials" this issue is
	// about, so an unattributable 403 is reported as its own honest state
	// instead of being collapsed into a definitive expiry claim.
	BackendAuthForbidden = "forbidden"
)

// credentialRejectionSignals are the substrings in an auth-class pane line that
// DO identify a rejected credential — a 401, an explicit bad/invalid/expired
// credential, or a failed key verification — for which re-login is the correct
// remedy. Their ABSENCE from an auth-class line (e.g. a bare 403 Forbidden)
// means the cause is not verifiable from the pane, and the verdict must stay
// BackendAuthForbidden rather than asserting an expiry (#6500).
var credentialRejectionSignals = []string{
	"401",
	"unauthorized",
	"bad credentials",
	"could not be validated",
	"invalid api key",
	"invalid or expired",
	"api key verification failed",
	"token expired",
	"expired token",
	"expired api key",
}

// lineIndicatesCredentialRejection reports whether an auth-class line carries
// wording that identifies a rejected credential (as opposed to a bare
// authorization denial whose cause is undetermined).
func lineIndicatesCredentialRejection(lower string) bool {
	for _, sig := range credentialRejectionSignals {
		if strings.Contains(lower, sig) {
			return true
		}
	}
	return false
}

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
		// classifyProviderError's "auth" class covers the explicit "not
		// licensed to use copilot" text (a revoked/expired seat — #6500), a
		// bare 401/credential rejection, AND a bare 403 Forbidden whose cause
		// the pane does not spell out. Only the first two justify a definitive
		// verdict:
		//   - "not licensed": an entitlement failure, reported verbatim.
		//   - a recognised credential rejection (401, bad/invalid/expired
		//     credential): an expired/rejected token, for which re-login helps.
		// Anything else in this class — most importantly a bare 403 — is an
		// authorization denial whose cause (revoked seat vs. stale token vs.
		// org policy) is NOT verifiable from the line. Reporting "token-expired"
		// there would assert a credential expiry the code never checked and
		// send the operator to a re-login that cannot fix it, which is the
		// exact false claim in this issue. Report BackendAuthForbidden — an
		// honest "auth denied, cause undetermined" — instead.
		lower := strings.ToLower(line)
		if strings.Contains(lower, "not licensed") {
			return BackendAuthUnlicensed, true
		}
		if lineIndicatesCredentialRejection(lower) {
			return BackendAuthTokenExpired, true
		}
		return BackendAuthForbidden, true
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
