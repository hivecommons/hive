package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// agentStateDir is the directory holding the per-agent runtime state files:
// .hive-mode-<name> / .hive-caps-<name> (the enforcement files gh-wrapper.sh
// and the proxy read) and .hive-bootstrap-<name>.txt (the goose bootstrap
// prompt, cat'ed by a launch command built from this same value, so writer and
// reader can never disagree).
//
// A var (not const) as a TEST SEAM, matching the
// ModeFileGlob/CapsFileGlob/SharedRepoParent convention: a test suite running
// on a host that also runs a live hive must never write the live enforcement
// files under /tmp — rewriting /tmp/.hive-mode-<agent> from a test would
// change a REAL agent's GitHub enforcement mode (#4737/#4738). TestMain points
// this at the per-run temp tree. Production value is unchanged.
var agentStateDir = "/tmp"

const (
	claudeInferenceSettingsPath = "/tmp/.claude-inference-settings.json"
	claudeInferenceHomePrefix   = "/tmp/.claude-inference-home-"
)

// inferenceHomePrefixOverride redirects the per-agent inference HOME prefix.
// TEST SEAM ONLY — empty in production, where inferenceHomePath always returns
// claudeInferenceHomePrefix+name. It exists so the auth probe's per-UID home
// resolution can be exercised against a temp dir instead of /tmp.
var inferenceHomePrefixOverride string

// inferenceHomePath returns the per-agent inference HOME directory.
func inferenceHomePath(agentName string) string {
	if inferenceHomePrefixOverride != "" {
		return inferenceHomePrefixOverride + agentName
	}
	return claudeInferenceHomePrefix + agentName
}

// codexHomePrefix is the per-agent CODEX_HOME directory prefix. Each agent gets
// its own dir so Codex's owner-gated app-server sees a directory the agent UID
// actually owns (a shared, merely group-writable dir is not sufficient for
// Codex, unlike claude/copilot). Lives on the persistent /data volume.
var codexHomePrefix = "/data/home/.codex-"

// codexHomePath returns the per-agent CODEX_HOME directory.
func codexHomePath(agentName string) string {
	return codexHomePrefix + agentName
}

// codexSharedAuthFile is the login credential that a `codex login` (ChatGPT
// sign-in) or an OPENAI_API_KEY setup writes: a user running codex without
// CODEX_HOME set lands in $HOME/.codex, i.e. /data/home/.codex (group-writable
// so any agent UID can refresh the token). Because each agent has its OWN
// CODEX_HOME (for the app-server owner check), that per-agent dir would start
// with no auth — so a single sign-in would not reach the agents, and they would
// prompt for sign-in again. setupCodexHome bridges this by symlinking each
// agent's auth.json to this shared file, so ONE login propagates to every agent
// and token refreshes are shared.
var codexSharedAuthFile = "/data/home/.codex/auth.json"

// setupCodexHome pre-creates the agent's CODEX_HOME directory AS the agent, so
// it is owned by the agent UID. Codex 0.144.1 refuses to create CODEX_HOME
// itself ("CODEX_HOME ... does not exist") and its app-server requires the
// current UID to own the dir. The manager runs as dev and cannot chown, so —
// mirroring the tmux-dir setup — it runs mkdir via su-exec as the agent user.
// It also symlinks the agent's auth.json to the shared login file so a single
// `codex login` reaches every agent. No-op for root agents (UID 0), which own
// /data/home already.
func (m *Manager) setupCodexHome(agent *AgentProcess) {
	if agent.UID <= 0 {
		return
	}
	dir := codexHomePath(agent.Name)
	agentUser := m.agentExecUserSpec(agent)
	salvagedConfig := m.healCodexHomeOwnership(agent, dir, agentUser)
	if err := exec.Command("su-exec", agentUser, "mkdir", "-p", dir).Run(); err != nil {
		m.logger.Error("failed to pre-create codex home; codex cannot start without it, and a later run may create it under the wrong identity", "agent", agent.Name, "dir", dir, "error", err)
	}
	if salvagedConfig != nil {
		if err := writeFileAsUser(agentUser, filepath.Join(dir, "config.toml"), salvagedConfig); err != nil {
			m.logger.Warn("failed to restore salvaged codex config", "agent", agent.Name, "dir", dir, "error", err)
		}
	}
	// Bridge auth: symlink the per-agent auth.json to the shared login file so a
	// single sign-in propagates to all agents. `ln -sfn` is idempotent and
	// overwrites a stale regular-file auth.json left by an earlier codex version.
	// The symlink target need not exist yet (a later `codex login` creates it).
	authLink := filepath.Join(dir, "auth.json")
	if err := exec.Command("su-exec", agentUser, "ln", "-sfn", codexSharedAuthFile, authLink).Run(); err != nil {
		m.logger.Warn("failed to link codex auth", "agent", agent.Name, "link", authLink, "error", err)
	}
}

