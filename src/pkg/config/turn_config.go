package config

import (
	"os"
	"strings"
	"time"
)

// TurnConfig groups opt-in gates for the RFC #4002 re-entrant turn rollout.
type TurnConfig struct {
	Reentrant ReentrantTurnConfig `yaml:"reentrant,omitempty" json:"reentrant,omitempty"`
	// PRFollowUp routes follow-up events on an agent's own PR (CI failure,
	// changes requested, new comments) back into the CLI session that opened
	// the PR instead of a fresh, context-cleared kick (hivecommons/hive#9583).
	PRFollowUp PRFollowUpConfig `yaml:"pr_follow_up,omitempty" json:"pr_follow_up,omitempty"`
}

// PRFollowUpConfig is the opt-in surface for PR follow-up session resume.
type PRFollowUpConfig struct {
	// Enabled turns the feature on. Default false: PR follow-ups reach the
	// authoring agent only through the existing fix-before-new blocks, exactly
	// as before.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// MaxAge bounds how long after a PR opens its authoring session may still
	// be resumed (Go duration, e.g. "12h"). Empty or invalid means
	// DefaultPRFollowUpMaxAge. Older follow-ups fall back to a fresh dispatch.
	MaxAge string `yaml:"max_age,omitempty" json:"max_age,omitempty"`
	// Retention bounds how long a PR's pointer and handoff note are kept at
	// all (Go duration, e.g. "336h"). Pointers are also deleted as soon as
	// their PR merges or closes; this is the backstop for PRs whose end the
	// hive never observes. Empty or invalid means DefaultPRFollowUpRetention.
	Retention string `yaml:"retention,omitempty" json:"retention,omitempty"`
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
	// PRFollowUpResumeEnvVar overrides turn.pr_follow_up.enabled for one
	// process ("true"/"false"); the one-step rollback.
	PRFollowUpResumeEnvVar = "HIVE_PR_FOLLOWUP_RESUME"
	// PRFollowUpMaxAgeEnvVar overrides turn.pr_follow_up.max_age.
	PRFollowUpMaxAgeEnvVar = "HIVE_PR_FOLLOWUP_MAX_AGE"
	// DefaultPRFollowUpMaxAge is how long an authoring session stays eligible
	// for resume after its PR opens. A day covers a normal CI + first-review
	// round; past it the conversation is stale enough that a fresh dispatch
	// rebuilding context from the PR is the better answer.
	DefaultPRFollowUpMaxAge = 24 * time.Hour
	// PRFollowUpRetentionEnvVar overrides turn.pr_follow_up.retention.
	PRFollowUpRetentionEnvVar = "HIVE_PR_FOLLOWUP_RETENTION"
	// DefaultPRFollowUpRetention is how long a pointer (and the handoff note
	// it carries) may live before the sweep deletes it even if the PR's merge
	// or close was never observed. Two weeks matches the audit window the
	// fix-before-new blocks use to attribute a PR to its agent
	// (auditPRAttributionWindow): past it the PR no longer reaches its
	// author through those blocks either.
	DefaultPRFollowUpRetention = 14 * 24 * time.Hour
)

// PRFollowUpResumeEnabled reports whether PR follow-ups should try to resume
// the authoring session. Fail-safe: off unless explicitly enabled.
func (c *Config) PRFollowUpResumeEnabled() bool {
	if v, ok := parseBoolEnv(PRFollowUpResumeEnvVar); ok {
		return v
	}
	if c == nil {
		return false
	}
	return c.Turn.PRFollowUp.Enabled
}

// PRFollowUpMaxAge returns the resume window, falling back to
// DefaultPRFollowUpMaxAge for an unset, unparsable, or non-positive value.
func (c *Config) PRFollowUpMaxAge() time.Duration {
	raw := strings.TrimSpace(os.Getenv(PRFollowUpMaxAgeEnvVar))
	if raw == "" && c != nil {
		raw = strings.TrimSpace(c.Turn.PRFollowUp.MaxAge)
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	return DefaultPRFollowUpMaxAge
}

// PRFollowUpRetention returns how long pointers are kept, falling back to
// DefaultPRFollowUpRetention for an unset, unparsable, or non-positive value.
func (c *Config) PRFollowUpRetention() time.Duration {
	raw := strings.TrimSpace(os.Getenv(PRFollowUpRetentionEnvVar))
	if raw == "" && c != nil {
		raw = strings.TrimSpace(c.Turn.PRFollowUp.Retention)
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	return DefaultPRFollowUpRetention
}

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
