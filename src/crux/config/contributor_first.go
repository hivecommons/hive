package config

import "time"

// DefaultContributorFirstGrace is how long the governor keeps withholding a
// contributor-first agent's kicks after the last covering relay disappears,
// when the agent sets no contributor_first_grace of its own.
//
// Ten minutes is long enough to ride out a relay reconnect — the hub keeps a
// dropped relay's task lease for thirty minutes precisely because reconnects
// are routine (#4260) — and short enough that a contributor who really has
// gone home does not leave the lane idle for long.
const DefaultContributorFirstGrace = 10 * time.Minute

// ContributorFirstGraceOrDefault resolves contributor_first_grace: a zero
// (absent) value yields DefaultContributorFirstGrace, anything positive is
// returned as-is. Negative values are rejected at load, so this never sees
// one from a loaded config; a caller that assembled an AgentConfig by hand
// gets the default for a negative value rather than a grace in the past.
func (a AgentConfig) ContributorFirstGraceOrDefault() time.Duration {
	if a.ContributorFirstGrace <= 0 {
		return DefaultContributorFirstGrace
	}
	return a.ContributorFirstGrace
}
