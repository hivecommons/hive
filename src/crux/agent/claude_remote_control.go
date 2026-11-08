package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
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
// fleet-critical keys hive does not own (enabledPlugins, ...), and the seed
// must never be a reason to touch them.
func claudeRemoteControlSeed() map[string]any {
	return map[string]any{remoteControlSettingKey: false}
}

// claudeBypassConsentSeed returns the key that suppresses Claude Code's
// "Bypass Permissions mode" consent screen (see inferenceSettingsSeed for how
// it was verified). Interactive claude agents launch with
// --dangerously-skip-permissions too, and nothing else provisions this key in
// the shared settings, so without it every launch stops on a menu whose
// default answer is "No, exit" (#14307).
func claudeBypassConsentSeed() map[string]any {
	return map[string]any{"skipDangerousModePermissionPrompt": true}
}

// sharedClaudeSettingsFile is the userSettings file every interactive
// claude-backend agent reads: each per-agent ~/.claude is a REAL directory
// (perAgentCLIHomes) whose settings.json symlinks to
// sharedAgentHome/.claude/settings.json, so one file governs the whole fleet.
// Hive owns that file; see ensureClaudeRemoteControlDefault.
func sharedClaudeSettingsFile() string {
	return filepath.Join(sharedAgentHome, claudeHomeDirName, claudeSettingsFileName)
}

// ensureClaudeRemoteControlDefault re-asserts, before a claude-backend launch,
// that Remote Control does not auto-start for the fleet. Called on EVERY
// launch so a relaunch can never fall back to the server-side rollout default,
// and so an out-of-band deletion of the key self-heals on the next launch —
// the same posture as ensureBobAuthSettings for bob's shared settings. The
// bypass-consent suppressor (claudeBypassConsentSeed) is seeded the same way,
// and so are the CLI-memory drain hooks (mergeClaudeMemoryDrainHooks).
//
// The shared file is hive-owned (hivecommons/hive#17059): before seeding, a
// settings.json that is a symlink or owned by another uid is moved aside and
// recreated from the seed; after seeding, the file is verified again and
// group/other write is removed. A file that still fails verification is moved
// aside so the launch never reads agent-writable settings.
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
	dirInfo, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		m.logger.Debug("shared claude settings dir absent; skipping remote-control pin",
			"agent", agent.Name, "dir", filepath.Dir(path), "error", err)
		return
	}
	if !dirInfo.IsDir() {
		m.logger.Warn("shared claude settings dir is not a real directory; not writing hive settings through it",
			"agent", agent.Name, "dir", filepath.Dir(path), "mode", dirInfo.Mode().String())
		return
	}
	m.rejectUntrustedSharedClaudeSettings(agent.Name, path)
	m.seedJSONFile(agent.Name, path, claudeRemoteControlSeed())
	m.seedJSONFile(agent.Name, path, claudeBypassConsentSeed())
	sessionEnd, onWrite := m.claudeMemoryDrainHooksLocked()
	m.mergeClaudeMemoryDrainHooks(agent.Name, path, sessionEnd, onWrite)
	if reason := lockSharedClaudeSettings(path); reason != "" {
		m.logger.Warn("shared claude settings.json could not be brought to a hive-owned state after seeding",
			"agent", agent.Name, "path", path, "reason", reason)
		m.moveSharedClaudeSettingsAside(agent.Name, path, reason)
		return
	}
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

// sharedClaudeSettingsFileMode is the widest mode the hive-owned shared Claude
// settings.json may have: readable by every agent UID, writable only by the
// hive.
// Claude Code user settings carry "hooks", "apiKeyHelper", "env" and other
// keys that run commands inside every agent's Claude session, so a file any
// agent could rewrite would be a cross-agent execution channel
// (hivecommons/hive#17059).
const sharedClaudeSettingsFileMode os.FileMode = 0o644

