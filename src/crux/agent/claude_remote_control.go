package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// remoteControlSettingKey is the Claude Code settings key that controls
// whether the Remote Control bridge starts with the session. Honored from
// user settings; an absent key delegates the decision to a server-side
// rollout, which is exactly what this pin exists to prevent.
const remoteControlSettingKey = "remoteControlAtStartup"

// remoteControlUsedKey is the ~/.claude.json marker the CLI persists once the
// bridge has actually run in a session (observed as hasUsedRemoteControl: true
// on the #5607 fleet). Read-only here: it is the launch-time evidence that the
// bridge came up despite (or before) the pin.
const remoteControlUsedKey = "hasUsedRemoteControl"

// claudeRemoteControlSeed returns the key hive pins into the shared Claude
// user settings for interactive claude-backend agents. Kept minimal on
// purpose: the shared /data/home/.claude/settings.json also carries
// fleet-critical keys hive does not own (skipDangerousModePermissionPrompt,
// enabledPlugins, ...), and the seed must never be a reason to touch them.
func claudeRemoteControlSeed() map[string]any {
	return map[string]any{remoteControlSettingKey: false}
}

// sharedClaudeSettingsFile is the userSettings file every interactive
// claude-backend agent reads: the per-agent homes symlink ~/.claude to
// sharedAgentHome/.claude (interactiveHomeBridgeDirs), so one file governs
// the whole fleet.
func sharedClaudeSettingsFile() string {
	return filepath.Join(sharedAgentHome, ".claude", "settings.json")
}

// ensureClaudeRemoteControlDefault re-asserts, before a claude-backend launch,
// that Remote Control does not auto-start for the fleet. Called on EVERY
// launch so a relaunch can never fall back to the server-side rollout default,
// and so an out-of-band deletion of the key self-heals on the next launch —
// the same posture as ensureBobAuthSettings for bob's shared settings.
//
// Semantics:
//   - key absent  -> written as false (the pin; this is the whole fix)
//   - key present -> left untouched, true OR false (operator intent wins);
//     an explicit true is logged as a receipt so an enabled bridge is always
//     explainable from the hive log
//   - unparseable file -> rewritten from the seed alone (seedJSONFile
//     semantics; the CLI could not read the old keys either)
//
// Errors are logged and swallowed: a settings file hive cannot write is not a
// reason to refuse a launch that may still succeed.
func (m *Manager) ensureClaudeRemoteControlDefault(agent *AgentProcess) {
	path := sharedClaudeSettingsFile()
	// The entrypoint pre-creates /data/home/.claude (it holds the shared OAuth
	// credential) before the hive starts. If it is missing there is no login
	// and therefore no bridge entitlement either, so skip rather than invent a
	// directory whose modes the entrypoint owns.
	if _, err := os.Lstat(filepath.Dir(path)); err != nil {
		m.logger.Debug("shared claude settings dir absent; skipping remote-control pin",
			"agent", agent.Name, "dir", filepath.Dir(path), "error", err)
		return
	}
	m.seedJSONFile(agent.Name, path, claudeRemoteControlSeed())
	if enabled, explicit := readRemoteControlSetting(path); explicit && enabled {
		// Receipt, not an error: the operator opted in and hive honored it.
		m.logger.Info("claude remote control left ON by explicit operator setting",
			"agent", agent.Name, "path", path, "key", remoteControlSettingKey)
	}
	home := AgentHome(agent.Name, agent.UID, "claude")
	if m.remoteControlBridgeUsed(home) {
		m.logger.Warn("claude remote control bridge has run for this agent despite the startup pin — history predating the pin, an explicit operator opt-in, or the pin not being honored",
			"agent", agent.Name, "session_file", claudeSessionFile(home),
			"marker", remoteControlUsedKey)
	}
}

// readRemoteControlSetting reports the value of remoteControlAtStartup in the
// settings file at path and whether it is explicitly present as a bool.
func readRemoteControlSetting(path string) (value, explicit bool) {
	data, err := readInferenceConfigFile(path)
	if err != nil {
		return false, false
	}
	settings := map[string]any{}
	if json.Unmarshal(data, &settings) != nil {
		return false, false
	}
	v, ok := settings[remoteControlSettingKey].(bool)
	return v, ok
}

// remoteControlBridgeUsed reports whether the agent's Claude session file
// records the Remote Control bridge having actually run. Best-effort and
// read-only: on privileged deployments the per-agent 0600 session file is
// readable by the hive; where it is not, the check silently reports false
// rather than blocking anything.
func (m *Manager) remoteControlBridgeUsed(home string) bool {
	if home == "" {
		return false
	}
	data, err := readInferenceConfigFile(claudeSessionFile(home))
	if err != nil {
		return false
	}
	session := map[string]any{}
	if json.Unmarshal(data, &session) != nil {
		return false
	}
	used, _ := session[remoteControlUsedKey].(bool)
	return used
}
