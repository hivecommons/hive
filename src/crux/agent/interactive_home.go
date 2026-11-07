package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// sharedAgentHome is the legacy shared HOME every per-UID agent used before
// the per-agent layout, and remains the anchor all symlink bridges point at.
// Var (not const) as a TEST SEAM, matching the SharedRepoParent convention.
var sharedAgentHome = "/data/home"

// interactiveHomeRootName is the child of the shared home that holds the
// per-agent homes: /data/home/agents/<name>. Living INSIDE /data/home keeps
// it on the same persistent volume as the credentials it bridges to.
const interactiveHomeRootName = "agents"

// interactiveHomeRoot returns the parent directory of all per-agent
// interactive homes.
func interactiveHomeRoot() string {
	return filepath.Join(sharedAgentHome, interactiveHomeRootName)
}

// interactiveHomePath returns the per-agent HOME for a per-UID interactive
// (non-inference) agent.
func interactiveHomePath(agentName string) string {
	return filepath.Join(interactiveHomeRoot(), agentName)
}

// sharedAgentHomeForced reports whether the operator forced the legacy shared
// /data/home layout for all agents via HIVE_SHARED_AGENT_HOME=1.
func sharedAgentHomeForced() bool {
	return os.Getenv("HIVE_SHARED_AGENT_HOME") == "1"
}

// interactiveHomeBridgeDirs are the shared-state directories bridged into each
// per-agent home as symlinks to the same name under sharedAgentHome. These are
// the dirs the entrypoint pre-creates group-writable and the permissions
// watcher reconciles — sharing them is deliberate:
//
//   - .config / .codex — per-tool auth+config the fleet shared safely before
//     the per-agent layout (neither is rewritten wholesale by rename the way
//     .claude.json is, and neither holds per-session or memory state: codex
//     runs under a per-agent CODEX_HOME, see setupCodexHome). .config is also
//     $XDG_CONFIG_HOME: gh's hosts.yml and goose's config.yaml (provider keys)
//     live there, so it stays shared on purpose — it is credential/config
//     state, not session state.
//   - .cache — tool caches; per-agent copies would cold-start every cache on
//     every new agent for no isolation benefit.
//
// DELIBERATELY NOT BRIDGED: .claude.json (the contended session file — the
// whole point), .bash_history (same rename contention, zero sharing value),
// .npm (per-agent npm caches avoid the cross-UID EACCES collisions the shared
// cache suffered — see installCavemanForAgent), .local (#6238 — the
// $XDG_DATA_HOME / $XDG_STATE_HOME root; see setupAgentXDGDirs for why it is
// a REAL per-agent directory with one narrow bridge inside it), and the CLI
// state directories .copilot (hivecommons/hive#9444), .claude, .gemini and
// .bob (hivecommons/hive#17031): each holds BOTH the fleet-shared login and
// the CLI's per-session transcripts and persistent cross-session memory.
// Bridging the whole directory put every agent's state in ONE tree that any
// agent could read and rewrite: Claude Code's user-level ~/.claude/CLAUDE.md,
// history.jsonl and todos/ were a single shared copy, and each agent's
// auto-memory (~/.claude/projects/-data-agents-<name>/memory/MEMORY.md —
// keyed by cwd, so per-agent by path) sat beside every other agent's,
// writable by all of them (and by anything running as the shared uid). See
// perAgentCLIHomes for the REAL per-agent directories with narrow,
// credential-only bridges inside them.
var interactiveHomeBridgeDirs = []string{
	".config", ".codex",
	".cache",
}

// copilotConfigFileName is the one file inside ~/.copilot that must stay
// fleet-shared: Copilot CLI's token map (see copilotConfigHasTokens). Sharing
// only this file — not the whole directory — keeps the "one login
// authenticates the fleet" guarantee without also sharing
// .copilot/session-state (hivecommons/hive#9444).
const copilotConfigFileName = "config.json"

