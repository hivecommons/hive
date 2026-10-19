package stageengine

import (
	"fmt"
	"strings"
)

// Mode is how the reconciler treats a Decision. Config key
// governor.labels.stage.engine.mode (short name stage_engine.mode).
type Mode string

const (
	// ModeOff keeps the legacy derivation paths; Reconcile does nothing.
	// Exists only for rollback.
	ModeOff Mode = "off"
	// ModeShadow derives, diffs against Options.Legacy, records the
	// mismatch and never writes the engine's result. When Legacy and a
	// Writer are both configured the legacy result is written, so a sweep
	// routed through the reconciler keeps today's labels.
	ModeShadow Mode = "shadow"
	// ModeEnforce derives and writes the engine's result.
	ModeEnforce Mode = "enforce"
)

// DefaultMode is the mode a zero Options gets: shadow until the plan's
// acceptance metrics are met.
const DefaultMode = ModeShadow

// Valid reports whether m is one of the three modes.
func (m Mode) Valid() bool {
	switch m {
	case ModeOff, ModeShadow, ModeEnforce:
		return true
	}
	return false
}

// String is the config spelling.
func (m Mode) String() string { return string(m) }

// ParseMode reads a config value; "" yields DefaultMode.
func ParseMode(s string) (Mode, error) {
	m := Mode(strings.ToLower(strings.TrimSpace(s)))
	if m == "" {
		return DefaultMode, nil
	}
	if !m.Valid() {
		return "", fmt.Errorf("stage engine mode %q: want off, shadow or enforce", s)
	}
	return m, nil
}
