package agent

import (
	"log/slog"
	"path"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/sandbox"
)

// SandboxInferenceRelay is the listener a sandbox Job's Claude CLI reaches the
// inference translator through (#19619); see crux.SandboxInferenceRelay.
type SandboxInferenceRelay = crux.SandboxInferenceRelay

type sandboxInferenceRelayInstall struct {
	relay SandboxInferenceRelay
}

// SetSandboxInferenceRelay installs (or, with nil, removes) the listener that
// carries sandbox Job inference to hive's translator (#19619).
func (m *Manager) SetSandboxInferenceRelay(relay SandboxInferenceRelay) {
	if relay == nil {
		m.sandboxReadRelay.inference.Store(nil)
		return
	}
	m.sandboxReadRelay.inference.Store(&sandboxInferenceRelayInstall{relay: relay})
}

// sandboxInferenceRelayLoad returns the installed listener, or nil.
func (m *Manager) sandboxInferenceRelayLoad() SandboxInferenceRelay {
	if in := m.sandboxReadRelay.inference.Load(); in != nil {
		return in.relay
	}
	return nil
}

// routeSandboxInferenceLocked reports whether this kick's Job should reach
// inference through the listener: a Job-runtime kick on a model gateway
// backend while the listener is installed. When it should, the agent's route
// is (re)set on the translator exactly as a tmux launch sets it, because a
// sandboxed agent never passes through that launch path. Caller holds m.mu.
func (m *Manager) routeSandboxInferenceLocked(agent *AgentProcess, runtime string) bool {
	if runtime != config.SandboxRuntimeJob || m.sandboxInferenceRelayLoad() == nil {
		return false
	}
	backend := effectiveBackend(agent)
	if !m.routableBackend(backend) {
		return false
	}
	if m.inferenceRouteCallback != nil {
		model := agent.Config.Model
		if agent.ModelOverride != "" {
			model = agent.ModelOverride
		}
		m.inferenceRouteCallback(agent.Name, backend, m.resolveLaunchModel(agent, backend, model, true))
	}
	return true
}

// sandboxInferenceCARelPath is where the listener's trust bundle is written
// in the kick's workspace; the Job sees it under sandbox.DefaultWorkspaceMount.
const sandboxInferenceCARelPath = ".hive/inference-ca.pem"

// sandboxInferenceEnv points a Job's Claude CLI at the listener. The API key
// is the per-kick key, which only the listener accepts: no gateway key exists
// on this path, and TestSandboxInferenceEnvCarriesOnlyTheKickKey pins that.
func sandboxInferenceEnv(baseURL, kickKey, caPath string) map[string]string {
	env := map[string]string{
		"ANTHROPIC_BASE_URL":    baseURL,
		"ANTHROPIC_API_KEY":     kickKey,
		"NODE_EXTRA_CA_CERTS":   caPath,
		claudeAutoModeServerEnv: claudeAutoModeServerOff,
	}
	for _, v := range inferenceQuietCLIEnv {
		env[v] = "1"
	}
	return env
}

// enrollInferenceRelay admits this kick to the inference listener. A failure
// is logged and the Job runs on whatever model path it had before (for
// example a job.env_from_secrets recipe). The returned release revokes the
// kick key and must be called when the kick ends; it is never nil.
func (e *SandboxExecutor) enrollInferenceRelay(spec SandboxKickSpec, workspace string) (map[string]string, func()) {
	noop := func() {}
	if e.InferenceRelay == nil || spec.Runtime != config.SandboxRuntimeJob || !spec.InferenceRouted {
		return nil, noop
	}
	baseURL, key, bundle, release, err := e.InferenceRelay.Enroll(spec.Agent)
	if err != nil {
		e.warnInferenceRelay(spec.Agent, "enroll", err)
		return nil, noop
	}
	// Confined: on a review kick the tree is the PR author's, and a .hive
	// symlink in it must not redirect the host's write.
	if err := writeWorkspaceFileConfined(workspace, sandboxInferenceCARelPath, bundle); err != nil {
		release()
		e.warnInferenceRelay(spec.Agent, "write CA bundle", err)
		return nil, noop
	}
	caJob := path.Join(sandbox.DefaultWorkspaceMount, sandboxInferenceCARelPath)
	return sandboxInferenceEnv(baseURL, key, caJob), release
}

func (e *SandboxExecutor) warnInferenceRelay(agentName, step string, err error) {
	if e.Logger != nil {
		e.Logger.Warn("sandbox inference listener unavailable; this kick's Job uses its own model path", slog.String("agent", agentName), slog.String("step", step), slog.String("error", err.Error()))
	}
}
