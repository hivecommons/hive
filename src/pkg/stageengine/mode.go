package stageengine

import (
	"fmt"
	"strings"
)

// Mode is how the reconciler treats a Decision. Config key
// governor.labels.stage.engine.mode (short name stage_engine.mode).
type Mode string

const (
	// ModeShadow derives and counts but writes nothing: the dry run. A
	// sweep routed through the reconciler in shadow mode leaves every
	// stage label exactly as it found it.
	ModeShadow Mode = "shadow"
	// ModeEnforce derives and writes the engine's result.
	ModeEnforce Mode = "enforce"
)

// DefaultMode is the mode a zero Options gets (library callers and tests
// that pass no mode): enforce, the same as config.DefaultStageEngineMode.
// Since the legacy derivation path left with plan PR-F the engine is the
// only writer, so a reconciler built without a mode must write.
const DefaultMode = ModeEnforce

// Valid reports whether m is one of the two modes.
func (m Mode) Valid() bool {
	switch m {
	case ModeShadow, ModeEnforce:
		return true
	}
	return false
}

// String is the config spelling.
func (m Mode) String() string { return string(m) }

// ParseMode reads a config value; "" yields DefaultMode. The pre-PR-F
// rollback value "off" is no longer a mode: the legacy pass-through it
// selected is gone, so it is rejected like any other unknown value.
func ParseMode(s string) (Mode, error) {
	m := Mode(strings.ToLower(strings.TrimSpace(s)))
	if m == "" {
		return DefaultMode, nil
	}
	if !m.Valid() {
		return "", fmt.Errorf("stage engine mode %q: want shadow or enforce", s)
	}
	return m, nil
}

// ModeSwitch is the runtime mode control the Reconciler New returns
// implements. cmd/hive flips it when governor.labels.stage.engine.mode
// changes (config reload or a dashboard PUT) so an enforce→shadow pause
// or the reverse needs no restart. SetMode reports whether the mode
// changed; every change is logged once at WARN.
type ModeSwitch interface {
	Mode() Mode
	SetMode(Mode) bool
}
