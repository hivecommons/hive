package config

// DefaultCoverageTarget is the coverage goal (percent) used when
// governor.coverage_target is unset: the hivecommons/hive project's own gate.
const DefaultCoverageTarget = 91

// ValidateCoverageTarget reports whether v is an accepted coverage goal: 0
// means unset and 1–100 are explicit percentages.
func ValidateCoverageTarget(v int) bool {
	return v == 0 || (v >= 1 && v <= 100)
}

// EffectiveCoverageTarget returns the configured coverage goal, falling back to
// DefaultCoverageTarget when unset or out of range. Resolved lazily rather than
// mutating config so saved configs do not materialize defaults.
func (g GovernorConfig) EffectiveCoverageTarget() int {
	if g.CoverageTarget >= 1 && g.CoverageTarget <= 100 {
		return g.CoverageTarget
	}
	return DefaultCoverageTarget
}
