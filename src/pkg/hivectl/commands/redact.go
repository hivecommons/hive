package commands

import "regexp"

// redactedToken replaces a token-shaped value in any message hivectl prints.
const redactedToken = "[REDACTED]"

// githubTokenShape matches GitHub credential formats: classic and OAuth,
// user-to-server, server-to-server and refresh tokens (gh?_...), and
// fine-grained personal access tokens (github_pat_...).
var githubTokenShape = regexp.MustCompile(`gh[pousr]_[A-Za-z0-9_]+|github_pat_[A-Za-z0-9_]+`)

// RedactTokens replaces every GitHub-token-shaped substring of message. cobra
// and pflag quote the offending argument in their errors ("unknown command
// \"ghp_...\""), so every error hivectl prints passes through here first.
func RedactTokens(message string) string {
	return githubTokenShape.ReplaceAllString(message, redactedToken)
}
