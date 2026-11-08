package agent

import (
	"encoding/json"
	"os"
)

// Claude Code memory-drain hooks (hivecommons/hive#17210). Claude Code runs
// a command hook at SessionEnd, and optionally after every Write, Edit or
// MultiEdit, so its auto-memory reaches the knowledge store inside the
// session instead of waiting for the next kick. Hive owns the Claude settings
// files its agents read, so it seeds the hook there and takes it back out
// when the drain is turned off.

// claudeMemoryDrainHookCommand is Hive's hook. It names the agent through
// HIVE_AGENT, so one fleet-wide entry serves every agent, and the hive
// answers a no-op for an agent whose drain is off.
const claudeMemoryDrainHookCommand = `hive memory-drain --cli claude --agent "$HIVE_AGENT"`

// claudeMemoryDrainWriteMatcher scopes the PostToolUse hook to the tools
// that write files; the command itself exits at once for any file outside
// ~/.claude/projects/*/memory/.
const claudeMemoryDrainWriteMatcher = "Write|Edit|MultiEdit"

const (
	claudeHookSessionEnd  = "SessionEnd"
	claudeHookPostToolUse = "PostToolUse"
)

// claudeMemoryDrainHooksLocked reports which hooks the fleet needs: the
// settings files are shared, so one draining agent is enough to seed a hook.
// Caller holds m.mu (the launch path).
func (m *Manager) claudeMemoryDrainHooksLocked() (sessionEnd, onWrite bool) {
	for _, a := range m.agents {
		if a == nil {
			continue
		}
		if a.Config.CLIMemory.DrainMemoryToKnowledge() {
			sessionEnd = true
		}
		if a.Config.CLIMemory.DrainMemoryOnWrite() {
			onWrite = true
		}
	}
	return sessionEnd, onWrite
}

// seedClaudeSettingsFiles writes (or repairs) each Claude settings file and
// then merges the CLI-memory drain hooks into it (hivecommons/hive#17210).
// Both the HOME userSettings file and the standalone --settings file get the
// same entries; Claude Code runs an identical hook command once.
// Caller holds m.mu (the launch path).
func (m *Manager) seedClaudeSettingsFiles(agentName string, paths ...string) {
	sessionEnd, onWrite := m.claudeMemoryDrainHooksLocked()
	for _, path := range paths {
		m.seedClaudeSettingsFile(agentName, path)
		m.mergeClaudeMemoryDrainHooks(agentName, path, sessionEnd, onWrite)
	}
}

// mergeClaudeMemoryDrainHooks adds or removes Hive's hook entries in the
// Claude settings file at path. seedJSONFile only merges top-level keys and
// would never touch an existing "hooks" object, so this goes one level
// deeper, per hooks.<event>: an operator's entries are always kept, Hive's
// entry is added only when absent and removed (alone) when its hook is off.
// A file that is missing or unparseable is left to seedJSONFile, which runs
// first; a file that needs no change is not rewritten.
func (m *Manager) mergeClaudeMemoryDrainHooks(agentName, path string, sessionEnd, onWrite bool) {
	data, err := readInferenceConfigFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			m.logger.Warn("failed to read claude settings for memory-drain hooks", "agent", agentName, "path", path, "error", err)
		}
		return
	}
	settings := map[string]any{}
	if json.Unmarshal(data, &settings) != nil {
		return
	}
	if !applyClaudeMemoryDrainHooks(settings, sessionEnd, onWrite) {
		return
	}
	out, err := json.Marshal(settings)
	if err != nil {
		m.logger.Warn("failed to marshal claude settings", "agent", agentName, "path", path, "error", err)
		return
	}
	if err := writeInferenceConfigFile(path, out); err != nil {
		m.logger.Warn("failed to write claude memory-drain hooks", "agent", agentName, "path", path, "error", err)
		return
	}
	m.logger.Info("claude memory-drain hooks updated", "agent", agentName, "path", path,
		"session_end", sessionEnd, "post_tool_use", onWrite)
}

// applyClaudeMemoryDrainHooks edits settings in place and reports whether
// anything changed. A "hooks" value that is not an object is the operator's
// and is left alone.
func applyClaudeMemoryDrainHooks(settings map[string]any, sessionEnd, onWrite bool) bool {
	raw, present := settings["hooks"]
	hooks, ok := raw.(map[string]any)
	if present && !ok {
		return false
	}
	if hooks == nil {
		hooks = map[string]any{}
	}
	changed := setClaudeMemoryDrainHook(hooks, claudeHookSessionEnd, "", sessionEnd)
	changed = setClaudeMemoryDrainHook(hooks, claudeHookPostToolUse, claudeMemoryDrainWriteMatcher, onWrite) || changed
	if !changed {
		return false
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		settings["hooks"] = hooks
	}
	return true
}

// setClaudeMemoryDrainHook makes hooks[event] carry Hive's command when want
// is true and not carry it when false, touching nothing else.
func setClaudeMemoryDrainHook(hooks map[string]any, event, matcher string, want bool) bool {
	raw, present := hooks[event]
	groups, ok := raw.([]any)
	if present && !ok {
		return false
	}
	has := false
	for _, g := range groups {
		if len(hiveHookItems(g)) > 0 {
			has = true
			break
		}
	}
	if want {
		if has {
			return false
		}
		group := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": claudeMemoryDrainHookCommand}}}
		if matcher != "" {
			group["matcher"] = matcher
		}
		hooks[event] = append(groups, group)
		return true
	}
	if !has {
		return false
	}
	kept := make([]any, 0, len(groups))
	for _, g := range groups {
		gm, _ := g.(map[string]any)
		items, _ := gm["hooks"].([]any)
		hive := hiveHookItems(g)
		if len(hive) == 0 {
			kept = append(kept, g)
			continue
		}
		if len(hive) == len(items) {
			continue
		}
		rest := make([]any, 0, len(items)-len(hive))
		for _, it := range items {
			if !isClaudeMemoryDrainHookItem(it) {
				rest = append(rest, it)
			}
		}
		gm["hooks"] = rest
		kept = append(kept, gm)
	}
	if len(kept) == 0 {
		delete(hooks, event)
	} else {
		hooks[event] = kept
	}
	return true
}

// hiveHookItems returns the items of one matcher group that are Hive's hook.
func hiveHookItems(group any) []any {
	gm, _ := group.(map[string]any)
	items, _ := gm["hooks"].([]any)
	var out []any
	for _, it := range items {
		if isClaudeMemoryDrainHookItem(it) {
			out = append(out, it)
		}
	}
	return out
}

func isClaudeMemoryDrainHookItem(item any) bool {
	im, _ := item.(map[string]any)
	cmd, _ := im["command"].(string)
	return cmd == claudeMemoryDrainHookCommand
}
