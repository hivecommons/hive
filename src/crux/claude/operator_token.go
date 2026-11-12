package claude

import (
	"os"
	"strings"
)

const (
	// OperatorTokenEnvVar holds an operator-generated long-lived token
	// (`claude setup-token`). When set, every claude-backend agent runs on it
	// through CLAUDE_CODE_OAUTH_TOKEN instead of the dashboard sign-in.
	OperatorTokenEnvVar = "HIVE_CLAUDE_OAUTH_TOKEN"

	// OperatorTokenFileEnvVar names a file holding the same token, for
	// deployments that mount it as a secret rather than an env var.
	OperatorTokenFileEnvVar = "HIVE_CLAUDE_OAUTH_TOKEN_FILE"
)

// OperatorTokenFile is the mounted Secret key read when neither variable is
// set, mirroring the dashboard token's /secrets/dashboard-token. A var so tests
// can point it at a hermetic path.
var OperatorTokenFile = "/secrets/claude-oauth-token"

// OperatorToken returns the operator-supplied long-lived Claude token, or ""
// when none is configured. The env var wins over the file. The value is a
// secret: callers must only hand it to an agent as a secret env pair.
func OperatorToken() string {
	if v := strings.TrimSpace(os.Getenv(OperatorTokenEnvVar)); v != "" {
		return v
	}
	path := strings.TrimSpace(os.Getenv(OperatorTokenFileEnvVar))
	if path == "" {
		path = OperatorTokenFile
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// Claude auth modes, as the dashboard status and the admin MCP
// claude_auth_status read report them. Neither names any token material.
const (
	AuthModeLongLivedToken  = "long-lived token"
	AuthModeDashboardSignIn = "dashboard sign-in"
)

// TokenRejectedMessage is what an operator sees when Claude answers 401 while
// agents run on the long-lived token. `/login` cannot fix that: the token
// itself was revoked or has expired, so the fix is a new one.
const TokenRejectedMessage = "Claude token rejected: generate a new one with claude setup-token"

// AuthMode reports which Claude auth mode is active: the operator's
// long-lived token when one is configured, otherwise the dashboard sign-in.
func AuthMode() string {
	if OperatorToken() != "" {
		return AuthModeLongLivedToken
	}
	return AuthModeDashboardSignIn
}
