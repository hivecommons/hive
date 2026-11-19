package config

import (
	"regexp"
	"strings"
)

// CopilotBackend is the backend name of the GitHub Copilot CLI.
const CopilotBackend = "copilot"

// claudeModelIDPrefix is the family prefix of every concrete Anthropic model
// id. Bare aliases (sonnet, opus, haiku) carry no version and never need
// normalization.
const claudeModelIDPrefix = "claude-"

// claudeMinorVersionID matches a Claude id with a MAJOR and MINOR version and
// nothing after it, in either spelling: claude-sonnet-5-5 / claude-sonnet-5.5,
// claude-haiku-4-5 / claude-haiku-4.5. Group 1 is everything up to the major
// version, group 2 the minor. Bare generations (claude-opus-5) and dated or
// suffixed ids (claude-opus-4-5-20251101) do not match: they have no Copilot
// counterpart to map to.
var claudeMinorVersionID = regexp.MustCompile(`^((?i:claude)-[A-Za-z]+-[0-9]+)[.-]([0-9]+)$`)

// CanonicalizeModelForBackend returns the spelling backend's CLI uses for a
// Claude model id, and whether that differs from the input. It is the ONE
// mapping between the two naming conventions for the same Anthropic models:
//
//	claude backend (Claude Code CLI, Anthropic API): DASHED
//	  claude-sonnet-5.5 -> claude-sonnet-5-5, claude-opus-4.6 -> claude-opus-4-6
//	copilot backend (Copilot CLI catalog): DOTTED major.minor
//	  claude-sonnet-5-5 -> claude-sonnet-5.5, claude-haiku-4-5 -> claude-haiku-4.5
//
// Each CLI rejects the other's spelling: `claude --model claude-sonnet-5.5`
// fails every kick ("There's an issue with the selected model"), and the
// Copilot CLI rejects claude-sonnet-5-5 and silently falls back to another
// model (#9927). The wrong spelling reaches an agent when it is switched
// between the two backends and keeps its model.
//
// Everything else passes through unchanged: other backends, non-Claude ids
// (gpt-5.5, gemini-3.6-flash), aliases, bare generations (claude-opus-5) and
// gateway ids with a provider prefix (anthropic/claude-sonnet-5.5), which the
// gateway routes verbatim. Idempotent.
func CanonicalizeModelForBackend(backend, id string) (string, bool) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" || strings.Contains(trimmed, "/") || !strings.HasPrefix(strings.ToLower(trimmed), claudeModelIDPrefix) {
		return id, false
	}
	var canonical string
	switch backend {
	case ClaudeBackend:
		if !strings.Contains(trimmed, ".") {
			return id, false
		}
		canonical = strings.ReplaceAll(trimmed, ".", "-")
	case CopilotBackend:
		m := claudeMinorVersionID.FindStringSubmatch(trimmed)
		if m == nil {
			return id, false
		}
		canonical = m[1] + "." + m[2]
	default:
		return id, false
	}
	if canonical == id {
		return id, false
	}
	return canonical, true
}

// CanonicalizeClaudeModel is CanonicalizeModelForBackend for the claude
// backend.
func CanonicalizeClaudeModel(id string) (string, bool) {
	return CanonicalizeModelForBackend(ClaudeBackend, id)
}
