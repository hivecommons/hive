package config

import (
	"os"
	"strings"
)

// TurnConfig groups opt-in gates for the RFC #4002 re-entrant turn rollout.
type TurnConfig struct {
	Reentrant ReentrantTurnConfig `yaml:"reentrant,omitempty" json:"reentrant,omitempty"`
}

// ReentrantTurnConfig is the explicit opt-in surface for the pkg/turn envelope.
type ReentrantTurnConfig struct {
	// Enabled is the global safety catch. Default false means no agent can enter
	// the envelope runner, even if its per-agent reentrant_turn field is true.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// BackgroundFleetEnabled extends opt-in to every enabled background agent
	// once a soak is underway. Default false keeps the rollout to named agents.
	BackgroundFleetEnabled bool `yaml:"background_fleet_enabled,omitempty" json:"background_fleet_enabled,omitempty"`
}

const (
	// ReentrantTurnEnvVar overrides the configured rollout gate for one process.
	ReentrantTurnEnvVar = "HIVE_REENTRANT_TURN_ENABLED"
	// ReentrantTurnBackgroundFleetEnvVar extends the opt-in to the background
	// fleet for one process.
	ReentrantTurnBackgroundFleetEnvVar = "HIVE_REENTRANT_TURN_BACKGROUND_FLEET"
)

// ReentrantTurnEnabled reports whether agent is enrolled in the pkg/turn
// envelope. It is fail-safe: the global gate must be on, and an individual
// agent must explicitly opt in unless the background-fleet gate is on.
func (c *Config) ReentrantTurnEnabled(agent AgentConfig) bool {
	if !c.reentrantTurnGlobalEnabled() {
		return false
	}
	if agent.ReentrantTurn != nil {
		return *agent.ReentrantTurn
	}
	return c.reentrantTurnBackgroundFleetEnabled()
}

func (c *Config) reentrantTurnGlobalEnabled() bool {
	if v, ok := parseBoolEnv(ReentrantTurnEnvVar); ok {
		return v
	}
	if c == nil {
		return false
	}
	return c.Turn.Reentrant.Enabled
}

func (c *Config) reentrantTurnBackgroundFleetEnabled() bool {
	if v, ok := parseBoolEnv(ReentrantTurnBackgroundFleetEnvVar); ok {
		return v
	}
	if c == nil {
		return false
	}
	return c.Turn.Reentrant.BackgroundFleetEnabled
}

func parseBoolEnv(name string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	default:
		return false, false
	}
}
