package ioscan

import (
	"fmt"
	"strings"
)

// GateInput is the light integration entry point for the agent-input path
// (e.g. issue/PR/comment text the agent is about to act on). It scans the text
// and, when the block policy trips, returns a non-nil error describing the
// findings so the caller can refuse to proceed. The Verdict is always returned
// so the caller may audit/log even when not blocked.
//
// TODO(ioscan-integration): call this from exactly one place on the agent-input
// path — e.g. where pkg/agent assembles the issue/PR body before dispatching to
// the agent, or from the trajectory reviewer before it forwards external text.
// Keep the wiring to a single call site; the scanner is pure so it is safe to
// invoke inline. Do not deeply rewire the agent loop here.
func GateInput(text string) (Verdict, error) {
	v := ScanInput(text)
	if v.Blocked {
		return v, fmt.Errorf("ioscan: input blocked: %s", summarize(v))
	}
	return v, nil
}

// GateOutput is the light integration entry point for the agent-output path
// (agent-produced text / PR bodies). Semantics mirror GateInput but with the
// stricter output block policy.
//
// TODO(ioscan-integration): call this from exactly one place on the output path
// — e.g. where the gh-wrapper composes a PR/comment body before posting.
func GateOutput(text string) (Verdict, error) {
	v := ScanOutput(text)
	if v.Blocked {
		return v, fmt.Errorf("ioscan: output blocked: %s", summarize(v))
	}
	return v, nil
}

// summarize renders a compact, secret-safe description of a verdict's findings
// for error messages and audit logs.
func summarize(v Verdict) string {
	parts := make([]string, 0, len(v.Findings))
	for _, f := range v.Findings {
		parts = append(parts, fmt.Sprintf("%s(%s)", f.Rule, f.Severity))
	}
	return strings.Join(parts, ", ")
}
