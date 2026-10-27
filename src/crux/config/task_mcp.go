package config

type AgentTaskMCPConfig struct {
	// DropStuffedContext makes task-MCP-enabled kicks render stuffed work
	// lists as refs only. Default false preserves existing prompts until an
	// operator opts one lane in and verifies positive token delta.
	DropStuffedContext bool `yaml:"drop_stuffed_context,omitempty" json:"drop_stuffed_context,omitempty"`
}