// perAgentCLIHome describes one CLI state directory under $HOME that is a
// REAL per-agent directory, with only the named entries bridged back to the
// same path under sharedAgentHome. The bridge list is the allowlist: it names
// exactly the login/settings files one operator sign-in must propagate to the
// fleet, and nothing else — transcripts, todos, history and auto-memory stay
// per agent (hivecommons/hive#17031). This is the contributor-container
// posture (CLAUDE_STAGE_ALLOWLIST in the Justfile, hivecommons/hive#7836)
// applied to the hive's own agents.
type perAgentCLIHome struct {
	// dir is the directory name under $HOME (".claude").
	dir string
	// bridges are the entries inside dir that symlink to the shared copy.
	// Files are linked even when the shared target does not exist yet (the
	// link goes live on first login, like interactiveHomeBridgeFiles);
	// directories are linked only when the shared target exists, because a
	// CLI's `mkdir -p` through a dangling directory symlink fails with EEXIST.
	bridges []string
	// memoryRel are the per-agent paths, relative to dir, that hold the
	// CLI's persistent cross-session memory. Glob patterns are allowed.
	// Used by the launch-time shared-memory warning and by the memory wipe.
	memoryRel []string
}

// perAgentCLIHomes are the CLI state directories provisioned by
// setupPerAgentCLIHomes. Order is irrelevant; each is independent.
//
//   - .claude — .credentials.json is the shared OAuth token and settings.json
//     the fleet-wide user settings hive itself seeds (claude_remote_control.go,
//     and the auto-memory pin). projects/ (transcripts AND
//     projects/<cwd>/memory/), todos/, history.jsonl, session-env/,
//     shell-snapshots/ and statsig/ are per agent.
//   - .gemini — antigravity-cli/ holds agy's OAuth token and onboarding cache
//     (the entrypoint pre-creates and guards it); settings.json,
//     oauth_creds.json, google_accounts.json and installation_id are the
//     Gemini CLI login/settings. GEMINI.md (the CLI's saved global memory) and
//     tmp/ (checkpoints, session logs) are per agent.
//   - .bob — settings.json (bobshell 1.x) and settings/ (bobshell 2.x) carry
//     the auth-method/provider pins ensureBobAuthSettings asserts; they must
//     stay one file so the pin reaches every bob agent. tmp/ (chat
//     recordings), installation_id and trustedFolders.json are per agent.
//   - .copilot — config.json only (hivecommons/hive#9444).
var perAgentCLIHomes = []perAgentCLIHome{
	{
		dir:       claudeHomeDirName,
		bridges:   []string{claudeCredentialsFileName, claudeSettingsFileName},
		memoryRel: []string{filepath.Join("projects", "*", "memory")},
	},
	{
		dir:       geminiHomeDirName,
		bridges:   []string{"antigravity-cli", "settings.json", "oauth_creds.json", "google_accounts.json", "installation_id"},
		memoryRel: []string{geminiMemoryFileName},
	},
	{
		dir:     bobHomeDirName,
		bridges: []string{"settings.json", "settings"},
	},
	{
		dir:     copilotHomeDirName,
		bridges: []string{copilotConfigFileName},
	},
}

const (
	claudeHomeDirName         = ".claude"
	claudeCredentialsFileName = ".credentials.json"
	claudeSettingsFileName    = "settings.json"
	geminiHomeDirName         = ".gemini"
	// geminiMemoryFileName is the Gemini CLI's global memory file, written by
	// its save_memory tool / `/memory add` under ~/.gemini.
	geminiMemoryFileName = "GEMINI.md"
	bobHomeDirName       = ".bob"
	copilotHomeDirName   = ".copilot"
)

// xdgDataHomeRel / xdgStateHomeRel are the XDG Base Directory defaults
// relative to $HOME. They are exported EXPLICITLY (not left to the spec
// default) so a CLI that resolves XDG_* before HOME, or a wrapper that
// re-homes the process, still lands in the per-agent tree.
const (
	xdgLocalDirName = ".local"
	xdgDataHomeRel  = ".local/share"
	xdgStateHomeRel = ".local/state"
)

// xdgDataSharedBridges are the children of the per-agent $XDG_DATA_HOME that
// are symlinked back to the shared /data/home/.local/share. This is the
// credential allowlist the issue asks for — sharing stays deliberate and
// named, never the default:
//
//   - opencode — `opencode auth login` writes its credential to
//     $XDG_DATA_HOME/opencode/auth.json; one login must reach every agent,
//     the same contract as ~/.claude/.credentials.json.
var xdgDataSharedBridges = []string{"opencode"}

// gooseLogsStateRel is the rolling log directory goose creates under
// $XDG_STATE_HOME on startup. Goose panics when it is missing (the permissions
// watcher pre-creates the shared copy for the same reason — GooseLogsDir), so
// the per-agent one is pre-created at provisioning time too.
const gooseLogsStateRel = "goose/logs/cli"

