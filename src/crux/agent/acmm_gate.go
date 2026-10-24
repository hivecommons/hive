package agent

const operabilityAgentMinACMMLevel = 5

// AgentAvailableAtACMMLevel is the hard ACMM roster gate for agents whose
// existence is level-scoped, not just whose GitHub write authority is scoped.
func AgentAvailableAtACMMLevel(name string, level int) bool {
	switch name {
	case "telemetry", "operations":
		return level >= operabilityAgentMinACMMLevel
	default:
		return true
	}
}
