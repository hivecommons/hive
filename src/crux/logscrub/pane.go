package logscrub

import "regexp"

var statusTokenRedactor = regexp.MustCompile(`(ghp_|gho_|ghs_|ghu_|ghr_|github_pat_)[A-Za-z0-9_]{10,}`)

var statusJWTRedactor = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}`)

var statusBearerRedactor = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{16,}\b`)

var deviceCodeRedactor = regexp.MustCompile(`(?i)(one-time code:\s*)[A-Z0-9]{4}-[A-Z0-9]{4}`)

var deviceCodeLineRedactor = regexp.MustCompile(`(?m)^.*(?:login/device|Waiting for authorization|one-time code:|Press any key to copy).*$`)

// apiKeyRedactor matches sk-prefixed API keys: real Anthropic keys (sk-ant-…),
// the per-agent LiteLLM virtual keys hive itself mints (sk-hive-<agent>, see
// Manager.agentEnv), and the generic OpenAI-style sk-… form.
//
// This surface previously redacted only GitHub tokens, so an agent command line
// carrying ANTHROPIC_API_KEY='sk-hive-…' reached the pane/full-log endpoints in
// clear text (observed in the #3852 report). That was survivable only by
// accident: an 80-column pane frequently clipped the key off the end of the
// line before it was ever captured. Widening the pane for #3878 removes that
// accidental cover and makes the exposure reliable, so the redaction has to
// become real. Truncation is not a security control.
//
// The {6,} floor keeps the pattern off ordinary prose ("sk-" alone) while still
// catching short virtual keys such as sk-hive-qa.
//
// The leading (^|[^A-Za-z0-9._-]) guard anchors the match to a real token
// start. Without it the pattern fires INSIDE ordinary identifiers — a module
// named "task-sk-flow" was rewritten to "task-[redacted]", which would corrupt
// the very tool-call text this change exists to make readable. A redactor that
// eats legitimate output is its own bug, so the boundary is asserted by
// TestRedactTokensPreservesNonSecretText.
var apiKeyRedactor = regexp.MustCompile(`(^|[^A-Za-z0-9._-])(sk-[A-Za-z0-9._\-]{6,})`)

// RedactPaneTokens masks GitHub tokens, sk- API keys and device-login one-time
// codes/lines in text derived from an agent pane.
func RedactPaneTokens(s string) string {
	s = statusJWTRedactor.ReplaceAllString(s, "[REDACTED]")
	s = statusBearerRedactor.ReplaceAllString(s, "[REDACTED]")
	s = statusTokenRedactor.ReplaceAllStringFunc(s, func(m string) string {
		// The pattern guarantees at least 14 characters, so m[:7] is always valid.
		return m[:7] + "***"
	})
	// ${1} preserves the boundary character the pattern had to consume.
	s = apiKeyRedactor.ReplaceAllString(s, "${1}sk-***REDACTED***")
	s = deviceCodeRedactor.ReplaceAllString(s, "${1}****-****")
	s = deviceCodeLineRedactor.ReplaceAllString(s, "[auth flow redacted]")
	return s
}