// healCodexHomeOwnership repairs a CODEX_HOME wedged by wrong-owner state.
// /data is the persistent volume, so a stale owner survives every restart:
// either the whole dir was created by a codex run under another identity
// (e.g. after a failed pre-create), or a config.toml inside an agent-owned
// dir was written as dev/root and the agent EACCESes on it at startup — the
// same failure class cavemanNpmCachePath removes foreign-owned caches for.
//
// The primary repair is an in-place `chown -R` to the agent UID (#5379): an
// agent RENAME is the common trigger — the per-agent CODEX_HOME keeps the
// PREVIOUS agent's owner — and a rename should not discard the lane's codex
// state (cache/, .tmp/, history). Chowning also avoids walking the tree from
// Go entirely, which matters because /data on hosted spokes is an NFSv3 PVC
// where os.RemoveAll's openat-based descent fails with EACCES even as root.
//
// Only when the chown itself fails do we fall back to a rebuild, and that
// rebuild shells out to `rm -rf` (via su-exec, the same idiom the rest of
// setupCodexHome uses) rather than os.RemoveAll, for the same NFSv3 reason.
// Hive never writes config.toml, so any content is operator-authored: the
// rebuild path salvages it when the manager can read it and returns the bytes
// for setupCodexHome to write back as the agent after the re-mkdir. A dir that
// can be neither chowned nor removed is left alone with an Error log naming
// the owner and the manual fix.
func (m *Manager) healCodexHomeOwnership(agent *AgentProcess, dir, agentUser string) []byte {
	owner := fileOwnerUID(dir)
	if owner < 0 {
		return nil // absent (fresh mkdir will own it) or unstattable
	}
	if owner == agent.UID {
		return m.healForeignCodexConfig(agent, dir, agentUser)
	}
	// Codex's app-server requires the current UID to own CODEX_HOME itself,
	// so a foreign-owned dir must be re-owned (preferred) or rebuilt.
	chownErr := m.chownCodexHomeToAgent(agent, dir)
	if chownErr == nil {
		m.logger.Warn("re-owned codex home that was owned by the wrong UID (agent rename); codex state preserved", "agent", agent.Name, "dir", dir, "ownerUID", owner, "wantUID", agent.UID)
		// healForeignCodexConfig is still the right follow-up: the recursive
		// chown fixed every entry it could reach, but a config.toml that is a
		// symlink or otherwise skipped stays foreign-owned, and that narrower
		// heal removes it (salvaging content) so codex can read its config.
		return m.healForeignCodexConfig(agent, dir, agentUser)
	}
	m.logger.Warn("could not chown codex home to the agent; falling back to rebuild", "agent", agent.Name, "dir", dir, "ownerUID", owner, "wantUID", agent.UID, "error", chownErr)
	cfgPath := filepath.Join(dir, "config.toml")
	salvaged, readErr := os.ReadFile(cfgPath)
	if err := removeTreeAsRoot(dir); err != nil {
		m.logger.Error("codex home is owned by the wrong UID and could not be rebuilt; codex will fail until it is chowned or removed manually", "agent", agent.Name, "dir", dir, "ownerUID", owner, "wantUID", agent.UID, "error", err)
		return nil
	}
	m.logger.Warn("rebuilt codex home that was owned by the wrong UID", "agent", agent.Name, "dir", dir, "ownerUID", owner, "wantUID", agent.UID, "configSalvaged", readErr == nil)
	if readErr != nil {
		return nil
	}
	return salvaged
}

// codexHomeChownUserSpec is the identity the recursive chown runs as. Only
// root can give a directory away to another UID, and the manager runs as dev,
// so this goes through the same SUID su-exec helper every other UID switch in
// setupCodexHome uses (see the Dockerfile C6 note: su-exec is 4750
// root:hive-launch, exec-able by dev but NOT by any agent UID).
const codexHomeChownUserSpec = "root"

// chownCodexHomeToAgent recursively gives CODEX_HOME to the agent UID.
//
// NFS SAFETY (#5379): this MUST shell out. /data on hosted spokes is NFSv3,
// where Go's own tree walks (os.RemoveAll, filepath.WalkDir + os.Lchown) fail
// mid-descent with "openfdat ...: permission denied" because openat-based
// directory descriptors are not reliably supported there. `chown -R` in
// coreutils does not use that access pattern and is verified working on the
// affected mount. Do not "simplify" this back into a Go walk.
//
// -h chowns symlinks THEMSELVES rather than following them: auth.json is a
// symlink into the SHARED credential file, which must keep its own ownership
// and must never be rewritten through.
func (m *Manager) chownCodexHomeToAgent(agent *AgentProcess, dir string) error {
	return chownTreeAsRoot(dir, fmt.Sprintf("%d:%d", agent.UID, os.Getgid()))
}