// agentXDGDataHome returns the per-agent $XDG_DATA_HOME for a given HOME.
func agentXDGDataHome(home string) string {
	return filepath.Join(home, xdgDataHomeRel)
}

// agentXDGStateHome returns the per-agent $XDG_STATE_HOME for a given HOME.
func agentXDGStateHome(home string) string {
	return filepath.Join(home, xdgStateHomeRel)
}

// perAgentXDGHome reports whether the agent's HOME is per-agent, i.e. whether
// XDG_DATA_HOME / XDG_STATE_HOME should be exported beneath it. Under the
// legacy shared HOME (no UID, or HIVE_SHARED_AGENT_HOME=1) nothing is exported
// and the CLIs keep resolving the spec defaults under /data/home.
func perAgentXDGHome(agentName string, uid int, backend string) (string, bool) {
	if uid <= 0 {
		return "", false
	}
	home := AgentHome(agentName, uid, backend)
	if home == "" || home == sharedAgentHome {
		return "", false
	}
	return home, true
}

// interactiveHomeBridgeFiles are shared regular files bridged the same way.
// .bashrc/.profile are the shared shell rc files the entrypoint writes for
// agent panes. Links are created even when the target does not exist yet (a
// dangling link goes live the moment the entrypoint or a later step writes
// the shared file).
//
// DELIBERATELY NOT BRIDGED: .gitconfig (hivecommons/hive#9478 — one shared,
// last-writer-wins global config for the whole fleet; see
// retireSharedGitConfigBridge).
var interactiveHomeBridgeFiles = []string{".bashrc", ".profile"}

// interactiveHomeDirMode / interactiveHomeSharedDirMode mirror the inference-
// home pair: 0700 once chowned to the agent UID (isolation is the point);
// world-writable fallback when the hive runs unprivileged and cannot chown,
// trading isolation for a usable HOME exactly like tightenInferenceHome.
const (
	interactiveHomeDirMode       = 0o700
	interactiveHomeSharedDirMode = 0o777
)

// interactiveHomeRootMode is the mode of /data/home/agents itself: every agent
// UID must traverse it to reach its own home, but nothing is written directly
// in it by agents.
const interactiveHomeRootMode = 0o755

// claudeSessionSeedFileMode is the mode of a freshly seeded per-agent
// .claude.json. The agent's own CLI must rewrite it; when the chown to the
// agent UID fails (unprivileged deployment) the surrounding home is already
// world-writable, so 0666 matches the inferenceConfigFileMode trade-off.
const claudeSessionSeedFileMode = 0o666

// claudeOrphanTmpSweepCap bounds the launch-time sweep of orphaned
// .claude.json.tmp.* files so a pathological accumulation can never stall a
// launch. Ten orphans appeared in one afternoon on the #4596 hive; 200 is
// comfortably above any real backlog while keeping the sweep O(small).
const claudeOrphanTmpSweepCap = 200

// setupInteractiveHome provisions the per-agent HOME for a per-UID interactive
// agent before launch: creates the directory (symlink-safe), bridges shared
// state, seeds the Claude session file from a signed-in source, and sweeps
// legacy orphaned tmp files out of the shared home. Every step is best-effort:
// a partially provisioned home still beats the shared-home contention it
// replaces, and failures are logged rather than blocking the launch.
func (m *Manager) setupInteractiveHome(agent *AgentProcess, backend string) {
	if agent.UID <= 0 || IsInferenceBackend(backend) || sharedAgentHomeForced() {
		return
	}
	home := interactiveHomePath(agent.Name)

	// Parent first (0755: all agents traverse, none write), then the home
	// itself. mkdirAllNoFollow refuses to traverse a planted symlink at any
	// component below the shared-home root (same F12 posture as inference
	// homes).
	if err := mkdirAllNoFollow(sharedAgentHome, interactiveHomeRoot(), interactiveHomeRootMode); err != nil {
		m.logger.Warn("failed to create interactive home root",
			"agent", agent.Name, "dir", interactiveHomeRoot(), "error", err)
		return
	}
	if err := mkdirAllNoFollow(sharedAgentHome, home, interactiveHomeDirMode); err != nil {
		m.logger.Warn("failed to create interactive home",
			"agent", agent.Name, "dir", home, "error", err)
		return
	}

	m.bridgeInteractiveHome(agent.Name, home)
	m.retireSharedGitConfigBridge(agent.Name, home)
	m.setupAgentXDGDirs(agent.Name, home, agent.UID)
	m.setupPerAgentCLIHomes(agent.Name, home, agent.UID)
	m.seedClaudeSessionForAgent(agent, home)
	m.tightenInteractiveHome(agent.Name, home, agent.UID)
	m.sweepOrphanedClaudeTmp(agent.Name)
	m.warnOnStrayGitIdentity(agent.Name, home)
}

