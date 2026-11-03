package config

import "strings"

// ModelRole is one `model_roles` entry: a named model (and optionally the
// backend and reasoning effort it runs with) that agents and the advisor lane
// reference by `@name` instead of a literal model id. It lives in crux/config
// so the agent runtime can project roles into launch files (the OMP roles
// overlay) without importing the hub's config package; pkg/config aliases it.
type ModelRole struct {
	// Backend the role's model runs on. Optional; when empty the consumer's
	// own backend applies.
	Backend string `yaml:"backend,omitempty" json:"backend,omitempty"`
	// Model is the model id. Required.
	Model string `yaml:"model" json:"model"`
	// ReasoningEffort for backends that expose an effort control. Optional.
	// Not validated when Backend is empty (the consumer's backend applies at
	// launch); for omp it must be one of OMPThinkingLevels; otherwise it is
	// validated against ReasoningEffortsByBackend.
	ReasoningEffort string `yaml:"reasoning_effort,omitempty" json:"reasoning_effort,omitempty"`
}

// AdvisorClaudeBackend is the backend of the Claude Code `Stop` hook adapter
// (phase 1). AdvisorCopilotBackend and AdvisorCodexBackend are the Copilot
// CLI `agentStop` and Codex CLI `Stop` adapters (phase 3, #9724).
const (
	AdvisorClaudeBackend  = "claude"
	AdvisorCopilotBackend = "copilot"
	AdvisorCodexBackend   = "codex"
)

// AdvisorOMPBackend is the backend of the phase-2 adapter (the OMP
// `session_stop` extension and `--config` roles overlay).
const AdvisorOMPBackend = "omp"

// NousEngineEnv is the name the hive exports an experimenter agent's
// resolved engine under.
const NousEngineEnv = "HIVE_NOUS_ENGINE"

// AdvisorSupportedBackendList names the supported backends for operator-facing
// messages.
const AdvisorSupportedBackendList = AdvisorClaudeBackend + ", " + AdvisorOMPBackend + ", " + AdvisorCopilotBackend + ", " + AdvisorCodexBackend

// AdvisorSupportedBackend reports whether the advisor lane has an adapter for
// the backend: the Claude Code, Copilot CLI and Codex CLI turn-end hooks and
// the OMP `session_stop` extension.
func AdvisorSupportedBackend(backend string) bool {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case AdvisorClaudeBackend, AdvisorOMPBackend, AdvisorCopilotBackend, AdvisorCodexBackend:
		return true
	}
	return false
}

// NousDispatcherSDK is the in-process Nous dispatcher; NousDispatcherEnv
// overrides nous.dispatcher at runtime and is also the name the hive exports
// the resolved dispatcher under to experimenter agents.
const (
	NousDispatcherSDK = "sdk"
	NousDispatcherEnv = "HIVE_NOUS_DISPATCHER"
)