// chownTreeAsRoot is a var, not a plain func, ONLY so tests can substitute a
// harness that performs the same re-owning without the SUID helper (which
// exists only inside the image). Production always uses the exec below.
var chownTreeAsRoot = func(dir, spec string) error {
	cmd := exec.Command("su-exec", codexHomeChownUserSpec, "chown", "-Rh", spec, dir)
	if output, err := cmd.CombinedOutput(); err != nil {
		return outputErr(fmt.Sprintf("chown -Rh %s %s", spec, dir), err, output)
	}
	return nil
}

// removeTreeAsRoot deletes a tree the manager may not own.
//
// NFS SAFETY (#5379): os.RemoveAll CANNOT be used here. It descends with
// openat-based directory file descriptors, which fail with EACCES on the
// NFSv3-backed /data PVC even for root — the exact wedge that left a renamed
// agent's codex backend dead for days. A shell `rm -rf` on the identical path
// succeeds immediately. Keep this as an exec, not a Go walk.
// It is a var for the same test-substitution reason as chownTreeAsRoot.
var removeTreeAsRoot = func(dir string) error {
	cmd := exec.Command("su-exec", codexHomeChownUserSpec, "rm", "-rf", dir)
	if output, err := cmd.CombinedOutput(); err != nil {
		return outputErr(fmt.Sprintf("rm -rf %s", dir), err, output)
	}
	return nil
}

// healForeignCodexConfig handles the agent-owned-dir case: a config.toml
// owned by another UID (written by a codex run as dev/root with CODEX_HOME
// pointed at this agent's dir). The agent owns the dir, so it can unlink the
// file even though it cannot read it; content is preserved when the manager
// can read it.
func (m *Manager) healForeignCodexConfig(agent *AgentProcess, dir, agentUser string) []byte {
	cfgPath := filepath.Join(dir, "config.toml")
	owner := fileOwnerUID(cfgPath)
	if owner < 0 || owner == agent.UID {
		return nil
	}
	salvaged, readErr := os.ReadFile(cfgPath)
	if err := exec.Command("su-exec", agentUser, "rm", "-f", cfgPath).Run(); err != nil {
		m.logger.Error("codex config.toml is owned by the wrong UID and could not be removed; codex will fail until it is chowned or removed manually", "agent", agent.Name, "path", cfgPath, "ownerUID", owner, "wantUID", agent.UID, "error", err)
		return nil
	}
	m.logger.Warn("removed codex config.toml that was owned by the wrong UID", "agent", agent.Name, "path", cfgPath, "ownerUID", owner, "wantUID", agent.UID, "configSalvaged", readErr == nil)
	if readErr != nil {
		return nil
	}
	return salvaged
}

// fileOwnerUID returns the numeric owner of path, or -1 when it cannot be
// determined (path absent, or a non-unix Stat result).
func fileOwnerUID(path string) int {
	info, err := os.Lstat(path)
	if err != nil {
		return -1
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1
	}
	return int(st.Uid)
}

// writeFileAsUser writes content to path as userSpec via su-exec, for files
// that must end up owned by the agent UID (the manager runs as dev and
// cannot chown). The `sh -c 'cat > "$1"'` form keeps the path out of shell
// parsing entirely.
func writeFileAsUser(userSpec, path string, content []byte) error {
	cmd := exec.Command("su-exec", userSpec, "sh", "-c", `cat > "$1"`, "sh", path)
	cmd.Stdin = strings.NewReader(string(content))
	if output, err := cmd.CombinedOutput(); err != nil {
		return outputErr(fmt.Sprintf("writing %s as %s", path, userSpec), err, output)
	}
	return nil
}

// inferenceConfigMigrationVersion matches the Claude CLI internal config
// migration version so the CLI skips first-run migration prompts.
const inferenceConfigMigrationVersion = 13

// inferenceUserConfigSeed returns the required top-level keys for an inference
// agent's ~/.claude.json. These skip the first-run setup (onboarding,
// migrations) and pre-approve the per-agent inference API key.
//
// NOTE: "bypassPermissionsModeAccepted" does NOT suppress the interactive
// "Bypass Permissions mode" consent dialog — verified live against Claude CLI
// v2.1.190 and v2.1.204, the dialog is gated only on the settings key
// "skipDangerousModePermissionPrompt" (see inferenceSettingsSeed). The
// .claude.json key is still read by non-interactive CLI paths (e.g. the --bg
// bypass check), so it stays in the seed for those.
// apiKeyApprovalSuffixLen is how many trailing characters of an API key the
// Claude CLI compares against customApiKeyResponses.approved entries
// (key.slice(-20), verified in CLI v2.1.190). Seeding only the full key
// leaves keys longer than this unapproved — "sk-hive-" plus an agent name
// over 12 chars — so the CLI shows the "Detected a custom API key" prompt,
// whose default selection is "No (recommended)".
const apiKeyApprovalSuffixLen = 20

