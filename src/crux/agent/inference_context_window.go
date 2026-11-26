package agent

import (
	"os"
	"strconv"
	"strings"
)

// Claude Code sizes auto-compaction from the model ID. For an ID it does not
// recognize, such as a gateway's "vllm/<model>", it assumes a 1M-token window,
// so a conversation on a smaller local model can outgrow the server's real
// context before the CLI compacts, and the server then refuses the turn
// (hivecommons/hive#19913). CLAUDE_CODE_MAX_CONTEXT_TOKENS tells the CLI the
// real window; for an ID that is not a Claude model it takes effect on its own
// (https://code.claude.com/docs/en/model-config).
const (
	claudeMaxContextTokensEnv = "CLAUDE_CODE_MAX_CONTEXT_TOKENS"

	// inferencePassContextWindowEnv switches the export off when set to a
	// false value ("0", "false", "no", "off"). Default on: an inference-routed
	// agent with a known window gets it.
	inferencePassContextWindowEnv = "HIVE_INFERENCE_PASS_CONTEXT_WINDOW"

	inferenceAutoCompactPercentEnv = "HIVE_INFERENCE_AUTO_COMPACT_PERCENT"
	claudeAutoCompactWindowEnv     = "CLAUDE_CODE_AUTO_COMPACT_WINDOW"
	claudeAutoCompactPercentEnv    = "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"

	// claudeModelIDMarker identifies a model ID that names a Claude model
	// (for example "anthropic/claude-sonnet-5-5" behind a gateway). The CLI
	// already knows those windows, and the variable does not apply to them
	// on its own, so hive leaves them alone.
	claudeModelIDMarker = "claude"
)

// inferencePassContextWindowEnabled reads inferencePassContextWindowEnv.
func inferencePassContextWindowEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(inferencePassContextWindowEnv))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// inferenceContextWindowFor returns the context window, in tokens, to pass to
// the Claude CLI of an inference-routed agent, or 0 to pass none. model is
// the agent's configured model; the model on the agent's live route, when the
// proxy reports one, is checked instead, because that is the ID the CLI sees
// once an empty model resolves to the gateway default. The agent's
// max_context_tokens setting wins, but only while the agent runs its
// configured model: it describes that model, so a model override (a dashboard
// switch, a pin) falls back to discovery. Otherwise the window the proxy
// discovered for the route is used. Caller decides that the agent is inference-routed.
// Safe with m.mu held.
func (m *Manager) inferenceContextWindowFor(agent *AgentProcess, model string) int {
	if !inferencePassContextWindowEnabled() {
		return 0
	}
	discovered := 0
	if fnp := m.inferenceContextWindow.Load(); fnp != nil && *fnp != nil {
		var routeModel string
		discovered, routeModel = (*fnp)(agent.Name)
		if routeModel != "" {
			model = routeModel
		}
	}
	if strings.Contains(strings.ToLower(model), claudeModelIDMarker) {
		return 0
	}
	if n := agent.Config.MaxContextTokens; n > 0 && !modelOverridden(agent) {
		return n
	}
	return discovered
}

// inferenceContextWindowEnv returns the CLI env for a window from
// inferenceContextWindowFor: empty for 0.
func inferenceContextWindowEnv(window int) map[string]string {
	if window <= 0 {
		return nil
	}
	return map[string]string{claudeMaxContextTokensEnv: strconv.Itoa(window)}
}

// claudeContextEnv selects the same context and compaction settings for pane
// and Job launches. An explicit window describes only the configured model.
func (m *Manager) claudeContextEnv(agent *AgentProcess, backend, model string) map[string]string {
	routed := m.routableBackend(backend)
	if backend != "claude" && !routed {
		return nil
	}
	window := 0
	if routed {
		window = m.inferenceContextWindowFor(agent, model)
	}
	env := inferenceContextWindowEnv(window)
	if n := agent.Config.AutoCompactWindowTokens; n >= 100000 && !modelOverridden(agent) {
		if env == nil {
			env = make(map[string]string)
		}
		env[claudeAutoCompactWindowEnv] = strconv.Itoa(n)
		return env
	}
	pct, err := strconv.Atoi(strings.TrimSpace(os.Getenv(inferenceAutoCompactPercentEnv)))
	if window > 0 && err == nil && pct >= 1 && pct <= 100 {
		env[claudeAutoCompactPercentEnv] = strconv.Itoa(pct)
	}
	return env
}

// sandboxClaudeContextEnv omits the percentage because sandbox filtering
// removes CLAUDE_CODE_MAX_CONTEXT_TOKENS, which the percentage depends on.
func (m *Manager) sandboxClaudeContextEnv(agent *AgentProcess, backend, model string) map[string]string {
	env := m.claudeContextEnv(agent, backend, model)
	delete(env, claudeAutoCompactPercentEnv)
	return env
}

// modelOverridden reports whether the agent runs a model other than its
// configured one.
func modelOverridden(agent *AgentProcess) bool {
	o := strings.TrimSpace(agent.ModelOverride)
	return o != "" && o != strings.TrimSpace(agent.Config.Model)
}
