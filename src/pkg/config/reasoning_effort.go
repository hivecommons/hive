package config

import (
	"fmt"
	"strings"
)

// Claude Code's reasoning-effort levels, the closed set `claude --effort`
// accepts (claude --help, v2.1.273+). Named so the launch path, the
// validator and the tests agree on the vocabulary (hivecommons/hive#8377).
const (
	ClaudeEffortLow    = "low"
	ClaudeEffortMedium = "medium"
	ClaudeEffortHigh   = "high"
	ClaudeEffortXHigh  = "xhigh"
	ClaudeEffortMax    = "max"
)

// ClaudeBackend is the backend name of the Claude Code CLI, the only backend
// whose launch command carries `--effort` (hivecommons/hive#8377).
const ClaudeBackend = "claude"

// ReasoningEffortsByBackend lists the reasoning-effort values each CLI
// backend accepts, for the backends that expose an effort control at all:
// codex takes `-c model_reasoning_effort="<v>"`, agy takes `--effort <v>`,
// and claude takes `--effort <v>` (hivecommons/hive#8377). Backends absent
// here have no effort flag; a configured effort is ignored for them rather
// than breaking their launch command.
var ReasoningEffortsByBackend = map[string][]string{
	"codex":       {"minimal", "low", "medium", "high", "xhigh"},
	"agy":         {"low", "medium", "high"},
	ClaudeBackend: {ClaudeEffortLow, ClaudeEffortMedium, ClaudeEffortHigh, ClaudeEffortXHigh, ClaudeEffortMax},
}

// ValidEffort reports whether effort is one of the values backend's effort
// control accepts. An empty effort is never "valid" here — it means unset —
// so callers that want "unset is fine" check for "" first
// (ValidateReasoningEffort does). A backend with no effort control accepts
// nothing.
func ValidEffort(backend, effort string) bool {
	if effort == "" {
		return false
	}
	for _, v := range ReasoningEffortsByBackend[backend] {
		if v == effort {
			return true
		}
	}
	return false
}

// ValidateReasoningEffort reports whether effort is settable for backend:
// empty is always valid (the backend's own default), otherwise the backend
// must have an effort control and the value must be one it accepts. Rejecting
// at set time keeps the failure at the dashboard, not hours later on the kick
// path — the same rationale as ValidateBackend.
func ValidateReasoningEffort(backend, effort string) error {
	if effort == "" {
		return nil
	}
	accepted, ok := ReasoningEffortsByBackend[backend]
	if !ok {
		return fmt.Errorf("backend %s has no reasoning-effort control", backend)
	}
	if ValidEffort(backend, effort) {
		return nil
	}
	return fmt.Errorf("invalid reasoning effort %q for backend %s (accepted: %s)",
		effort, backend, strings.Join(accepted, ", "))
}