func inferenceUserConfigSeed(agentName string) map[string]any {
	apiKey := "sk-hive-" + agentName
	approved := []string{apiKey}
	if len(apiKey) > apiKeyApprovalSuffixLen {
		approved = append(approved, apiKey[len(apiKey)-apiKeyApprovalSuffixLen:])
	}
	return map[string]any{
		"hasCompletedOnboarding":        true,
		"opusProMigrationComplete":      true,
		"sonnet1m45MigrationComplete":   true,
		"migrationVersion":              inferenceConfigMigrationVersion,
		"bypassPermissionsModeAccepted": true,
		"customApiKeyResponses": map[string]any{
			"approved": approved,
			"rejected": []string{},
		},
	}
}

func inferenceProjectTrustSeed() map[string]any {
	return map[string]any{
		"hasTrustDialogAccepted": true,
		"allowedTools":           []any{},
	}
}

// inferenceSettingsSeed returns the required keys for an inference agent's
// Claude settings.json — both ~/.claude/settings.json (userSettings) and the
// standalone file passed via --settings (flagSettings).
//
// "skipDangerousModePermissionPrompt" is the key that actually suppresses the
// "Bypass Permissions mode" consent dialog. Verified live against Claude CLI
// v2.1.190 and v2.1.204 with a scratch HOME: the interactive dialog is gated
// only on this settings key (honored from userSettings, localSettings,
// flagSettings, or policySettings), and accepting the dialog interactively
// persists this same key into ~/.claude/settings.json. Seeding
// bypassPermissionsModeAccepted in .claude.json does NOT suppress the dialog
// on either version, and IS_SANDBOX=1 does not suppress it either. Without
// this key every --dangerously-skip-permissions launch shows a consent menu
// whose default selection is "No, exit" — if dismissal loses the race, the
// CLI exits and the pane degrades to bare bash.
// "remoteControlAtStartup" is pinned false because an ABSENT key delegates
// the decision to a server-side rollout that flipped to auto-ON (#5607, CLI
// 2.1.226+). Seeding it into both the userSettings and the --settings
// flagSettings file keeps the Remote Control bridge off at every relaunch;
// the merge-only repair in seedJSONFile preserves an operator's explicit
// true. See claude_remote_control.go for the full mechanism.
func inferenceSettingsSeed() map[string]any {
	return map[string]any{
		"permissions":                       map[string]any{"allow": []any{}, "deny": []any{}},
		"hasCompletedOnboarding":            true,
		"bypassPermissions":                 true,
		"hasAcknowledgedDisclaimer":         true,
		"skipDangerousModePermissionPrompt": true,
		remoteControlSettingKey:             false,
	}
}

// seedClaudeUserConfig writes or repairs the inference agent's .claude.json.
// Unlike a plain exists-check, this merges required keys into an existing
// file that is missing some of them (e.g. seeded by an older hive version
// without bypassPermissionsModeAccepted) and rewrites a file that fails to
// parse. A complete, parseable file is left untouched.
func (m *Manager) seedClaudeUserConfig(agentName, path string) {
	m.seedJSONFile(agentName, path, inferenceUserConfigSeed(agentName))
	m.mergeApprovedAPIKeys(agentName, path)
}

func (m *Manager) seedClaudeProjectTrust(agentName, path string, projectDirs []string) {
	if len(projectDirs) == 0 {
		return
	}
	data, err := readInferenceConfigFile(path)
	existing := map[string]any{}
	if err == nil {
		if jsonErr := json.Unmarshal(data, &existing); jsonErr != nil {
			m.logger.Warn("inference config unparseable, rewriting",
				"agent", agentName, "path", path, "error", jsonErr)
			existing = map[string]any{}
		}
	}
	changed := false
	if _, ok := existing["hasCompletedOnboarding"]; !ok {
		existing["hasCompletedOnboarding"] = true
		changed = true
	}
	projects, ok := existing["projects"].(map[string]any)
	if !ok {
		projects = map[string]any{}
		existing["projects"] = projects
		changed = true
	}
	for _, dir := range projectDirs {
		if dir == "" {
			continue
		}
		project, ok := projects[dir].(map[string]any)
		if !ok {
			project = map[string]any{}
			projects[dir] = project
			changed = true
		}
		for key, value := range inferenceProjectTrustSeed() {
			if _, ok := project[key]; !ok {
				project[key] = value
				changed = true
			}
		}
	}
	if !changed {
		return
	}
	out, err := json.Marshal(existing)
	if err != nil {
		m.logger.Warn("failed to marshal inference config", "agent", agentName, "path", path, "error", err)
		return
	}
	if err := writeInferenceConfigFile(path, out); err != nil {
		m.logger.Warn("failed to write inference config", "agent", agentName, "path", path, "error", err)
	}
}

func (m *Manager) claudeTrustedProjectDirs(agentName string) []string {
	agentDir := filepath.Join(m.workDir, agentName)
	dirs := []string{agentDir}
	seen := map[string]bool{agentDir: true}
	_ = filepath.WalkDir(agentDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == agentDir {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() == ".git" {
			repoDir := filepath.Dir(path)
			if !seen[repoDir] {
				dirs = append(dirs, repoDir)
				seen[repoDir] = true
			}
			return filepath.SkipDir
		}
		return nil
	})
	return dirs
}

