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
//   - "off": no convergence decisions are surfaced or applied by the gated code
//     paths; they are entirely inert. Exact pre-enrollment baseline, and the
//     explicit rollback / control arm for a fixed-commit A/B comparison.
//   - "shadow" (DEFAULT since #7260): decisions are computed and surfaced as
//     read-only diagnostics
//     (API/SSE fields, log lines, soak telemetry) but NEVER enforced — no kick
//     is withheld, no queue row changes, no routing changes.
//   - "enforce": the SAME decision shadow computes may gate ONLY the explicitly
//     enrolled path — the #4247 internal scheduled/cached issue-dispatch
//     boundary. Every path not explicitly enrolled behaves identically in all
//     three modes.
//
// This toggle does NOT govern the contributor-neutral admission already landed
// by #3857/#3904 (ReadyQueue/selectTask dependency gating) — that is existing,
// shipped v4 behaviour and is unchanged in every mode.
type ConvergenceConfig struct {
	// Mode is "off", "shadow", or "enforce". Empty/unset resolves to the
	// DEFAULT, "shadow" (#7260). Any other value -- a typo, a mode from a
	// future build -- still resolves to "off": an unparseable setting must
	// never select a posture the operator did not ask for.
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

	// DefaultConvergenceRolloutMode is the posture a hive that has never
	// touched the knob runs in.
	//
	// It was "off" until #7260. The feature's whole design is "promote to
	// enforce on evidence from your own fleet", but "off" returns before
	// computing anything, so the soak ring stayed empty forever on every hive
	// whose owner never opted in -- which was all of them. A default that
	// cannot produce the evidence its own rollout procedure depends on is not
	// a conservative default, just an inert one.
	//
	// Shadow is safe to default to because it is dispatch-identical to off:
	// applyConvergenceKickAdmission returns the RAW actionable population in
	// shadow, so no kick is withheld and no queue row changes. The added work
	// is a local bead-ledger sweep plus a pure evaluator -- no LLM call, no
	// extra GitHub fetch -- and the telemetry it writes is a bounded ring
	// (convergenceSoakMaxEntries).
	//
	// "off" remains fully selectable and is documented as the rollback /
	// control arm, which is what #4263's fixed-commit A/B comparison needs; it
	// never required off to be the DEFAULT.
	DefaultConvergenceRolloutMode = ConvergenceModeShadow
)

// ConvergenceMode resolves the effective convergence mode: the
// HIVE_CONVERGENCE_MODE environment variable wins when it names a known mode,
// then the configured convergence.mode.
//
// An UNSET mode resolves to DefaultConvergenceRolloutMode ("shadow", #7260).
// A non-empty but unrecognised value — a typo, a mode from a future build —
// resolves to "off", so a setting this build cannot honour can never silently
// select a posture the operator did not ask for. Nil-safe: a nil Config takes
// the default.
func (c *Config) ConvergenceMode() string {
	if env, ok := normalizeConvergenceMode(os.Getenv(ConvergenceModeEnvVar)); ok {
		return env
	}
	if c == nil {
		return DefaultConvergenceRolloutMode
	}
	return ResolveConvergenceMode(c.Convergence.Mode)
}

// ResolveConvergenceMode maps a raw configured mode string to the mode that is
// actually in force, ignoring the environment override.
//
// It is the single source of truth for the #7260 distinction, which callers
// kept getting subtly wrong when they open-coded it: UNSET means "never chose"
// and takes DefaultConvergenceRolloutMode, while a NON-EMPTY value naming no
// known mode means "chose something this build cannot honour" and takes the
// "off" fail-safe. Collapsing the two either strands hives on the inert
// default or lets a typo silently pick a posture nobody asked for.
func ResolveConvergenceMode(raw string) string {
	if mode, ok := normalizeConvergenceMode(raw); ok {
		return mode
	}
	if strings.TrimSpace(raw) == "" {
		return DefaultConvergenceRolloutMode
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