// sharedClaudeSettingsRejectedSuffix names where an untrusted shared
// settings.json is moved before hive re-seeds the file, so an operator can
// review it and re-apply any keys they want back. Only the latest rejected
// copy is kept.
const sharedClaudeSettingsRejectedSuffix = ".rejected"

// sharedClaudeSettingsOwnerTrusted reports whether uid may own the shared
// Claude settings.json: root, the hive's own uid, or DevUID (the uid the
// permissions watcher hands root-owned entries to). Agent UIDs never qualify.
func sharedClaudeSettingsOwnerTrusted(uid uint32) bool {
	return uid == 0 || int(uid) == os.Geteuid() || int(uid) == DevUID
}

// untrustedSharedClaudeSettingsReason returns why the entry described by info
// (from Lstat) cannot be the hive-owned shared settings.json, or "" when its
// type and owner are acceptable. The mode is checked separately because a
// hive-owned file that is merely group-writable can be tightened in place.
func untrustedSharedClaudeSettingsReason(info os.FileInfo) string {
	if info.Mode()&os.ModeSymlink != 0 {
		return "symlink"
	}
	if !info.Mode().IsRegular() {
		return "not a regular file"
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "owner unknown"
	}
	if !sharedClaudeSettingsOwnerTrusted(st.Uid) {
		return fmt.Sprintf("owned by uid %d, not the hive", st.Uid)
	}
	return ""
}

// rejectUntrustedSharedClaudeSettings moves the shared settings.json aside
// when it is a symlink, not a regular file, or owned by a uid other than the
// hive's. /data/home/.claude stays group-writable (agents replace the shared
// OAuth credential by temp-file-and-rename there), so any agent can
// rename-replace settings.json; checking ownership before every claude launch
// is what keeps a replaced file from reaching the next session. The seeders
// that run next recreate the file from the hive seed.
func (m *Manager) rejectUntrustedSharedClaudeSettings(agentName, path string) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		m.logger.Warn("failed to inspect shared claude settings.json",
			"agent", agentName, "path", path, "error", err)
		return
	}
	if reason := untrustedSharedClaudeSettingsReason(info); reason != "" {
		m.moveSharedClaudeSettingsAside(agentName, path, reason)
	}
}

// moveSharedClaudeSettingsAside renames the shared settings.json (or the
// symlink standing in its place — rename never follows it) to the rejected
// path, replacing any earlier rejected copy.
func (m *Manager) moveSharedClaudeSettingsAside(agentName, path, reason string) {
	rejected := path + sharedClaudeSettingsRejectedSuffix
	if err := os.Rename(path, rejected); err != nil {
		if os.IsNotExist(err) {
			return
		}
		m.logger.Warn("shared claude settings.json failed the hive ownership check and could not be moved aside",
			"agent", agentName, "path", path, "reason", reason, "error", err)
		return
	}
	m.logger.Warn("shared claude settings.json failed the hive ownership check; moved aside so it is re-seeded by hive",
		"agent", agentName, "path", path, "reason", reason, "rejected", rejected)
}

// lockSharedClaudeSettings verifies the shared settings.json through a
// descriptor opened without following symlinks (and without blocking on a
// planted FIFO), and removes group/other write from a hive-owned file (the
// entrypoint and the permissions watcher historically opened the whole shared
// tree to the agents group). Returns "" when the file is hive-owned and not
// writable by agents, otherwise the reason it is not.
func lockSharedClaudeSettings(path string) string {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Sprintf("open: %v", err)
	}
	defer func() { _ = f.Close() }() // read-only fd; nothing to lose on close error
	info, err := f.Stat()
	if err != nil {
		return fmt.Sprintf("stat: %v", err)
	}
	if reason := untrustedSharedClaudeSettingsReason(info); reason != "" {
		return reason
	}
	perm := info.Mode().Perm()
	if perm&0o022 == 0 {
		return ""
	}
	if err := f.Chmod(perm & sharedClaudeSettingsFileMode); err != nil {
		return fmt.Sprintf("group- or world-writable (%v) and chmod failed: %v", perm, err)
	}
	return ""
}