// mergeApprovedAPIKeys ensures every seeded approved API key form is present
// in an existing customApiKeyResponses.approved list. The top-level merge in
// seedJSONFile skips a key that already exists, which would leave configs
// seeded by older hive versions without the truncated key form the CLI
// actually matches against (see apiKeyApprovalSuffixLen).
func (m *Manager) mergeApprovedAPIKeys(agentName, path string) {
	data, err := readInferenceConfigFile(path)
	if err != nil {
		return
	}
	existing := map[string]any{}
	if err := json.Unmarshal(data, &existing); err != nil {
		return
	}
	responses, ok := existing["customApiKeyResponses"].(map[string]any)
	if !ok {
		return
	}
	approved, _ := responses["approved"].([]any)
	present := make(map[string]bool, len(approved))
	for _, v := range approved {
		if s, ok := v.(string); ok {
			present[s] = true
		}
	}
	seedResponses, _ := inferenceUserConfigSeed(agentName)["customApiKeyResponses"].(map[string]any)
	seedApproved, _ := seedResponses["approved"].([]string)
	changed := false
	for _, key := range seedApproved {
		if !present[key] {
			approved = append(approved, key)
			changed = true
		}
	}
	if !changed {
		return
	}
	responses["approved"] = approved
	out, err := json.Marshal(existing)
	if err != nil {
		return
	}
	if err := writeInferenceConfigFile(path, out); err != nil {
		m.logger.Warn("failed to write inference config", "agent", agentName, "path", path, "error", err)
	}
}

// seedClaudeSettingsFile writes or repairs a Claude settings.json, merging in
// the keys from inferenceSettingsSeed (e.g. a file seeded by an older hive
// version without skipDangerousModePermissionPrompt gains the key instead of
// being skipped by an exists-check).
func (m *Manager) seedClaudeSettingsFile(agentName, path string) {
	m.seedJSONFile(agentName, path, inferenceSettingsSeed())
}

// seedJSONFile merges required top-level keys into the JSON object stored at
// path. Missing keys are added, existing keys are never overwritten, and a
// file that fails to parse is rewritten from the seed alone. A complete,
// parseable file is left untouched.
func (m *Manager) seedJSONFile(agentName, path string, seed map[string]any) {
	existing := map[string]any{}
	if data, err := readInferenceConfigFile(path); err == nil {
		if jsonErr := json.Unmarshal(data, &existing); jsonErr != nil {
			m.logger.Warn("inference config unparseable, rewriting",
				"agent", agentName, "path", path, "error", jsonErr)
			existing = map[string]any{}
		}
	}

	needsWrite := false
	for key, value := range seed {
		if _, ok := existing[key]; !ok {
			existing[key] = value
			needsWrite = true
		}
	}
	if !needsWrite {
		return
	}

	data, err := json.Marshal(existing)
	if err != nil {
		m.logger.Warn("failed to marshal inference config", "agent", agentName, "path", path, "error", err)
		return
	}
	if err := writeInferenceConfigFile(path, data); err != nil {
		m.logger.Warn("failed to write inference config", "agent", agentName, "path", path, "error", err)
	}
}