// bridgeInteractiveHome creates the symlink bridges from a per-agent home to
// the shared state under sharedAgentHome. Existing correct links are left
// alone; a link pointing elsewhere is replaced; a REAL file or directory at a
// bridge name is never clobbered (the agent may have deliberately localized
// that state — destroying it would lose data).
func (m *Manager) bridgeInteractiveHome(agentName, home string) {
	names := make([]string, 0, len(interactiveHomeBridgeDirs)+len(interactiveHomeBridgeFiles))
	names = append(names, interactiveHomeBridgeDirs...)
	names = append(names, interactiveHomeBridgeFiles...)
	for _, name := range names {
		m.bridgeHomeEntry(agentName, filepath.Join(home, name), filepath.Join(sharedAgentHome, name))
	}
}

// bridgeHomeEntry creates one symlink bridge from link to target with the
// bridgeInteractiveHome semantics: an existing correct link is left alone, a
// link pointing elsewhere is replaced, and a real file or directory is never
// clobbered.
func (m *Manager) bridgeHomeEntry(agentName, link, target string) {
	info, err := os.Lstat(link)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		if existing, rerr := os.Readlink(link); rerr == nil && existing == target {
			return
		}
		if rerr := os.Remove(link); rerr != nil {
			m.logger.Warn("failed to replace stale home bridge",
				"agent", agentName, "link", link, "error", rerr)
			return
		}
	case err == nil:
		// Real file/dir: refuse to clobber.
		return
	case !os.IsNotExist(err):
		m.logger.Warn("failed to inspect home bridge",
			"agent", agentName, "link", link, "error", err)
		return
	}
	if err := os.Symlink(target, link); err != nil && !os.IsExist(err) {
		m.logger.Warn("failed to create home bridge",
			"agent", agentName, "link", link, "error", err)
	}
}

// setupAgentXDGDirs makes $HOME/.local a real per-agent directory holding the
// agent's XDG_DATA_HOME and XDG_STATE_HOME (#6238), retiring the legacy
// symlink bridge to the shared /data/home/.local when one is found, then
// re-bridges only the named shared entries (xdgDataSharedBridges) back into
// the shared tree. Every step is best-effort and idempotent, like the rest of
// provisioning: a real .local that already exists is kept as is (it may hold
// live per-agent state), and only a SYMLINK is replaced — nothing under the
// shared tree is moved or deleted.
func (m *Manager) setupAgentXDGDirs(agentName, home string, uid int) {
	local := filepath.Join(home, xdgLocalDirName)
	if info, err := os.Lstat(local); err == nil && info.Mode()&os.ModeSymlink != 0 {
		// Legacy bridge from before the per-agent layout. Removing the link
		// touches nothing it pointed at.
		if rerr := os.Remove(local); rerr != nil {
			m.logger.Warn("failed to retire legacy shared .local bridge; XDG state stays shared for this agent",
				"agent", agentName, "link", local, "error", rerr)
			return
		}
		m.logger.Info("retired legacy shared .local bridge in favour of per-agent XDG dirs",
			"agent", agentName, "home", home)
	}
	dirs := []string{
		local,
		agentXDGDataHome(home),
		agentXDGStateHome(home),
		filepath.Join(agentXDGStateHome(home), gooseLogsStateRel),
	}
	for _, dir := range dirs {
		if err := mkdirAllNoFollow(sharedAgentHome, dir, interactiveHomeDirMode); err != nil {
			// Permission errors mean we do not own this per-UID home; expected steady state.
			if errors.Is(err, fs.ErrPermission) {
				m.logger.Debug("per-agent XDG dir creation skipped (not owner)",
					"agent", agentName, "dir", dir, "error", err)
				return
			}
			m.logger.Warn("failed to create per-agent XDG dir",
				"agent", agentName, "dir", dir, "error", err)
			return
		}
		m.ownAgentDir(agentName, dir, uid)
	}
	for _, name := range xdgDataSharedBridges {
		m.bridgeHomeEntry(agentName,
			filepath.Join(agentXDGDataHome(home), name),
			filepath.Join(agentXDGDataHome(sharedAgentHome), name))
	}
}

