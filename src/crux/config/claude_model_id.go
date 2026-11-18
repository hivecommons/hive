package config

import "strings"

// claudeModelIDPrefix is the family prefix of every concrete Anthropic model
// id. Bare aliases (sonnet, opus, haiku) carry no version and never need
// normalization.
const claudeModelIDPrefix = "claude-"

// CanonicalizeClaudeModel returns the spelling the Claude Code CLI accepts
// for a Claude model id, and whether that differs from the input.
//
// The Claude CLI (and the Anthropic API it calls) spells every version with
// DASHES: claude-sonnet-5-5, claude-opus-4-6. The Copilot CLI spells the same
// models with DOTS (claude-sonnet-5.5, see copilotCLIAcceptedModels in
// crux/copilot_models.go), and so do OpenRouter's catalog ids. A dotted id
// that reaches `claude --model` is rejected outright ("There's an issue with
// the selected model (claude-sonnet-5.5)") and every kick fails.
//
// The dotted form reaches claude agents when an agent that ran on copilot is
// switched to the claude backend: the backend changes, the copilot-spelled
// model is carried over unchanged, and the dashboard's tolerant dropdown
// matcher preselects the dashed option, so nothing on screen looks wrong.
//
// Only an unprefixed claude-* id containing a dot is rewritten. Gateway ids
// with a provider prefix (anthropic/claude-sonnet-5.5) are routed verbatim by
// the gateway and must not be touched; neither are aliases or unknown
// families. Idempotent.
func CanonicalizeClaudeModel(id string) (string, bool) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" || strings.Contains(trimmed, "/") || !strings.Contains(trimmed, ".") {
		return id, false
	}
	if !strings.HasPrefix(strings.ToLower(trimmed), claudeModelIDPrefix) {
		return id, false
	}
	return strings.ReplaceAll(trimmed, ".", "-"), true
}
