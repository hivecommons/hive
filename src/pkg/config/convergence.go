package config

import (
	"os"
	"strings"
)

// ConvergenceConfig is the operator-facing feature toggle for Hive's
// convergence-driven admission surfaces (kubestellar/hive#3845).
//
// Everything the convergence follow-on increments add — admission DIAGNOSTICS
// (#4246) and internal-kick admission observation (#4247) — ships behind this
// toggle, DEFAULT OFF, so an existing v4 hive sees zero behaviour change until
// an operator opts in. #4263 will formalise the full off/shadow/enforce
// rollout; until then only the two conservative modes below exist:
//
//   - "off" (default): no convergence decisions are surfaced or applied by the
//     gated code paths; they are entirely inert.
//   - "shadow": decisions are computed and surfaced as read-only diagnostics
//     (API/SSE fields, log lines) but NEVER enforced — no kick is withheld, no
//     queue row changes, no routing changes.
//
// This toggle does NOT govern the contributor-neutral admission already landed
// by #3857/#3904 (ReadyQueue/selectTask dependency gating) — that is existing,
// shipped v4 behaviour and is unchanged in either mode.
type ConvergenceConfig struct {
	// Mode is "off" or "shadow". Any other value (including empty) resolves to
	// "off" — the fail-safe direction is always "do nothing new".
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
}

const (
	// ConvergenceModeOff disables every toggle-gated convergence surface.
	ConvergenceModeOff = "off"
	// ConvergenceModeShadow computes and surfaces decisions as diagnostics
	// only; nothing is enforced.
	ConvergenceModeShadow = "shadow"

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

// normalizeConvergenceMode maps a raw operator-supplied string to a known mode.
// The second return is false when the value names no known mode (callers fall
// through to the next source, ultimately "off").
func normalizeConvergenceMode(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case ConvergenceModeOff:
		return ConvergenceModeOff, true
	case ConvergenceModeShadow:
		return ConvergenceModeShadow, true
	default:
		return "", false
	}
}