// setupPerAgentCLIHomes provisions every perAgentCLIHomes entry: makes
// $HOME/<dir> a REAL per-agent directory, retiring the legacy whole-directory
// symlink bridge to the shared /data/home/<dir> when one is found, then
// re-bridges only the named login/settings entries back into the shared tree.
//
// The pattern was introduced for .copilot (hivecommons/hive#9444): before it,
// .copilot rode interactiveHomeBridgeDirs as a bare symlink — correct for a
// directory that holds ONLY credential/config state, but Copilot CLI also
// keeps its per-run chat transcripts under .copilot/session-state, so every
// agent's session-state tree was the SAME physical directory. .claude, .gemini
// and .bob had the identical defect with higher stakes (hivecommons/hive#17031):
// Claude Code's user-level CLAUDE.md, history.jsonl and todos/ were one
// shared copy, and every agent's cwd-keyed auto-memory
// (projects/-data-agents-<name>/memory/MEMORY.md) lived in a tree any other
// agent could read or rewrite, forever (it lives on the PVC).
//
// Every step is best-effort and idempotent: a real directory that already
// exists is kept (it may hold live per-agent state), only a SYMLINK is
// replaced, and nothing under the shared tree is moved or deleted — a
// pre-existing shared memory store is left in place and reported once.
func (m *Manager) setupPerAgentCLIHomes(agentName, home string, uid int) {
	for _, spec := range perAgentCLIHomes {
		m.setupPerAgentCLIHome(agentName, home, uid, spec)
	}
}

func (m *Manager) setupPerAgentCLIHome(agentName, home string, uid int, spec perAgentCLIHome) {
	dir := filepath.Join(home, spec.dir)
	shared := filepath.Join(sharedAgentHome, spec.dir)
	if info, err := os.Lstat(dir); err == nil && info.Mode()&os.ModeSymlink != 0 {
		// Legacy whole-directory bridge. Removing the link touches nothing it
		// pointed at (the shared state lives on, reachable by every other
		// agent's own link until they are re-provisioned too).
		if rerr := os.Remove(dir); rerr != nil {
			m.logger.Warn("failed to retire legacy shared CLI home bridge; session state and memory stay shared for this agent",
				"agent", agentName, "link", dir, "error", rerr)
			return
		}
		m.logger.Info("retired legacy shared CLI home bridge in favour of a per-agent directory",
			"agent", agentName, "dir", dir, "shared", shared)
		m.warnSharedCLIMemoryOnce(spec, shared)
	}
	if err := mkdirAllNoFollow(sharedAgentHome, dir, interactiveHomeDirMode); err != nil {
		m.logger.Warn("failed to create per-agent CLI home dir",
			"agent", agentName, "dir", dir, "error", err)
		return
	}
	m.ownAgentDir(agentName, dir, uid)
	for _, name := range spec.bridges {
		link := filepath.Join(dir, name)
		target := filepath.Join(shared, name)
		if _, err := os.Stat(target); err != nil && isPerAgentCLIBridgeDir(spec.dir, name) {
			// Absent target: a dangling FILE link goes live on first login; a
			// dangling DIRECTORY link would make the CLI's own mkdir -p fail,
			// so a directory-shaped bridge waits for the entrypoint to create it.
			m.logger.Debug("shared CLI dir absent; per-agent bridge deferred",
				"agent", agentName, "link", link, "target", target)
			continue
		}
		m.bridgeHomeEntry(agentName, link, target)
	}
}

// perAgentCLIBridgeDirs names the bridge entries that are directories (not
// files) in the shared tree, so an absent target is deferred rather than
// linked dangling. Keyed by "<dir>/<entry>".
var perAgentCLIBridgeDirs = map[string]bool{
	geminiHomeDirName + "/antigravity-cli": true,
	bobHomeDirName + "/settings":           true,
}

func isPerAgentCLIBridgeDir(dir, entry string) bool {
	return perAgentCLIBridgeDirs[dir+"/"+entry]
}