// ensureClaudeSettings creates a per-agent writable HOME directory for inference
// agents with pre-populated .claude/settings.json. Each agent gets its own dir
// to avoid cross-agent permission conflicts when Claude Code creates session
// files. Directories are created 0o700 and chowned to the agent UID where the
// deployment permits it (the hosted container runs as root), falling back to
// 0o777 only where chown is refused — see tightenInferenceHome.
//
// The .claude.json and settings.json seeds are repaired on every call
// (missing keys merged in, corrupt files rewritten) so agents launched by
// older hive versions pick up newly required keys instead of being skipped
// by an exists-check.
func (m *Manager) ensureClaudeSettings(agentName string, uid int) {
	homePath := inferenceHomePath(agentName)
	settingsDir := filepath.Join(homePath, ".claude")
	settingsFile := filepath.Join(settingsDir, "settings.json")

	// SECURITY (audit F12): os.MkdirAll stats components with a
	// symlink-FOLLOWING stat, so a pre-planted link at the predictable anchor
	// redirected everything below — including the root-privileged Chown/Chmod
	// in tightenInferenceHome. mkdirAllNoFollow Lstats every component instead.
	if err := mkdirAllNoFollow(inferenceHomeRoot(), settingsDir, inferenceHomeDirMode); err != nil {
		m.logger.Warn("failed to create claude inference home", "agent", agentName, "error", err)
		return
	}
	// SECURITY (audit F12): prefer an agent-OWNED 0700 home over a world-writable
	// one. The audit's fix is "UID-owned 0700 homes"; the surrounding code
	// assumed that was impossible ("hive runs as dev, not root, so chown is not
	// available"), but the hosted container runs as root and chown succeeds
	// there — verified live. Where it does succeed, no other UID can enter the
	// directory and the symlink cannot be planted in the first place.
	//
	// Falls back to the historical world-writable mode when chown is refused
	// (a genuinely unprivileged deployment), because a home the agent's own UID
	// cannot write is a hard launch failure. The O_NOFOLLOW guards on the
	// writers hold in BOTH cases — this narrows who can reach the path, it is
	// not what closes the finding.
	m.tightenInferenceHome(agentName, homePath, settingsDir, uid)
	// Write (or repair) both settings files: the HOME userSettings file and
	// the standalone file passed via --settings. The CLI honors
	// skipDangerousModePermissionPrompt from either source.
	m.seedClaudeSettingsFile(agentName, settingsFile)
	m.seedClaudeSettingsFile(agentName, claudeInferenceSettingsPath)
	// Pre-populate (or repair) .claude.json so the CLI skips first-run setup.
	userConfigPath := filepath.Join(homePath, ".claude.json")
	m.seedClaudeUserConfig(agentName, userConfigPath)
	m.seedClaudeProjectTrust(agentName, userConfigPath, m.claudeTrustedProjectDirs(agentName))
	// Only widen when the home is NOT agent-owned. tightenInferenceHome may have
	// just established a 0700 home owned by the agent UID; running the widening
	// walk unconditionally would chmod it straight back to 0777 and silently
	// undo the hardening (audit F12).
	if !m.inferenceHomeIsOwnedBy(homePath, uid) {
		m.ensureWorldWritable(homePath)
	}
}

// ensureWorldWritable walks the tree and sets dirs to 0o777, files to 0o666.
//
// SECURITY (audit F12, CWE-59/CWE-61): this walk must never follow a symlink.
//
// The tree it repairs is an agent's own HOME and is world-writable by design,
// so the agent can plant entries in it. os.Chmod FOLLOWS symlinks, so a link
// planted here pointed the hive's own chmod at any file the hive user can
// reach — hive.yaml, key files, state — and turned it 0666. The agent supplies
// the link; the hive supplies the privilege. Verified: a 0600 file outside the
// tree became 0666 through a planted link, and stays 0600 with this guard.
//
// filepath.WalkDir reports entries via Lstat, so the symlink is visible as a
// symlink here; the danger is only in acting on it. Non-regular files (fifos,
// sockets, devices) are skipped for the same reason: chmod on them is never
// something this repair loop intends.
func (m *Manager) ensureWorldWritable(root string) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if d.IsDir() {
			if info.Mode().Perm() != 0o777 {
				_ = os.Chmod(path, 0o777)
			}
		} else if info.Mode().IsRegular() {
			if info.Mode().Perm() != 0o666 {
				_ = os.Chmod(path, 0o666)
			}
		}
		return nil
	})
}

// agentStateFileMode is owner-write, world-read (0644). These files sit in the
// pod-shared /tmp and carry per-agent CONTROL state — the enforcement mode the
// gh wrapper and git credential helper read, and the bootstrap prompt goose is
// launched with — so nothing but the hive uid may WRITE them: anything group-
// or world-writable lets one agent steer another.
//
// They must stay world-READABLE, though: the readers run as the per-agent UID
// (hive-<agent>, group node), not as the hive uid. bin/gh-wrapper.sh and
// bin/git-credential-hive.sh `cat` the mode file, and goose is launched with
// `--text "$(cat /tmp/.hive-bootstrap-<name>.txt)"` from the agent's tmux
// session. When #3172 tightened this to 0600 those readers got EACCES: under
// `set -e` both scripts died mid-flight, so every agent gh call failed and
// every push had no credential (#3679, #3881, #3882). Owner-only can never
// work for a file whose consumer is a different uid by design.
//
// 0644 does not reopen N15's write path: /tmp is sticky, so an agent UID cannot
// unlink, rename or replace a hive-owned file there, and the write itself stays
// O_NOFOLLOW + fchmod-via-descriptor (see writeAgentStateFile). The contents
// were never secret — the mode is the agent's own enforcement level and the
// bootstrap prompt is what the agent prints in its own pane.
const agentStateFileMode = 0o644

