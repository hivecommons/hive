package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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

// advisorHooksFileName is the hive-owned Copilot hook file, dropped into the
// hooks directory under the COPILOT_HOME hive points at the agent's own
// per-agent .copilot directory. codexHooksFileName is Codex's hooks.json in
// the per-agent CODEX_HOME hive provisions. Neither is a file the operator
// maintains: config.json and config.toml are never written for the advisor.
const (
	copilotAdvisorHooksFileName = "hive-advisor.json"
	codexHooksFileName          = "hooks.json"
	advisorHookTimeoutS         = 90
)

// copilotAdvisorHooksJSON renders the Copilot CLI hook file registering the
// advisor on `agentStop`. The command asks for the Copilot dialect.
func copilotAdvisorHooksJSON() []byte {
	command := fmt.Sprintf("%s %s --format %s", advisorHookRunnerBinary(), advisor.HookSubcommand, advisor.HookFormatCopilot)
	doc := map[string]any{
		"version": 1,
		"hooks": map[string]any{
			"agentStop": []map[string]any{
				{"type": "command", "bash": command, "timeoutSec": advisorHookTimeoutS},
			},
		},
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil // cannot happen for this literal shape; fail open regardless
	}
	return append(data, '\n')
}

// codexAdvisorHooksJSON renders the Codex CLI hooks.json registering the
// advisor's `Stop` hook. Codex speaks the same dialect as Claude Code here.
func codexAdvisorHooksJSON() []byte {
	command := fmt.Sprintf("%s %s --format %s", advisorHookRunnerBinary(), advisor.HookSubcommand, advisor.HookFormatCodex)
	doc := map[string]any{
		"hooks": map[string]any{
			"Stop": []map[string]any{
				{
					"hooks": []map[string]any{
						{"type": "command", "command": command, "timeout": advisorHookTimeoutS},
					},
				},
			},
		},
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil
	}
	return append(data, '\n')
}

// advisorFileBackend reports whether the backend receives the advisor through
// hook files in the per-agent home rather than a launch argument.
func advisorFileBackend(backend string) bool {
	b := strings.ToLower(strings.TrimSpace(backend))
	return b == config.AdvisorCopilotBackend || b == config.AdvisorCodexBackend
}

// advisorCopilotHome is the directory hive points COPILOT_HOME at when the
// advisor is on for a Copilot agent: the agent's own per-agent .copilot
// (interactive_home.go), which hive provisions. "" when the agent has no
// per-agent home to own.
func advisorCopilotHome(agent *AgentProcess) string {
	if agent.UID <= 0 || sharedAgentHomeForced() {
		return ""
	}
	return filepath.Join(interactiveHomePath(agent.Name), ".copilot")
}

// advisorHooksPath returns where the advisor's hook file lives for a
// file-projected backend, or "" when the agent has no hive-owned home.
func advisorHooksPath(agent *AgentProcess, backend string) string {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case config.AdvisorCopilotBackend:
		home := advisorCopilotHome(agent)
		if home == "" {
			return ""
		}
		return filepath.Join(home, "hooks", copilotAdvisorHooksFileName)
	case config.AdvisorCodexBackend:
		if agent.UID <= 0 {
			return ""
		}
		return filepath.Join(codexHomePath(agent.Name), codexHooksFileName)
	}
	return ""
}

// advisorHooksActive reports whether the advisor projects hook files for this
// agent on a Copilot or Codex launch.
func (m *Manager) advisorHooksActive(agent *AgentProcess, backend string) bool {
	return m.advisorEnabledFor(agent.Name) && advisorFileBackend(backend) && advisorHooksPath(agent, backend) != ""
}

// provisionAdvisorHooks writes the advisor's hook file into the hive-owned
// home for a Copilot or Codex launch, or removes a file it wrote earlier when
// the advisor is now off, so disabling needs nothing but the next launch.
// Best-effort like the other home provisioning: a failure is logged and the
// agent launches without the advisor. Call after the homes are provisioned.
func (m *Manager) provisionAdvisorHooks(agent *AgentProcess, backend string) {
	if !advisorFileBackend(backend) {
		return
	}
	path := advisorHooksPath(agent, backend)
	if path == "" {
		return
	}
	user := m.agentExecUserSpec(agent)
	if !m.advisorHooksActive(agent, backend) {
		if existing, err := readFileAsUser(user, path); err == nil && strings.Contains(string(existing), advisor.HookSubcommand) {
			if err := exec.Command("su-exec", user, "rm", "-f", path).Run(); err != nil {
				m.logger.Warn("failed to remove stale advisor hook file", "agent", agent.Name, "path", path, "error", err)
			}
		}
		return
	}
	content := codexAdvisorHooksJSON()
	if strings.EqualFold(strings.TrimSpace(backend), config.AdvisorCopilotBackend) {
		content = copilotAdvisorHooksJSON()
	}
	if len(content) == 0 {
		return
	}
	if err := exec.Command("su-exec", user, "mkdir", "-p", filepath.Dir(path)).Run(); err != nil {
		m.logger.Warn("failed to create advisor hook directory; agent launches without the advisor", "agent", agent.Name, "dir", filepath.Dir(path), "error", err)
		return
	}
	if err := writeFileAsUser(user, path, content); err != nil {
		m.logger.Warn("failed to write advisor hook file; agent launches without the advisor", "agent", agent.Name, "path", path, "error", err)
	}
}

// advisorLaunchFlag returns the launch-command suffix that projects the
// advisor's turn-end hook into the agent's backend, or "" when the advisor is
// off for the agent, the backend has no adapter yet, or the launch shape
// cannot carry it. An advisor enabled on an unsupported backend is refused,
// not silently skipped: the agent still launches normally and the refusal is
// logged and reported by the dashboard's advisor listing. Copilot carries no
// launch flag at all (its hooks directory rides COPILOT_HOME, see
// agentEnvPairs); Codex needs its experimental hooks feature switched on.
func (m *Manager) advisorLaunchFlag(agent *AgentProcess, backend string, isInference bool) string {
	if !m.advisorEnabledFor(agent.Name) {
		return ""
	}
	if !config.AdvisorSupportedBackend(backend) {
		m.logger.Warn("advisor is not active on this backend; agent launches without it",
			"agent", agent.Name, "backend", backend, "supported", config.AdvisorSupportedBackendList)
		return ""
	}
	if strings.EqualFold(strings.TrimSpace(backend), config.AdvisorOMPBackend) {
		return m.ompAdvisorLaunchFlag(agent)
	}
	if isInference {
		// Inference-routed Claude runs bare against the translator with a fixed
		// settings FILE; there is no interactive turn boundary to hook.
		m.logger.Warn("advisor is not active for inference-routed agents", "agent", agent.Name)
		return ""
	}
	if advisorFileBackend(backend) && advisorHooksPath(agent, backend) == "" {
		m.logger.Warn("advisor needs a hive-provisioned per-agent home; agent launches without it",
			"agent", agent.Name, "backend", backend)
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case config.AdvisorCodexBackend:
		return " -c features.hooks=true"
	case config.AdvisorCopilotBackend:
		return ""
	}
	settings := claudeAdvisorSettings()
	if settings == "" {
		return ""
	}
	return " --settings " + shellQuote(settings)
}