// warnSharedCLIMemoryOnce logs, once per process, that the shared CLI home
// still holds a persistent memory store that every agent used to read and
// write. The data is the operator's and is deliberately NOT deleted or moved;
// the log names the path so the operator can review or drain it
// (hivecommons/hive#17033) and then remove it by hand.
func (m *Manager) warnSharedCLIMemoryOnce(spec perAgentCLIHome, shared string) {
	for _, rel := range spec.memoryRel {
		matches, err := filepath.Glob(filepath.Join(shared, filepath.FromSlash(rel)))
		if err != nil || len(matches) == 0 {
			continue
		}
		m.sharedCLIMemoryWarnOnce.Do(func() {
			m.logger.Warn("shared CLI home still holds a persistent memory store that was shared by every agent; it is no longer read by per-agent homes and was left in place for review",
				"dir", shared, "memory", matches, "see", "hivecommons/hive#17031")
		})
		return
	}
}

// retireSharedGitConfigBridge removes the legacy ~/.gitconfig symlink that
// pointed every per-agent home at the one shared /data/home/.gitconfig
// (hivecommons/hive#9478).
//
// That bridge made the GLOBAL git config a single fleet-wide file: a
// `git config --global user.name` by any agent (and global config outranks
// the system /etc/gitconfig) silently re-attributed every other lane until
// the next agent overwrote it — last-writer-wins commit identity, the bug
// this issue reports. The commit identity itself is now pinned per lane at
// launch via GIT_AUTHOR_*/GIT_COMMITTER_* (agentGitIdentity in
// manager_env.go), which outranks every config layer; retiring the bridge
// removes the shared layer that made the config files misleading in the
// first place. Nothing is lost: /etc/gitconfig carries both the bot identity
// and the git-credential-hive.sh helper for every UID regardless of $HOME
// (hivecommons/hive#5343), which the entrypoint asserts at boot.
//
// Only a symlink pointing at the shared file is removed — a REAL per-agent
// .gitconfig is the agent's own state and is never touched, and removing the
// link touches nothing it pointed at.
func (m *Manager) retireSharedGitConfigBridge(agentName, home string) {
	link := filepath.Join(home, gitConfigFileName)
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return
	}
	if target, rerr := os.Readlink(link); rerr != nil || target != filepath.Join(sharedAgentHome, gitConfigFileName) {
		return
	}
	if err := os.Remove(link); err != nil {
		m.logger.Warn("failed to retire shared .gitconfig bridge; global git config stays fleet-shared for this agent",
			"agent", agentName, "link", link, "error", err)
		return
	}
	m.logger.Info("retired shared .gitconfig bridge; agent reads the system /etc/gitconfig and the pinned GIT_AUTHOR_*/GIT_COMMITTER_* identity",
		"agent", agentName, "home", home)
}

// warnOnStrayGitIdentity logs a WARN when a global git config layer reachable
// from the agent's HOME still declares user.name/user.email
// (hivecommons/hive#9478, proposed fix item 4).
//
// The launch-pinned GIT_AUTHOR_*/GIT_COMMITTER_* env vars outrank every config
// layer, so such a file no longer changes who commits — but it still answers
// `git config --show-origin user.email` with a stale, fleet-shared value, which
// is exactly what made the original investigation chase the wrong lane. Naming
// the file at launch keeps the discrepancy visible instead of silent. The check
// is read-only and best-effort: an unreadable candidate is not a finding.
func (m *Manager) warnOnStrayGitIdentity(agentName, home string) {
	for _, rel := range gitIdentityConfigCandidates {
		path := filepath.Join(home, rel)
		if !gitConfigDeclaresUserIdentity(path) {
			continue
		}
		m.logger.Warn("agent git config declares a user identity outside the system /etc/gitconfig; commits still use the pinned per-lane GIT_AUTHOR_*/GIT_COMMITTER_* identity, but `git config user.email` reads this shared value instead (hivecommons/hive#9478)",
			"agent", agentName, "config", path)
	}
}

// gitConfigFileName is git's global config file in $HOME; the .config/git
// candidate is the XDG-located second global layer (~/.config is itself a
// shared bridge, see interactiveHomeBridgeDirs), which git reads when the
// first is absent.
const gitConfigFileName = ".gitconfig"

