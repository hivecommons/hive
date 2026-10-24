package config

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
	// ResumeIDMaxAge bounds how long a captured backend-native resume id
	// (hivecommons/hive#9606) is still offered to a later session (Go
	// duration, e.g. "72h"). Empty or invalid means
	// DefaultPRFollowUpResumeIDMaxAge. Past it the transcript is assumed
	// gone or too old to be worth reopening, and only the handoff note is
	// handed on.
	ResumeIDMaxAge string `yaml:"resume_id_max_age,omitempty" json:"resume_id_max_age,omitempty"`
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