// writeAgentStateFile writes per-agent control state to a shared-/tmp path
// without following symlinks.
//
// SECURITY (audit N15, CWE-367/732/20). These files were written with a plain
// os.WriteFile at 0o644, which:
//
//   - follows symlinks — os.WriteFile opens O_WRONLY|O_CREATE|O_TRUNC with no
//     O_NOFOLLOW, so a pre-planted symlink at the (predictable) path redirects
//     the hive's own write to any file the process can reach; and
//   - relied on the caller's umask and on O_CREATE for the mode, so a file left
//     behind by a previous release (or created under a restrictive umask) kept
//     whatever mode it already had, and nothing ever repaired it.
//
// Both matter because the path is derived from the agent NAME, which is
// guessable, and because the readers treat these files as authoritative:
// bin/gh-wrapper.sh takes its enforcement mode from .hive-mode-<name> in
// preference to the trustworthy env var, and goose is launched with whatever
// .hive-bootstrap-<name>.txt contains as its first instruction.
//
// O_NOFOLLOW makes a planted symlink fail loudly instead of redirecting the
// write. The file is deliberately NOT O_EXCL: these are rewritten on every
// mode change and every relaunch, so failing when the file already exists would
// break the normal path.
// inferenceHomeDirMode / inferenceHomeSharedDirMode are the two shapes a
// per-agent inference HOME can take (audit F12).
//
// The 0700 form is preferred and is what tightenInferenceHome establishes once
// the directory is chowned to the agent UID: nobody else can even enter it, so
// the predictable path stops being reachable. The 0777 form is the historical
// fallback for a deployment where chown is refused — the agent must be able to
// write its own HOME, and a home it cannot write is a hard launch failure.
const (
	inferenceHomeDirMode       = 0o700
	inferenceHomeSharedDirMode = 0o777
)

// errInferenceHomeSymlink reports that a path component of an inference HOME is
// a symlink, so the caller must not act on it (audit F12).
var errInferenceHomeSymlink = errors.New("inference home path component is a symlink")

// lstatNoFollow reports whether path exists and is a real directory, refusing a
// symlink (audit F12).
//
// Returns (false, nil) when the path does not exist yet — the normal first-run
// case, which the callers create. Any symlink, at any component, is
// errInferenceHomeSymlink: acting on it is exactly the redirection this guard
// exists to stop.
func lstatNoFollow(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("%w: %s", errInferenceHomeSymlink, path)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("inference home path is not a directory: %s", path)
	}
	return true, nil
}

// mkdirAllNoFollow creates dir and any missing parents BELOW root, refusing to
// traverse or create through a symlink (audit F12).
//
// SECURITY: os.MkdirAll stats each component with a symlink-FOLLOWING stat and
// is happy to treat a link-to-directory as an existing component, so a
// pre-planted link at the predictable anchor
// (/tmp/.claude-inference-home-<agent>) silently redirects the whole subtree —
// and with it the path-based Chown/Chmod in tightenInferenceHome, which run as
// root in the hosted container. Lstat-ing every component below root closes
// that: a planted link fails loudly instead of redirecting.
//
// root is RESOLVED rather than Lstat-refused: it is the operator-controlled
// container path (/tmp, or a test temp dir), and on macOS /tmp is itself a
// symlink to private/tmp — refusing it would break every real launch. The
// threat model is a link planted at the AGENT-controlled anchor below root, so
// resolving root and then Lstat-ing every component beneath it is what matters.
// root always already exists and is never created here.
func mkdirAllNoFollow(root, dir string, perm os.FileMode) error {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	// Re-anchor dir onto the resolved root so Rel compares like with like.
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return err
	}
	root = resolvedRoot
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("inference home %s escapes root %s", dir, root)
	}
	current := root
	if rel == "." {
		return nil
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		exists, err := lstatNoFollow(current)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if err := os.Mkdir(current, perm); err != nil && !os.IsExist(err) {
			return err
		}
		// Re-check: os.Mkdir returning EEXIST means something raced us into
		// place, and that something may be a symlink.
		if _, err := lstatNoFollow(current); err != nil {
			return err
		}
	}
	return nil
}

// inferenceHomeRoot returns the directory the per-agent inference HOMEs are
// created in — /tmp in production, the override's parent under test.
func inferenceHomeRoot() string {
	if inferenceHomePrefixOverride != "" {
		return filepath.Dir(inferenceHomePrefixOverride)
	}
	return filepath.Dir(claudeInferenceHomePrefix)
}

// tightenInferenceHome tries to give the agent UID sole ownership of its
// inference HOME, falling back to the historical world-writable mode when the
// hive lacks the privilege to chown. Best-effort by design: every failure path
// leaves a WORKING home.
//
// SECURITY (audit F12): os.Chown and os.Chmod both FOLLOW symlinks, and this
// runs as root in the hosted container, so each directory is Lstat-verified as
// a real directory immediately before it is acted on. A planted link is skipped
// loudly rather than having the hive's own privilege redirected onto whatever
// it points at. The leaf O_NOFOLLOW writers do NOT cover this: they protect
// files, and these are the ANCHOR DIRECTORIES.
func (m *Manager) tightenInferenceHome(agentName, homePath, settingsDir string, uid int) {
	if uid <= 0 {
		// No per-agent UID: the hive itself is the only writer, so 0700 owned
		// by the hive is already correct.
		return
	}
	for _, dir := range []string{homePath, settingsDir} {
		if exists, err := lstatNoFollow(dir); err != nil || !exists {
			m.logger.Warn("refusing to tighten inference home (not a real directory)",
				"agent", agentName, "dir", dir, "error", err)
			continue
		}
		if err := os.Chown(dir, uid, -1); err != nil {
			// Unprivileged deployment. Restore the shared mode so the agent can
			// still use its HOME, and say so once at debug level rather than
			// warning on every launch.
			m.logger.Debug("inference home left world-writable (chown unavailable)",
				"agent", agentName, "dir", dir, "error", err)
			if cerr := os.Chmod(dir, inferenceHomeSharedDirMode); cerr != nil {
				m.logger.Warn("failed to restore inference home mode",
					"agent", agentName, "dir", dir, "error", cerr)
			}
			continue
		}
		if err := os.Chmod(dir, inferenceHomeDirMode); err != nil {
			m.logger.Warn("failed to tighten inference home mode",
				"agent", agentName, "dir", dir, "error", err)
		}
	}
}