var gitIdentityConfigCandidates = []string{gitConfigFileName, filepath.Join(".config", "git", "config")}

// gitConfigDeclaresUserIdentity reports whether a git config file sets
// user.name or user.email. It is a deliberately small INI scan rather than a
// `git config` shell-out: provisioning runs as the hive process, not as the
// agent UID, so a shell-out would resolve a different HOME and a different
// answer than the one this check is about.
func gitConfigDeclaresUserIdentity(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	section := ""
	for _, line := range strings.Split(string(data), "\n") {
		entry := strings.TrimSpace(line)
		if strings.HasPrefix(entry, "[") {
			end := strings.Index(entry, "]")
			if end < 0 {
				continue
			}
			section = strings.ToLower(strings.TrimSpace(entry[1:end]))
			entry = strings.TrimSpace(entry[end+1:])
		}
		if entry == "" || strings.HasPrefix(entry, "#") || strings.HasPrefix(entry, ";") || section != "user" {
			continue
		}
		key := entry
		if eq := strings.Index(key, "="); eq >= 0 {
			key = key[:eq]
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "name", "email":
			return true
		}
	}
	return false
}

// ownAgentDir gives one freshly created per-agent directory to the agent UID
// at interactiveHomeDirMode, with the same unprivileged fallback as
// tightenInteractiveHome: when chown is unavailable, fall back to
// interactiveHomeSharedDirMode so the agent can still write there. mkdir
// already applied the mode subject to umask, so it is re-applied explicitly.
func (m *Manager) ownAgentDir(agentName, dir string, uid int) {
	if uid <= 0 {
		return
	}
	if err := os.Chown(dir, uid, -1); err != nil {
		m.logger.Debug("per-agent dir left world-writable (chown unavailable)",
			"agent", agentName, "dir", dir, "error", err)
		if cerr := os.Chmod(dir, interactiveHomeSharedDirMode); cerr != nil {
			if errors.Is(cerr, fs.ErrPermission) {
				m.logger.Debug("per-agent dir mode restore skipped (not owner)",
					"agent", agentName, "dir", dir, "error", cerr)
				return
			}
			m.logger.Warn("failed to restore per-agent dir mode",
				"agent", agentName, "dir", dir, "error", cerr)
		}
		return
	}
	if err := os.Chmod(dir, interactiveHomeDirMode); err != nil {
		m.logger.Warn("failed to tighten per-agent dir mode",
			"agent", agentName, "dir", dir, "error", err)
	}
}

// seedClaudeSessionForAgent gives a freshly provisioned (or signed-out)
// per-agent home a signed-in Claude session, so existing hives migrate to the
// per-agent layout without anyone re-running /login. Sources, in order:
//
//  1. The legacy shared /data/home/.claude.json — the file the ONE signed-in
//     agent of a pre-migration hive was maintaining.
//  2. Any signed-in sibling under /data/home/agents/*/.claude.json — how the
//     #4606 restart path re-authenticates an agent after an operator logs in
//     on a sibling.
//
// On a FRESH install source (2) is the only one that can ever exist, and it is
// always agent-owned at mode 0600 because the operator's login was written by
// that agent's OWN CLI — there is no legacy shared file to inherit. Reading it
// therefore goes through the su-exec seam in claude_session_adopt.go, the same
// way tmuxCmd and setupCodexHome reach across UIDs. Without that, seeding found
// no source on fresh installs and every agent needed its own interactive login
// (#4637), contradicting the "log in once per method" contract this whole
// layout exists to keep.
//
// ADOPT-ONLY: a signed-in per-agent file is NEVER overwritten, and no
// synthetic session is fabricated — when no signed-in source exists anywhere,
// the login menu is the honest state and must appear. Sources are read whole;
// Claude's atomic rename writes mean a read never observes a torn file.
func (m *Manager) seedClaudeSessionForAgent(agent *AgentProcess, home string) {
	agentName, uid := agent.Name, agent.UID
	target := claudeSessionFile(home)
	// Owner-aware on purpose: an unreadable target may be this agent's own
	// signed-in session, and adopt-only must not clobber it with a sibling's
	// identity just because the hive process cannot open it.
	if m.inspectClaudeSessionForAdoption(target).State == claudeSessionSignedIn {
		return
	}
	source := m.findSignedInClaudeSession(agentName)
	if source == "" {
		return
	}
	data, err := m.readClaudeSessionForAdoption(source)
	if err != nil {
		m.logger.Warn("failed to read claude session seed source",
			"agent", agentName, "source", source, "error", err)
		return
	}
	if err := m.writeClaudeSessionForAgent(target, data, m.agentExecUserSpec(agent)); err != nil {
		m.logger.Warn("failed to seed claude session",
			"agent", agentName, "target", target, "error", err)
		return
	}
	if uid > 0 {
		// Best-effort: unprivileged deployments cannot chown, and the seed mode
		// already keeps the file usable there. A no-op when the write above went
		// through su-exec, which produced an agent-owned file already.
		_ = os.Chown(target, uid, -1)
	}
	m.logger.Info("adopted signed-in claude session for agent",
		"agent", agentName, "source", source)
}

