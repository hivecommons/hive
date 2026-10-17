package claims

import "strings"

// ClaimedByPrefix is the namespace of the "who" modifier label,
// `hive/claimed-by-<who>`. The stage family says WHERE an issue is; this
// label says WHO holds it. Its suffix follows ONE rule (hivecommons/hive
// claim-label consistency):
//
//   - a claim with a human principal (KindHuman, KindContributor,
//     KindExternal — every marker whose `who=` is a GitHub login) uses that
//     login, so `hive/claimed-by-clubanderson` whether the person claimed from
//     the dashboard, `/claim`, hivectl or a Copilot CLI / Claude Code session
//     acting on their behalf;
//   - a hive-agent claim (KindAgent) with no human principal uses the agent's
//     name: `hive/claimed-by-scanner`, `hive/claimed-by-contributor`;
//   - a backend / model name (`copilot`, `claude`, `codex`, `pi`, …) is NEVER
//     a suffix: it names the tool, not the party accountable for the claim.
const ClaimedByPrefix = "hive/claimed-by-"

// ClaimedByFallback is the suffix of a hive-agent claim that carries no
// usable agent name (empty, or only a backend name). A claim with a human
// principal has no fallback: an unknown login yields no label at all.
const ClaimedByFallback = "agent"

// backendNames are the coding-tool / model names that must never stand in
// for a claim holder. Compared case-insensitively against the sanitized
// suffix. Kept to names no hive agent or contributor login legitimately
// carries (a person may well be `bob` or `goose`).
var backendNames = map[string]bool{
	"copilot": true,
	"claude":  true,
	"codex":   true,
	"gemini":  true,
	"pi":      true,
}

// IsBackendName reports whether s names a coding backend rather than a
// principal (see ClaimedByPrefix). Leading `@` and surrounding space are
// ignored.
func IsBackendName(s string) bool {
	return backendNames[strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "@"))]
}

// ClaimedBySuffix derives the `hive/claimed-by-` suffix for a holder of the
// given kind. The holder is sanitized to the label-safe alphabet
// ([A-Za-z0-9_-], a leading `@` dropped) and GitHub logins keep their case.
// Session, hive and backend hints are deliberately not inputs: the suffix
// identifies the accountable party, nothing else. A backend name is treated
// as no holder: KindAgent falls back to ClaimedByFallback, every other kind
// yields "" (no label).
func ClaimedBySuffix(holder string, kind Kind) string {
	s := sanitizeClaimedBy(holder)
	if s == "" || IsBackendName(s) {
		if kind == KindAgent {
			return ClaimedByFallback
		}
		return ""
	}
	return s
}

// ClaimedByLabel is ClaimedByPrefix + ClaimedBySuffix for a claim, or ""
// when the claim has no usable holder.
func ClaimedByLabel(c Claim) string {
	s := ClaimedBySuffix(c.Holder, c.Kind)
	if s == "" {
		return ""
	}
	return ClaimedByPrefix + s
}

// IsClaimedByLabel reports whether label is in the hive/claimed-by-
// namespace (case-insensitive on the prefix).
func IsClaimedByLabel(label string) bool {
	return len(label) > len(ClaimedByPrefix) && strings.EqualFold(label[:len(ClaimedByPrefix)], ClaimedByPrefix)
}

// ClaimedByLabelIsBackend reports whether label is a hive/claimed-by- label
// whose suffix is a backend name — the malformed form the reconciler heals.
func ClaimedByLabelIsBackend(label string) bool {
	return IsClaimedByLabel(label) && IsBackendName(label[len(ClaimedByPrefix):])
}

func sanitizeClaimedBy(s string) string {
	s = strings.TrimPrefix(strings.TrimSpace(s), "@")
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
