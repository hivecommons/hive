package config

import "log"

// deprecatedStrategyLabWarned ensures the deprecation notice is logged once
// per process rather than on every applyDefaults call (config is re-applied
// on reload, and a warning per reload would be noise, not signal).
var deprecatedStrategyLabWarned bool

// migrateDeprecatedStrategyLab folds the pre-rename dashboard.strategy_lab key
// into dashboard.experiment_lab (RFC #11585 part 2, #11695).
//
// The Strategy Lab was renamed to Experiment Lab in the UI and docs; the
// maintainer decision on #11585 keeps the old config key as an alias for one
// release. An operator who explicitly enabled dashboard.strategy_lab keeps
// the panel, sees a one-line deprecation in the log, and the next config save
// writes the documented dashboard.experiment_lab key instead. Either key set
// to true enables the panel; the alias can never turn it off.
func (c *Config) migrateDeprecatedStrategyLab() {
	if !c.Dashboard.StrategyLab {
		return
	}
	// Clear unconditionally so the deprecated key does not survive a re-save.
	c.Dashboard.StrategyLab = false
	c.Dashboard.ExperimentLab = true

	if !deprecatedStrategyLabWarned {
		deprecatedStrategyLabWarned = true
		log.Printf("[config] dashboard.strategy_lab is deprecated and will be removed in the next release; " +
			"set dashboard.experiment_lab: true instead (Strategy Lab was renamed Experiment Lab, see #11695).")
	}
}
