package agent

// opencode's launch command builder. v6 keeps the omp, codex, claude and agy
// builders in manager_launch.go and backend_launch_cmd.go.

// opencodeLaunchCmd starts opencode's TUI for a hive agent. --standalone gives
// each agent its own private server instead of sharing one background service
// across agent UIDs; --auto approves permissions that are not explicitly
// denied, because nobody is attached to an unattended pane to answer a prompt
// (the pod and its egress proxy are the boundary, as for claude and codex). The
// TUI has no --model flag: the model rides in OPENCODE_CONFIG_CONTENT, set by
// agentEnvPairs.
func opencodeLaunchCmd(binary string) string {
	return binary + " --standalone --auto"
}
