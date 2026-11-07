package agent

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Persistent CLI memory policy (hivecommons/hive#17032). See
// config.CLIMemoryConfig for the operator-facing knobs. Each CLI's switch is
// the one its vendor documents:
//
//   - Claude Code: auto-memory lives in ~/.claude/projects/<cwd>/memory/ and
//     is disabled with the CLAUDE_CODE_DISABLE_AUTO_MEMORY=1 environment
//     variable (equivalently "autoMemoryEnabled": false in settings.json).
//     https://code.claude.com/docs/en/memory — the env var is used because it
//     is per process, so a per-agent override needs no second settings file.
//   - Codex: memories are a feature flag, [features] memories = true|false in
//     $CODEX_HOME/config.toml, stored under $CODEX_HOME/memories/ (and the
//     memories_v2/ layout). https://developers.openai.com/codex/memories and
//     https://developers.openai.com/codex/config-reference
//   - Copilot CLI: the "memory" key in ~/.copilot/settings.json ("whether to
//     enable agentic memory", `copilot help config`; /memory on|off writes
//     it). The memory itself is server-side (Copilot Memory), so there is no
//     local store to wipe.
//   - Gemini CLI: no automatic memory; save_memory / `/memory add` write
//     ~/.gemini/GEMINI.md, which the wipe removes.
const (
	claudeDisableAutoMemoryEnv = "CLAUDE_CODE_DISABLE_AUTO_MEMORY"
	codexMemoriesFeatureKey    = "memories"
	codexFeaturesTableHeader   = "[features]"
	copilotSettingsFileName    = "settings.json"
	copilotMemorySettingKey    = "memory"
)

// claudeCLIBackend reports whether backend launches the Claude Code CLI
// (subscription or inference-routed), i.e. whether Claude's auto-memory
// switch applies.
func claudeCLIBackend(backend string) bool {
	return backend == "claude" || IsInferenceBackend(backend)
}

// cliMemoryEnvPairs returns the launch environment that pins the CLI's
// auto-memory off for this agent when the policy says so.
func cliMemoryEnvPairs(agent *AgentProcess, backend string) []agentEnvPair {
	if agent.Config.CLIMemory.AutoMemoryEnabled() || !claudeCLIBackend(backend) {
		return nil
	}
	return []agentEnvPair{{claudeDisableAutoMemoryEnv, "1", false}}
}

// codexConfigSeed applies every hive-owned pin to a $CODEX_HOME/config.toml:
// the startup update check off, and the memories feature pinned to the
// agents.cli_memory.auto_memory policy (hivecommons/hive#17032).
func codexConfigSeed(existing []byte, autoMemory bool) []byte {
	return codexConfigWithMemoriesPinned(codexConfigWithUpdateCheckDisabled(existing), autoMemory)
}

// codexConfigWithMemoriesPinned returns existing with `memories = <enabled>`
// set inside the [features] table, adding the table at the end when absent
// and replacing an existing memories line in place. Content outside that one
// key is preserved byte for byte, like codexConfigWithUpdateCheckDisabled.
func codexConfigWithMemoriesPinned(existing []byte, enabled bool) []byte {
	line := codexMemoriesFeatureKey + " = " + boolTOML(enabled)
	lines := strings.SplitAfter(string(existing), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	tableStart := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == codexFeaturesTableHeader {
			tableStart = i
			break
		}
	}
	if tableStart < 0 {
		text := strings.Join(lines, "")
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		if text != "" {
			text += "\n"
		}
		return []byte(text + codexFeaturesTableHeader + "\n" + line + "\n")
	}
	end := len(lines)
	for i := tableStart + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "[") {
			end = i
			break
		}
	}
	for i := tableStart + 1; i < end; i++ {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if key, _, ok := strings.Cut(trimmed, "="); ok && strings.TrimSpace(key) == codexMemoriesFeatureKey {
			lines[i] = line + lineEnding(lines[i])
			return []byte(strings.Join(lines, ""))
		}
	}
	// Insert as the last entry of the table, before any trailing blank lines
	// that separate it from the next table.
	insert := end
	for insert > tableStart+1 && strings.TrimSpace(lines[insert-1]) == "" {
		insert--
	}
	if !strings.HasSuffix(lines[insert-1], "\n") {
		lines[insert-1] += "\n"
	}
	merged := make([]string, 0, len(lines)+1)
	merged = append(merged, lines[:insert]...)
	merged = append(merged, line+"\n")
	merged = append(merged, lines[insert:]...)
	return []byte(strings.Join(merged, ""))
}

