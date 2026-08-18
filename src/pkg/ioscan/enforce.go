package ioscan

import "strings"

// redactionMarker is the visible annotation substituted for untrusted text that
// the scanner blocked. It is deliberately explicit so an agent reading the kick
// sees that content was withheld (and why, at a rule level) rather than silently
// missing text.
const redactionMarker = "[ioscan: content withheld — %s]"

// EnforceInput is the fail-safe enforcement wrapper for the agent-INPUT path.
// It scans untrusted external text (an issue/PR title or body about to be
// injected into an agent kick) and returns the text that is safe to inject:
//
//   - benign text passes through unchanged;
//   - text the input block policy trips is NOT returned raw — it is replaced
//     with a compact, secret-safe annotation naming the rules that fired, so the
//     agent still learns an item exists but never sees the raw injection payload.
//
// The Verdict is always returned so the caller can audit/log. Unlike
// GateInput, EnforceInput never surfaces an error: enforcement here is about
// producing safe text, and a blocked verdict is handled by redaction, not by
// failing the kick. This keeps the kick loop fail-safe by construction.
func EnforceInput(text string) (sanitized string, v Verdict) {
	v = ScanInput(text)
	if !v.Blocked {
		return text, v
	}
	return annotate(v), v
}

// EnforceOutput is the reusable enforcement wrapper for the agent-OUTPUT path
// (PR-body assembly, gh-wrapper comment/PR bodies, etc.). It scans agent-
// produced text with the stricter output block policy and returns the text that
// is safe to emit:
//
//   - benign text passes through unchanged;
//   - text the output block policy trips (a leaked secret, a dangerous
//     directive, or an injection echo at High+) is replaced with a secret-safe
//     annotation naming the rules that fired, so nothing sensitive is emitted.
//
// The Verdict is always returned for audit. Like EnforceInput it never errors —
// callers get sanitized text and a verdict, and decide whether to additionally
// log/bead the block.
//
// TODO(ioscan-output-wiring): call this from the single place where the
// gh-wrapper composes a PR/comment body before posting (pkg/trajectory PR-body
// assembly / the gh-wrapper concept). The helper is pure and self-contained, so
// wiring it is a one-line `body, _ = ioscan.EnforceOutput(body)` at that site,
// gated on the same ioscan.enabled flag used on the input path.
func EnforceOutput(text string) (sanitized string, v Verdict) {
	v = ScanOutput(text)
	if !v.Blocked {
		return text, v
	}
	return annotate(v), v
}

// annotate renders the redaction marker for a blocked verdict, embedding a
// compact, secret-safe rule summary (never the offending text itself).
func annotate(v Verdict) string {
	return strings.Replace(redactionMarker, "%s", summarize(v), 1)
}
