package config

import "strings"

// Experimenter role and engine names (RFC #11585 part 5, #11698). See
// pkg/config/nous.go for the dispatcher settings and the engine contract.
const (
	// RoleExperimenter is the agent role name. An agent named "experimenter"
	// gets it by the usual role-defaults-to-name rule; any other name may
	// opt in with `role: experimenter`.
	RoleExperimenter = "experimenter"

	// ExperimentEngineNous names the Nous engine.
	ExperimentEngineNous = "nous"

	// DefaultExperimentEngine is what an experimenter runs when
	// `engine:` is absent: a hive with no engine configured runs Nous.
	DefaultExperimentEngine = ExperimentEngineNous
)

// IsExperimenter reports whether this agent carries the experimenter role.
func (a *AgentConfig) IsExperimenter() bool {
	return a != nil && a.Role == RoleExperimenter
}

// ExperimentEngine returns the engine this agent runs, or "" when it is not
// an experimenter. Load-time defaults fill an experimenter's empty Engine
// with DefaultExperimentEngine; this accessor applies the same fallback for
// a directly constructed AgentConfig.
func (a *AgentConfig) ExperimentEngine() string {
	if !a.IsExperimenter() {
		return ""
	}
	if e := strings.ToLower(strings.TrimSpace(a.Engine)); e != "" {
		return e
	}
	return DefaultExperimentEngine
}