func boolTOML(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// seedCopilotMemorySetting pins the Copilot CLI "memory" setting in the
// agent's per-agent ~/.copilot/settings.json to the policy value. Unlike
// seedJSONFile this OVERWRITES the key: the policy owns it, so flipping
// agents.cli_memory.auto_memory takes effect on the next launch instead of
// being shadowed by the value an earlier launch wrote. Other keys are kept.
func (m *Manager) seedCopilotMemorySetting(agent *AgentProcess, home string) {
	dir := filepath.Join(home, copilotHomeDirName)
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	path := filepath.Join(dir, copilotSettingsFileName)
	want := agent.Config.CLIMemory.AutoMemoryEnabled()
	existing := map[string]any{}
	if data, err := readInferenceConfigFile(path); err == nil {
		if jsonErr := json.Unmarshal(data, &existing); jsonErr != nil {
			m.logger.Warn("copilot settings unparseable, rewriting",
				"agent", agent.Name, "path", path, "error", jsonErr)
			existing = map[string]any{}
		}
	}
	if cur, ok := existing[copilotMemorySettingKey].(bool); ok && cur == want {
		return
	}
	existing[copilotMemorySettingKey] = want
	data, err := json.Marshal(existing)
	if err != nil {
		m.logger.Warn("failed to marshal copilot settings", "agent", agent.Name, "path", path, "error", err)
		return
	}
	if err := writeInferenceConfigFile(path, data); err != nil {
		m.logger.Warn("failed to write copilot settings", "agent", agent.Name, "path", path, "error", err)
		return
	}
	if agent.UID > 0 {
		// Best-effort: unprivileged deployments cannot chown, and the file
		// mode already keeps it usable there.
		_ = os.Chown(path, agent.UID, -1)
	}
}

// cliMemoryStorePaths lists the persistent memory stores of every CLI this
// agent might have run, under its own HOME and CODEX_HOME only. Shared-home
// paths are never returned: under HIVE_SHARED_AGENT_HOME=1 (or a UID-0
// agent) the stores are fleet-shared and wiping them for one agent's kick
// would erase every other agent's memory too.
func cliMemoryStorePaths(agent *AgentProcess, backend string) []string {
	if agent.UID <= 0 || sharedAgentHomeForced() {
		return nil
	}
	home := AgentHome(agent.Name, agent.UID, backend)
	var paths []string
	for _, spec := range perAgentCLIHomes {
		for _, rel := range spec.memoryRel {
			matches, err := filepath.Glob(filepath.Join(home, spec.dir, filepath.FromSlash(rel)))
			if err != nil {
				continue
			}
			paths = append(paths, matches...)
		}
	}
	for _, rel := range codexMemoryDirs {
		p := filepath.Join(codexHomePath(agent.Name), rel)
		if _, err := os.Lstat(p); err == nil {
			paths = append(paths, p)
		}
	}
	return paths
}

// codexMemoryDirs are the Codex memory layouts under $CODEX_HOME
// (https://developers.openai.com/codex/memories).
var codexMemoryDirs = []string{"memories", "memories_v2"}

// clearCLIMemoryForKick removes the agent's persistent CLI memory stores
// before a kick when agents.cli_memory.clear_on_kick is set, so the new turn
// starts from the kick template and the shared knowledge system alone. It is
// best-effort: a store that cannot be removed is logged and the kick still
// proceeds, since a stale memory is a quality defect, not a safety one.
func (m *Manager) clearCLIMemoryForKick(agent *AgentProcess, backend string) {
	if !agent.Config.CLIMemory.ClearMemoryOnKick() {
		return
	}
	paths := cliMemoryStorePaths(agent, backend)
	if len(paths) == 0 {
		return
	}
	userSpec := m.agentExecUserSpec(agent)
	var removed []string
	for _, p := range paths {
		if err := m.removeAgentPath(p, userSpec); err != nil {
			m.logger.Warn("failed to clear CLI memory before kick",
				"agent", agent.Name, "path", p, "error", err)
			continue
		}
		removed = append(removed, p)
	}
	if len(removed) > 0 {
		m.logger.Info("cleared CLI persistent memory before kick",
			"agent", agent.Name, "paths", removed)
	}
}

// removeAgentPath removes path (recursively) from the manager's own identity,
// falling back to `su-exec <agent> rm -rf` when the tree is owned by the
// agent UID and the manager is not privileged to delete it.
func (m *Manager) removeAgentPath(path, userSpec string) error {
	err := os.RemoveAll(path)
	if err == nil || !errors.Is(err, fs.ErrPermission) || userSpec == "" {
		return err
	}
	if out, rerr := exec.Command("su-exec", userSpec, "rm", "-rf", "--", path).CombinedOutput(); rerr != nil {
		return outputErr("removing "+path+" as "+userSpec, rerr, out)
	}
	return nil
}
