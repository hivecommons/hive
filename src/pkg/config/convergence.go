package config

import (
	"os"
	"strings"
)

// ConvergenceConfig is the operator-facing feature toggle for Hive's
// convergence-driven admission surfaces (kubestellar/hive#3845).
//
// Everything the convergence follow-on increments add — admission DIAGNOSTICS
// (#4246), internal-kick admission observation (#4247), and the #4263 rollout
// formalisation — ships behind this toggle, DEFAULT OFF, so an existing v4
// hive sees zero behaviour change until an operator opts in. The three modes:
//
//   - "off" (default): no convergence decisions are surfaced or applied by the
//     gated code paths; they are entirely inert. Exact pre-enrollment baseline.
//   - "shadow": decisions are computed and surfaced as read-only diagnostics
//     (API/SSE fields, log lines, soak telemetry) but NEVER enforced — no kick
//     is withheld, no queue row changes, no routing changes. Mutation-boundary
//     claims and journal entries are recorded, and conflicts are logged as
//     would-have-denied only.
//   - "enforce": the SAME decision shadow computes may gate ONLY the explicitly
//     enrolled paths: the #4247 internal scheduled/cached issue-dispatch
//     boundary and the #6056 external mutation boundary. Every path not
//     explicitly enrolled behaves identically in all three modes.
//
// This toggle does NOT govern the contributor-neutral admission already landed
// by #3857/#3904 (ReadyQueue/selectTask dependency gating) — that is existing,
// shipped v4 behaviour and is unchanged in every mode.
type ConvergenceConfig struct {
	// Mode is "off", "shadow", or "enforce". Any other value (including empty)
	// resolves to "off" — the fail-safe direction is always "do nothing new".
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
}

const (
	// ConvergenceModeOff disables every toggle-gated convergence surface.
	ConvergenceModeOff = "off"
	// ConvergenceModeShadow computes and surfaces decisions as diagnostics
	// only; nothing is enforced.
	ConvergenceModeShadow = "shadow"
	// ConvergenceModeEnforce applies the SAME decision shadow computes to the
	// explicitly enrolled internal-dispatch path (#4247's kick boundary) and
	// nothing else. It is never a default and never selected by fallback: only
	// an exact, validated operator choice resolves to it.
	ConvergenceModeEnforce = "enforce"

	// ConvergenceModeEnvVar overrides the configured mode at the process level,
	// so an operator can flip shadow on/off without editing hive.yaml.
	ConvergenceModeEnvVar = "HIVE_CONVERGENCE_MODE"
)

// ConvergenceMode resolves the effective convergence mode: the
// HIVE_CONVERGENCE_MODE environment variable wins when it names a known mode,
// then the configured convergence.mode, and anything unrecognised — a typo, a
// future mode this build does not know, an empty value — resolves to "off".
// Nil-safe: a nil Config is "off".
func (c *Config) ConvergenceMode() string {
	if env, ok := normalizeConvergenceMode(os.Getenv(ConvergenceModeEnvVar)); ok {
		return env
	}
	if c == nil {
		return ConvergenceModeOff
	}
	if mode, ok := normalizeConvergenceMode(c.Convergence.Mode); ok {
		return mode
	}
	return ConvergenceModeOff
}

// ConvergenceModes lists the valid modes in rollout order, for settings UIs
// and validation error messages.
func ConvergenceModes() []string {
	return []string{ConvergenceModeOff, ConvergenceModeShadow, ConvergenceModeEnforce}
}

// NormalizeConvergenceMode maps a raw operator-supplied string to a known mode.
// The second return is false when the value names no known mode. Runtime
// setting writers use it to REJECT an invalid mode before any live mutation or
// persistence; the read path (ConvergenceMode) instead falls through to the
// next source, ultimately "off" — an invalid value can never silently select
// enforcement.
func NormalizeConvergenceMode(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case ConvergenceModeOff:
		return ConvergenceModeOff, true
	case ConvergenceModeShadow:
		return ConvergenceModeShadow, true
	case ConvergenceModeEnforce:
		return ConvergenceModeEnforce, true
	default:
		return "", false
	}
}

func normalizeConvergenceMode(raw string) (string, bool) {
	return NormalizeConvergenceMode(raw)
}