// inferenceHomeIsOwnedBy reports whether homePath is already owned by uid, i.e.
// tightenInferenceHome succeeded and the widening walk must be skipped.
//
// Fails SAFE for availability rather than for security: on any doubt (uid<=0,
// stat error, or a platform without Stat_t) it returns false, so the caller
// widens and the agent keeps a usable HOME. The O_NOFOLLOW writers are what
// close F12 in that case.
func (m *Manager) inferenceHomeIsOwnedBy(homePath string, uid int) bool {
	if uid <= 0 {
		return false
	}
	info, err := os.Stat(homePath)
	if err != nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(st.Uid) == uid
}

// inferenceConfigFileMode is the mode for a per-agent inference config file.
//
// NOT 0600 like agentStateFileMode: these live in a per-agent HOME that the
// AGENT's own UID must be able to rewrite (the CLI updates its own config), and
// the hive cannot chown them to that UID on every deployment shape. 0666 in a
// directory only that agent can enter is the trade-off the surrounding code
// already makes; the symlink guard below is what actually closes audit F12,
// because the exploit was redirection out of the directory, not the mode.
const inferenceConfigFileMode = 0o666

// readInferenceConfigFile reads a per-agent inference config, refusing to
// traverse a symlink (audit F12).
//
// os.ReadFile follows symlinks, so a planted link let an attacker feed the
// CONTENTS of any hive-readable file into the merge logic below, which then
// wrote the merged result back out — a read-and-echo primitive on top of the
// clobber. O_NOFOLLOW makes that fail loudly instead.
//
// A missing file is the normal first-run case; callers already treat any error
// as "start from the seed", so failing closed here costs nothing.
func readInferenceConfigFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only fd; nothing to lose on close error
	return io.ReadAll(f)
}

// writeInferenceConfigFile writes a per-agent inference config without
// following a symlink (audit F12, CWE-59/61).
//
// SECURITY: the inference HOME is a predictable path
// (/tmp/.claude-inference-home-<agent>) inside world-writable /tmp, and the
// directory itself is created world-writable so the agent UID can use it. Both
// writers here used a plain os.WriteFile, which opens O_WRONLY|O_CREATE|O_TRUNC
// with NO O_NOFOLLOW — so another local UID could pre-plant a symlink at the
// config path and have the hive overwrite any file it can reach.
//
// Verified before and after: a planted link at <home>/.claude.json had the
// VICTIM file's contents replaced with the seeded JSON; with this guard the
// write fails with ELOOP and the victim file is untouched.
//
// Note #3432 fixed only the sibling half of F12 — the chmod walk in
// ensureWorldWritable, which WIDENED a linked file's mode. This closes the
// write path, which CLOBBERED its contents. The audit named three sinks; all
// three are now covered.
func writeInferenceConfigFile(path string, data []byte) error {
	f, err := os.OpenFile(path,
		os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW,
		inferenceConfigFileMode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close() // best-effort cleanup; the write error is what's returned
		return err
	}
	return f.Close()
}

func writeAgentStateFile(path string, data []byte) error {
	f, err := os.OpenFile(path,
		os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW,
		agentStateFileMode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close() // best-effort cleanup; the write error is what's returned
		return err
	}
	// O_CREATE honours the mode only when the file did not already exist (and
	// umask-filters it even then), so an existing file from a previous release
	// — 0600 from the #3172 build, or umask-narrowed 0600 (#3882) — keeps its
	// old, agent-unreadable mode without this; the explicit chmod repairs it on
	// the next mode change or kick. Chmod through the still-open descriptor:
	// O_NOFOLLOW only proved the
	// path was not a symlink at OPEN time, so a path-based os.Chmod after Close
	// left a window in shared /tmp where the pathname could be swapped for a
	// symlink and the mode change applied to the link target (TOCTOU, #3175).
	// f.Chmod acts on the inode we opened, closing that window.
	if err := f.Chmod(agentStateFileMode); err != nil {
		_ = f.Close() // best-effort cleanup; the chmod error is what's returned
		return err
	}
	return f.Close()
}
