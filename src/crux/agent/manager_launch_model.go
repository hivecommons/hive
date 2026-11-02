package agent

import (
	"strings"

	"github.com/hivecommons/hive/crux/config"
)

// resolveLaunchModel applies the model fallback chain for one launch:
// agent.model (or its override) → the inference default of the source that
// supplies the agent's endpoint (governor.litellm.default_model / the named
// gateway's default_model, see config.DefaultInferenceModel) → "" (the
// launch builders then omit --model and the backend picks its own default).
//
// Observed live: every agent on a spoke had agents[].model: "" while
// governor.litellm.default_model was set; the default was only ever applied
// by the proxy's outbound rewrite, so the CLI was launched with a bare
// `--model` that swallowed --dangerously-skip-permissions. The fallback is
// returned verbatim because it becomes the gateway-checked "model" field.
func (m *Manager) resolveLaunchModel(agent *AgentProcess, backend, model string, isInference bool) string {
	model = strings.TrimSpace(model)
	if model != "" || !isInference || m.project.InferenceDefaultModel == nil {
		return model
	}
	fallback := strings.TrimSpace(m.project.InferenceDefaultModel(backend))
	if fallback == "" {
		return ""
	}
	if agent.inferenceDefaultModelWarned != fallback {
		agent.inferenceDefaultModelWarned = fallback
		m.logger.Warn("agent has no model configured; launching with the inference default",
			"agent", agent.Name, "backend", backend, "model", fallback,
			"remedy", "set agents."+agent.Name+".model (or the agent's model in the dashboard) to silence this")
	}
	return fallback
}

// dropDanglingModelFlag is the launch-time sanity check behind
// config.ModelFlag: a rendered command must never carry a `--model` whose
// value is the next flag. Operator launch_cmd overrides and any builder that
// slipped past the helper are caught here, logged, and launched without the
// dangling flag rather than with a CLI that silently misparses its argv.
func (m *Manager) dropDanglingModelFlag(agent *AgentProcess, backend, launchCmd string) string {
	cleaned, dropped := config.DropDanglingModelFlag(launchCmd)
	if dropped {
		m.logger.Warn("launch command had a bare --model with no value; dropped it so the next flag is not consumed as the model",
			"agent", agent.Name, "backend", backend, "launch_cmd", launchCmd, "cleaned", cleaned)
	}
	return cleaned
}