// findSignedInClaudeSession locates a signed-in .claude.json to adopt: legacy
// shared file first, then siblings (sorted for determinism), skipping the
// requesting agent's own home.
//
// Candidates are classified owner-aware, so an agent-owned 0600 sibling — the
// ONLY shape a fresh install's first login ever takes — counts as a source
// instead of reading as claudeSessionUnreadable and being skipped.
func (m *Manager) findSignedInClaudeSession(agentName string) string {
	legacy := claudeSessionFile(sharedAgentHome)
	if m.inspectClaudeSessionForAdoption(legacy).State == claudeSessionSignedIn {
		return legacy
	}
	entries, err := os.ReadDir(interactiveHomeRoot())
	if err != nil {
		return ""
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() && e.Name() != agentName {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		candidate := claudeSessionFile(interactiveHomePath(name))
		if m.inspectClaudeSessionForAdoption(candidate).State == claudeSessionSignedIn {
			return candidate
		}
	}
	return ""
}

// tightenInteractiveHome chowns the per-agent home to the agent UID and closes
// it to 0700, with the same unprivileged fallback as tightenInferenceHome:
// when chown fails, restore a world-writable mode so the home stays usable and
// note it once at debug level.
func (m *Manager) tightenInteractiveHome(agentName, home string, uid int) {
	if uid <= 0 {
		return
	}
	if exists, err := lstatNoFollow(home); err != nil || !exists {
		m.logger.Warn("refusing to tighten interactive home (not a real directory)",
			"agent", agentName, "dir", home, "error", err)
		return
	}
	if err := os.Chown(home, uid, -1); err != nil {
		m.logger.Debug("interactive home left world-writable (chown unavailable)",
			"agent", agentName, "dir", home, "error", err)
		if cerr := os.Chmod(home, interactiveHomeSharedDirMode); cerr != nil {
			// EPERM means we do not own the home (per-UID isolation); nothing to restore.
			if errors.Is(cerr, fs.ErrPermission) {
				m.logger.Debug("interactive home mode restore skipped (not owner)",
					"agent", agentName, "dir", home, "error", cerr)
				return
			}
			m.logger.Warn("failed to restore interactive home mode",
				"agent", agentName, "dir", home, "error", cerr)
		}
		return
	}
	if err := os.Chmod(home, interactiveHomeDirMode); err != nil {
		m.logger.Warn("failed to tighten interactive home mode",
			"agent", agentName, "dir", home, "error", err)
	}
}

// sweepOrphanedClaudeTmp removes orphaned Claude atomic-write temp files
// (.claude.json.tmp.<pid>.<hash>) from the legacy shared home. Under the
// shared layout, a CLI that lost the rename race (or died mid-write) left its
// temp file behind forever — ten accumulated in one afternoon on the #4596
// hive. Under the per-agent layout no NEW orphans land here, so this drains
// the legacy debris. Bounded and best-effort.
func (m *Manager) sweepOrphanedClaudeTmp(agentName string) {
	prefix := filepath.Base(claudeSessionFile(sharedAgentHome)) + ".tmp."
	entries, err := os.ReadDir(sharedAgentHome)
	if err != nil {
		return
	}
	removed := 0
	for _, e := range entries {
		if removed >= claudeOrphanTmpSweepCap {
			break
		}
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		if err := os.Remove(filepath.Join(sharedAgentHome, e.Name())); err == nil {
			removed++
		}
	}
	if removed > 0 {
		m.logger.Info(fmt.Sprintf("swept %d orphaned claude tmp file(s) from shared home", removed),
			"agent", agentName)
	}
}
