package agent

import (
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/types"
)

// BackendAuth status values live in pkg/crux/types as part of the Crux
// runtime surface (hivecommons/hive#13536); these aliases keep pkg/agent's
// names valid. See types.BackendAuthForbidden for why a bare 403 is its own
// state (#6500).
const (
	BackendAuthOK           = types.BackendAuthOK
	BackendAuthUnlicensed   = types.BackendAuthUnlicensed
	BackendAuthTokenExpired = types.BackendAuthTokenExpired
	BackendAuthUnreachable  = types.BackendAuthUnreachable
	BackendAuthQuota        = types.BackendAuthQuota
	BackendAuthForbidden    = types.BackendAuthForbidden
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

// BackendAuthState is the per-agent backend-auth canary (#6558); see
// types.BackendAuthState.
type BackendAuthState = types.BackendAuthState

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
	case "api_error", "backend_auth_error":
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
