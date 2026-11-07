package agent

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CLI auto-memory policy (agents.cli_memory, #17032).
//
// Every coding CLI hive drives keeps its own persistent memory across
// sessions, and a lane worker should not: a one-off inference written down on
// Tuesday comes back on every kick after it. The fleet policy has two halves:
//
//   - auto_memory: false (the default) seeds each CLI's documented disable at
//     launch — Claude Code's "autoMemoryEnabled": false, Codex's
//     [features] memories = false. Merge-only, like every other hive seed: a
//     value the operator already wrote is left alone. Gemini CLI and Copilot
//     CLI document no local switch (Gemini's memory is plain GEMINI.md files;
//     Copilot Memory is stored on GitHub), so nothing is seeded for them.
//   - clear_on_kick: true removes the agent's own memory store before each
//     kick (clearCLIMemoryLocked).

// claudeAutoMemorySettingKey is the Claude Code settings key that turns auto
// memory (~/.claude/projects/<project>/memory) on or off. Honored from user
// settings and from the --settings flagSettings file.
const claudeAutoMemorySettingKey = "autoMemoryEnabled"

// claudeAutoMemorySeed returns the settings key that turns Claude Code's auto
// memory off.
func claudeAutoMemorySeed() map[string]any {
	return map[string]any{claudeAutoMemorySettingKey: false}
}

// inferenceClaudeSettingsFiles are the two settings surfaces an inference
// agent's Claude CLI reads: userSettings in its per-agent HOME and the
// standalone --settings file (see ensureClaudeSettings).
func inferenceClaudeSettingsFiles(agentName string) []string {
	return []string{
		filepath.Join(inferenceHomePath(agentName), ".claude", "settings.json"),
		claudeInferenceSettingsPath,
	}
}

// ensureClaudeAutoMemoryDefault seeds "autoMemoryEnabled": false into each
// Claude settings file in paths unless the fleet policy leaves auto-memory
// on. Called on every launch so an out-of-band deletion self-heals. A missing
// parent directory is skipped rather than created: the directories belong to
// ensureClaudeSettings and the entrypoint.
func (m *Manager) ensureClaudeAutoMemoryDefault(agent *AgentProcess, paths ...string) {
	if agent.Config.CLIMemory.AutoMemory {
		return
	}
	for _, path := range paths {
		if _, err := os.Lstat(filepath.Dir(path)); err != nil {
			m.logger.Debug("claude settings dir absent; skipping auto-memory seed",
				"agent", agent.Name, "dir", filepath.Dir(path), "error", err)
			continue
		}
		m.seedJSONFile(agent.Name, path, claudeAutoMemorySeed())
	}
}

// codexConfigSeeded is the config.toml hive writes into an agent's
// CODEX_HOME: the update check is always disabled, and memories are disabled
// unless the fleet policy leaves auto-memory on.
func codexConfigSeeded(existing []byte, autoMemory bool) []byte {
	out := codexConfigWithUpdateCheckDisabled(existing)
	if !autoMemory {
		out = codexConfigWithMemoriesDisabled(out)
	}
	return out
}

const (
	codexFeaturesTable       = "[features]"
	codexDisableMemoriesLine = "memories = false"
)

// codexConfigWithMemoriesDisabled adds memories = false to the [features]
// table of a Codex config.toml. Any existing memories setting — under
// [features], as a top-level features.memories dotted key, or inside a
// top-level features = {...} inline table — is the operator's and is left
// untouched; writing a second one would also make the TOML invalid.
func codexConfigWithMemoriesDisabled(existing []byte) []byte {
	lines := strings.SplitAfter(string(existing), "\n")
	featuresHeader := -1
	table := ""
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			header, _, _ := strings.Cut(trimmed, "#")
			table = strings.TrimSpace(header)
			if table == codexFeaturesTable && featuresHeader < 0 {
				featuresHeader = i
			}
			continue
		}
		key, _, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		switch {
		case table == codexFeaturesTable && key == "memories":
			return existing
		case table == "" && (key == "features" || key == "features.memories"):
			return existing
		}
	}
	if featuresHeader >= 0 {
		header := lines[featuresHeader]
		if !strings.HasSuffix(header, "\n") {
			header += "\n"
		}
		merged := make([]string, 0, len(lines)+1)
		merged = append(merged, lines[:featuresHeader]...)
		merged = append(merged, header, codexDisableMemoriesLine+"\n")
		merged = append(merged, lines[featuresHeader+1:]...)
		return []byte(strings.Join(merged, ""))
	}
	text := string(existing)
	if text != "" {
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += "\n"
	}
	return []byte(text + codexFeaturesTable + "\n" + codexDisableMemoriesLine + "\n")
}

// cliMemoryRemover deletes the given memory-store paths. With a userSpec it
// runs `rm -rf` AS THE AGENT via su-exec, so a clear can never delete more
// than the agent itself could: the stores sit in agent-writable trees, and a
// recursive delete by the (often root) hive process there would follow any
// symlink the agent planted on the way (the audit F12 class). Without a
// userSpec (UID 0 agents, which run as the hive user anyway) it removes the
// paths directly. A var as a TEST SEAM.
var cliMemoryRemover = func(userSpec string, paths []string) error {
	if userSpec == "" {
		var errs []error
		for _, p := range paths {
			errs = append(errs, os.RemoveAll(p))
		}
		return errors.Join(errs...)
	}
	args := append([]string{userSpec, "rm", "-rf", "--"}, paths...)
	out, err := exec.Command("su-exec", args...).CombinedOutput()
	if err != nil {
		return outputErr("su-exec rm", err, out)
	}
	return nil
}

