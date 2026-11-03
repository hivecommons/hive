package agent

import "github.com/hivecommons/hive/crux"

// AgentAvailableAtACMMLevel is the hard ACMM roster gate for agents whose
// existence is level-scoped, not just whose GitHub write authority is scoped.
// The gate lives in the crux root package (crux.AgentAvailableAtACMMLevel).
func AgentAvailableAtACMMLevel(name string, level int) bool {
	return crux.AgentAvailableAtACMMLevel(name, level)
}
