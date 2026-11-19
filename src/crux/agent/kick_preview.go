package agent

import "strings"

// canaryPreamblePrefix is how an ioscan exfiltration canary opens a kick
// (hive pkg/ioscan.CanaryPreamblePrefix; the two must stay in step). The
// preamble runs to the first blank line and is about as long as the audit
// preview, so a kick that carried one showed operators nothing but the
// canary (hivecommons/hive#15823). The preview and the kick-history snippet
// start after it; the kick itself is delivered unchanged, canary first.
const canaryPreamblePrefix = "=== HIVE HARNESS HEADER"

// stripCanaryPreamble returns the kick without its canary preamble and true
// when one was present and something followed it. A canary-only kick (or one
// whose preamble is not terminated by a blank line) is returned unchanged
// with false.
func stripCanaryPreamble(message string) (string, bool) {
	if !strings.HasPrefix(message, canaryPreamblePrefix) {
		return message, false
	}
	_, rest, ok := strings.Cut(message, "\n\n")
	if !ok || strings.TrimSpace(rest) == "" {
		return message, false
	}
	return rest, true
}

// kickPreviewBody returns the part of a kick worth previewing: the message
// after its canary preamble, or the whole message when it carries none (or
// nothing follows the preamble, so the canary-only kick stays visible as
// such).
func kickPreviewBody(message string) string {
	body, _ := stripCanaryPreamble(message)
	return body
}
