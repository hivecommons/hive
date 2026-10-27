package crux

import (
	"time"

	"github.com/hivecommons/hive/crux/config"
)

// BackendAuth status values. These are the coarse states an operator or the
// hub's fleet aggregator needs to distinguish; the runtime's finer provider
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
	// from an org policy. Reporting "token-expired" here asserts a credential
	// expiry the code never verified and sends an operator to re-login, which
	// cannot fix an entitlement 403, so an unattributable 403 is reported as
	// its own honest state instead of being collapsed into a definitive expiry
	// claim.
	BackendAuthForbidden = "forbidden"
)

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

// interactiveAuthBackends are the CLI backends whose credentials come from an
// INTERACTIVE login flow (OAuth / device flow) that a human must complete. Only
// these can ever legitimately show a login badge.
var interactiveAuthBackends = map[string]bool{
	"claude":  true,
	"copilot": true,
	"codex":   true,
	"gemini":  true,
	"omp":     true,
}

// BackendRequiresInteractiveAuth reports whether a backend authenticates via an
// interactive login the operator must perform. Model-gateway backends
// (config.InferenceBackends: litellm, vllm, llm-d, watsonx) authenticate with
// an API key supplied by config — watsonx with an IAM bearer minted from that
// key — and therefore NEVER require a login. Showing them a 🔑 badge is always
// a false alarm.
func BackendRequiresInteractiveAuth(backend string) bool {
	if config.IsInferenceBackend(backend) {
		return false
	}
	return interactiveAuthBackends[backend]
}