// privateDirState classifies a directory a memory store lives under.
type privateDirState int

const (
	// dirAbsent: a component is missing, so there is no store to clear.
	dirAbsent privateDirState = iota
	// dirPrivate: root and every component below it are real directories.
	dirPrivate
	// dirShared: a component is a symlink (a bridge into shared state) or
	// otherwise not a plain directory, so a store below it may belong to
	// other agents too.
	dirShared
)

// agentPrivateDir classifies root/rel by Lstat-ing root and each component
// of rel in turn. A symlink anywhere on the way means the directory is (or
// may be) shared state, never the agent's own.
func agentPrivateDir(root, rel string) privateDirState {
	p := root
	parts := []string{""}
	if rel != "" {
		parts = append(parts, strings.Split(filepath.ToSlash(rel), "/")...)
	}
	for _, part := range parts {
		if part != "" {
			p = filepath.Join(p, part)
		}
		info, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return dirAbsent
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return dirShared
		}
	}
	return dirPrivate
}

// cliMemoryStores returns the existing memory stores of the agent's CLI that
// are private to the agent (targets), and the directories holding a store
// that were skipped because they are shared with other agents (shared).
//
//   - claude, and every inference backend (they run Claude Code):
//     $HOME/.claude/projects/*/memory
//   - codex: $CODEX_HOME/memories and $CODEX_HOME/memories_v2
//   - gemini: $HOME/.gemini/GEMINI.md, the global file save_memory writes
//
// A HOME that is not per-agent — UID 0 agents run out of the hive's own HOME,
// HIVE_SHARED_AGENT_HOME=1 puts every agent in /data/home — is shared by
// definition. So is a per-agent ~/.claude or ~/.gemini that is still a
// symlink bridge into /data/home (interactiveHomeBridgeDirs).
func (m *Manager) cliMemoryStores(agent *AgentProcess) (targets, shared []string) {
	backend := effectiveBackend(agent)
	if backend == codexBackend {
		root := codexHomePath(agent.Name)
		switch agentPrivateDir(root, "") {
		case dirShared:
			return nil, []string{root}
		case dirAbsent:
			return nil, nil
		}
		for _, name := range []string{"memories", "memories_v2"} {
			p := filepath.Join(root, name)
			if _, err := os.Lstat(p); err == nil {
				targets = append(targets, p)
			}
		}
		return targets, nil
	}

	var dir string
	switch {
	case backend == "gemini":
		dir = ".gemini"
	case backend == "claude" || m.routableBackend(backend):
		dir = filepath.Join(".claude", "projects")
	default:
		return nil, nil
	}
	home := AgentHome(agent.Name, agent.UID, backend)
	if agent.UID <= 0 || home == sharedAgentHome {
		if _, err := os.Lstat(filepath.Join(home, dir)); err == nil {
			shared = append(shared, filepath.Join(home, dir))
		}
		return nil, shared
	}
	switch agentPrivateDir(home, dir) {
	case dirShared:
		return nil, []string{filepath.Join(home, dir)}
	case dirAbsent:
		return nil, nil
	}
	if backend == "gemini" {
		p := filepath.Join(home, dir, "GEMINI.md")
		if _, err := os.Lstat(p); err == nil {
			targets = append(targets, p)
		}
		return targets, nil
	}
	projects := filepath.Join(home, dir)
	entries, err := os.ReadDir(projects)
	if err != nil {
		m.logger.Debug("cannot list claude projects; skipping auto-memory clear",
			"agent", agent.Name, "dir", projects, "error", err)
		return nil, nil
	}
	for _, e := range entries {
		project := filepath.Join(projects, e.Name())
		memory := filepath.Join(project, "memory")
		if _, err := os.Lstat(memory); err != nil {
			continue
		}
		if e.Type()&os.ModeSymlink != 0 || !e.IsDir() {
			shared = append(shared, project)
			continue
		}
		targets = append(targets, memory)
	}
	return targets, shared
}

// clearCLIMemoryLocked removes the agent's own CLI memory store before a kick
// when agents.cli_memory.clear_on_kick is on. Best-effort: a failure is
// logged and never blocks the kick. A store that is shared with other agents
// is skipped and logged once per agent, because clearing it would wipe
// every other agent's memory too. Caller holds m.mu.
func (m *Manager) clearCLIMemoryLocked(agent *AgentProcess) {
	if !agent.Config.CLIMemory.ClearOnKick {
		return
	}
	targets, shared := m.cliMemoryStores(agent)
	if len(shared) > 0 && !agent.cliMemorySharedLogged {
		agent.cliMemorySharedLogged = true
		m.logger.Warn("cli_memory.clear_on_kick: not clearing a CLI memory store shared with other agents; it clears once the agent's CLI home is per-agent",
			"agent", agent.Name, "paths", shared)
	}
	if len(targets) == 0 {
		return
	}
	if err := cliMemoryRemover(m.agentExecUserSpec(agent), targets); err != nil {
		m.logger.Warn("cli_memory.clear_on_kick: failed to clear CLI memory before kick",
			"agent", agent.Name, "paths", targets, "error", err)
		return
	}
	m.logger.Info("cleared CLI memory before kick", "agent", agent.Name, "paths", targets)
}
