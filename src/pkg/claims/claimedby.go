package claims

import "strings"

// ClaimedByPrefix is the namespace of the "who" modifier label,
// `hive/claimed-by-<who>`. The stage family says WHERE an issue is; this
// label says WHO holds it. Its suffix follows ONE rule (hivecommons/hive
// claim-label consistency; owner decision 2026-10-19: no login in a label):
//
//   - a claim with a human principal (KindHuman, KindContributor,
//     KindExternal — every marker whose `who=` is a GitHub login) uses the
//     generic ClaimedByHuman suffix, `hive/claimed-by-human`, whether the
//     person claimed from the dashboard, `/claim`, hivectl or a Copilot CLI /
//     Claude Code session acting on their behalf. The login lives in the
//     claim record, the `hive:claim who=<login>` marker comment and the
//     dashboard — never in the label, which would otherwise mint one label
//     per person in every repo and leak identities;
//   - a hive-agent claim (KindAgent) with no human principal uses the agent's
//     name: `hive/claimed-by-scanner`, `hive/claimed-by-contributor`;
//   - a backend / model name (`copilot`, `claude`, `codex`, `pi`, …) is NEVER
//     a suffix: it names the tool, not the party accountable for the claim.
//
// The retired per-login spelling `hive/claimed-by-<login>` is still READ as
// a human claim (every consumer matches the prefix) and is swapped for the
// generic label by the claim writers, the issue reconciler and the one-shot
// MigrateClaimedByHumanLabels sweep.
const ClaimedByPrefix = "hive/claimed-by-"

// ClaimedByHuman is the suffix of every claim with a human principal
// (KindHuman, KindContributor, KindExternal). It is also reserved: no hive
// agent may be named "human".
const ClaimedByHuman = "human"

// ClaimedByHumanLabel is the one label a human-principal claim wears.
const ClaimedByHumanLabel = ClaimedByPrefix + ClaimedByHuman

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
// given kind. A hive-agent holder is sanitized to the label-safe alphabet
// ([A-Za-z0-9_-], a leading `@` dropped) and used as-is; every other kind
// yields ClaimedByHuman — the login never reaches the label. Session, hive
// and backend hints are deliberately not inputs: the suffix identifies the
// accountable party, nothing else. A backend name is treated as no holder:
// KindAgent falls back to ClaimedByFallback, every other kind yields ""
// (no label).
func ClaimedBySuffix(holder string, kind Kind) string {
	s := sanitizeClaimedBy(holder)
	if s == "" || IsBackendName(s) {
		if kind == KindAgent {
			return ClaimedByFallback
		}
		return ""
	}
	if kind == KindAgent {
		return s
	}
	return ClaimedByHuman
}

// LegacyClaimedByLabel is the retired per-login spelling,
// `hive/claimed-by-<login>`, for a human-principal holder: what a hive
// before the generic label wrote and what readers must still accept. It is
// "" for an empty or backend-named holder and for a holder whose sanitized
// login would collide with the generic or fallback suffix.
func LegacyClaimedByLabel(holder string) string {
	s := sanitizeClaimedBy(holder)
	if s == "" || IsBackendName(s) || strings.EqualFold(s, ClaimedByHuman) || strings.EqualFold(s, ClaimedByFallback) {
		return ""
	}
	return ClaimedByPrefix + s
}

// ClaimedByLabels lists every label a claim may be wearing: its current
// label and, for a human-principal claim, the retired per-login spelling.
// Release and takeover remove all of them so an issue labelled before the
// upgrade is cleaned up too.
func ClaimedByLabels(c Claim) []string {
	var out []string
	if l := ClaimedByLabel(c); l != "" {
		out = append(out, l)
	}
	if c.Kind != KindAgent {
		if l := LegacyClaimedByLabel(c.Holder); l != "" && !strings.EqualFold(l, ClaimedByLabel(c)) {
			out = append(out, l)
		}
	}
	return out
}

// IsClaimedByHumanLabel reports whether label is the generic human label.
func IsClaimedByHumanLabel(label string) bool {
	return strings.EqualFold(label, ClaimedByHumanLabel)
}

// ClaimedByLabelLogin returns the login a retired per-login label names,
// or "" when label is not in the namespace, is the generic human label,
// the agent fallback or a backend-named label. It cannot tell a login from
// an agent name: callers that need the distinction check the live claim or
// the hive's agent names.
func ClaimedByLabelLogin(label string) string {
	if !IsClaimedByLabel(label) {
		return ""
	}
	suffix := label[len(ClaimedByPrefix):]
	if strings.EqualFold(suffix, ClaimedByHuman) || strings.EqualFold(suffix, ClaimedByFallback) || IsBackendName(suffix) {
		return ""
	}
	return suffix
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
