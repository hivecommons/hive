package agent

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/hivecommons/hive/pkg/advisor"
	"github.com/hivecommons/hive/pkg/config"
)

// This file is the Claude Code adapter's launch-side half of the advisor lane
// (hivecommons/hive#9722): the `Stop` hook rendered into a settings JSON hive
// passes on the command line at launch. Projection at launch, never a
// config-file edit — hive does not touch the agent's own settings.json, so
// disabling the advisor needs nothing more than the next launch without the
// flag. The hook command itself is `hive advisor-hook` (pkg/advisor/cli.go),
// which talks only to the hive's loopback advise endpoint.

// SetAdvisorEnabledResolver injects the live "is the advisor on for this
// agent" predicate. Called from main.go with a closure over the live config,
// so an advisor toggled from the dashboard applies on the agent's next launch
// without a hive restart — the same discipline as the explain-mode resolver.
// A nil fn clears it, leaving the advisor off for every launch.
func (m *Manager) SetAdvisorEnabledResolver(fn func(agentName string) bool) {
	// Atomic store — no m.mu — so the launch path, which already holds m.mu,
	// can read it lock-free (see explainModeDefaultResolver for the deadlock
	// reasoning this follows).
	if fn == nil {
		m.advisorEnabledResolver.Store(nil)
		return
	}
	m.advisorEnabledResolver.Store(&fn)
}

// advisorEnabledFor reports whether the advisor lane is on for the named
// agent, or false when no resolver was injected (tests / bare setups). Safe
// to call while holding m.mu.
func (m *Manager) advisorEnabledFor(name string) bool {
	fnp := m.advisorEnabledResolver.Load()
	if fnp == nil || *fnp == nil {
		return false
	}
	return (*fnp)(name)
}

// advisorHookRunnerBinary resolves the hive executable the hook command runs
// as, same seam as agyTurnRunnerBinary: the running executable when
// resolvable, PATH lookup as the fallback. Overridable in tests.
var advisorHookRunnerBinary = func() string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		return exe
	}
	return "hive"
}

// claudeAdvisorSettings renders the compact settings JSON that registers the
// advisor's Stop hook for one Claude Code launch. The timeout gives the hook
// room for the hub-side review timeout plus the round trip.
func claudeAdvisorSettings() string {
	command := fmt.Sprintf("%s %s", advisorHookRunnerBinary(), advisor.HookSubcommand)
	settings := map[string]any{
		"hooks": map[string]any{
			"Stop": []map[string]any{
				{
					"hooks": []map[string]any{
						{"type": "command", "command": command, "timeout": 90},
					},
				},
			},
		},
	}
	data, err := json.Marshal(settings)
	if err != nil {
		return "" // cannot happen for this literal shape; fail open regardless
	}
	return string(data)
}

// advisorLaunchFlag returns the launch-command suffix that projects the
// advisor's turn-end hook into the agent's backend, or "" when the advisor is
// off for the agent, the backend has no adapter yet, or the launch shape
// cannot carry it. An advisor enabled on an unsupported backend is refused,
// not silently skipped: the agent still launches normally and the refusal is
// logged and reported by the dashboard's advisor listing.
func (m *Manager) advisorLaunchFlag(agent *AgentProcess, backend string, isInference bool) string {
	if !m.advisorEnabledFor(agent.Name) {
		return ""
	}
	if !config.AdvisorSupportedBackend(backend) {
		m.logger.Warn("advisor is not active on this backend; agent launches without it",
			"agent", agent.Name, "backend", backend, "supported", config.AdvisorClaudeBackend)
		return ""
	}
	if isInference {
		// Inference-routed Claude runs bare against the translator with a fixed
		// settings FILE; there is no interactive turn boundary to hook.
		m.logger.Warn("advisor is not active for inference-routed agents", "agent", agent.Name)
		return ""
	}
	settings := claudeAdvisorSettings()
	if settings == "" {
		return ""
	}
	return " --settings " + shellQuote(settings)
}
