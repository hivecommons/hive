package agent

// Per-agent HOME for interactive CLI backends (#4596).
//
// THE DEFECT: every per-UID agent used to share HOME=/data/home. Claude Code
// rewrites $HOME/.claude.json WHOLESALE via atomic write (tmp file + rename),
// and rename needs only DIRECTORY write permission — /data/home is 2775
// dev:node, so ANY agent could replace the file regardless of its mode. An
// unauthenticated agent's rewrite stripped oauthAccount from under the
// authenticated one, and the whole fleet fell back to the login menu. File
// permissions can never fix this (measured in #4596: group-writable made it
// WORSE), because the contention is on the directory entry, not the file.
//
// THE FIX: each per-UID interactive agent gets its own HOME under
// /data/home/agents/<name>, mirroring the inferenceHomePath / setupCodexHome
// per-agent precedents. Inside it, SYMLINK BRIDGES point the tool state that
// is safe (or required) to share back at /data/home — most importantly
// ~/.claude -> /data/home/.claude, which holds .credentials.json (the OAuth
// TOKEN). The token was never the contended file; sharing it is what lets ONE
// interactive login authenticate the whole fleet. Only the per-writer session
// file (~/.claude.json) becomes truly per-agent, seeded at launch from a
// signed-in source so existing hives migrate without any operator step.
//
// SYNERGY WITH #4606: the capped token-triggered restart fires for an agent
// stuck at the login menu while the shared credential is valid. Under this
// layout the restart re-provisions the agent's home, adopts a signed-in
// session from the legacy shared file or a sibling, and the agent comes back
// authenticated — the restart theory #4606 documented as unproven becomes true.
//
// ESCAPE HATCH: HIVE_SHARED_AGENT_HOME=1 restores the legacy shared-HOME
// behavior wholesale (AgentHome and provisioning both honor it).

import (
	"fmt"
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
//   - .claude   — holds .credentials.json (the shared OAuth token) and
//     settings.json. One login authenticates the fleet.
//   - .copilot / .config / .codex / .bob / .gemini — per-tool auth+config the
//     fleet shared safely before this change (none are rewritten wholesale by
//     rename the way .claude.json is).
//   - .cache / .local — tool caches; per-agent copies would cold-start every
//     cache on every new agent for no isolation benefit.
//
// DELIBERATELY NOT BRIDGED: .claude.json (the contended session file — the
// whole point), .bash_history (same rename contention, zero sharing value),
// .npm (per-agent npm caches avoid the cross-UID EACCES collisions the shared
// cache suffered — see installCavemanForAgent).
var interactiveHomeBridgeDirs = []string{
	".claude", ".copilot", ".config", ".codex", ".bob", ".gemini",
	".cache", ".local",
}

// interactiveHomeBridgeFiles are shared regular files bridged the same way.
// .gitconfig carries the git identity + the git-credential-hive.sh helper
// wiring the entrypoint writes with HOME=/data/home; without the bridge every
// per-agent git push would lose its credential helper. .bashrc/.profile are
// the shared shell rc files the entrypoint writes for agent panes. Links are
// created even when the target does not exist yet (a dangling link goes live
// the moment the entrypoint or a later step writes the shared file).
var interactiveHomeBridgeFiles = []string{".gitconfig", ".bashrc", ".profile"}

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
	m.seedClaudeSessionForAgent(agent.Name, home, agent.UID)
	m.tightenInteractiveHome(agent.Name, home, agent.UID)
	m.sweepOrphanedClaudeTmp(agent.Name)
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
		link := filepath.Join(home, name)
		target := filepath.Join(sharedAgentHome, name)
		info, err := os.Lstat(link)
		switch {
		case err == nil && info.Mode()&os.ModeSymlink != 0:
			if existing, rerr := os.Readlink(link); rerr == nil && existing == target {
				continue
			}
			if rerr := os.Remove(link); rerr != nil {
				m.logger.Warn("failed to replace stale home bridge",
					"agent", agentName, "link", link, "error", rerr)
				continue
			}
		case err == nil:
			// Real file/dir: refuse to clobber.
			continue
		case !os.IsNotExist(err):
			m.logger.Warn("failed to inspect home bridge",
				"agent", agentName, "link", link, "error", err)
			continue
		}
		if err := os.Symlink(target, link); err != nil && !os.IsExist(err) {
			m.logger.Warn("failed to create home bridge",
				"agent", agentName, "link", link, "error", err)
		}
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
// ADOPT-ONLY: a signed-in per-agent file is NEVER overwritten, and no
// synthetic session is fabricated — when no signed-in source exists anywhere,
// the login menu is the honest state and must appear. Sources are read whole;
// Claude's atomic rename writes mean a read never observes a torn file.
func (m *Manager) seedClaudeSessionForAgent(agentName, home string, uid int) {
	target := claudeSessionFile(home)
	if inspectClaudeSession(target).State == claudeSessionSignedIn {
		return
	}
	source := m.findSignedInClaudeSession(agentName)
	if source == "" {
		return
	}
	data, err := os.ReadFile(source)
	if err != nil {
		m.logger.Warn("failed to read claude session seed source",
			"agent", agentName, "source", source, "error", err)
		return
	}
	if err := os.WriteFile(target, data, claudeSessionSeedFileMode); err != nil {
		m.logger.Warn("failed to seed claude session",
			"agent", agentName, "target", target, "error", err)
		return
	}
	if uid > 0 {
		// Best-effort: unprivileged deployments cannot chown, and the 0666
		// mode above already keeps the file usable there.
		_ = os.Chown(target, uid, -1)
	}
	m.logger.Info("adopted signed-in claude session for agent",
		"agent", agentName, "source", source)
}

// findSignedInClaudeSession locates a signed-in .claude.json to adopt: legacy
// shared file first, then siblings (sorted for determinism), skipping the
// requesting agent's own home.
func (m *Manager) findSignedInClaudeSession(agentName string) string {
	legacy := claudeSessionFile(sharedAgentHome)
	if inspectClaudeSession(legacy).State == claudeSessionSignedIn {
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
		if inspectClaudeSession(candidate).State == claudeSessionSignedIn {
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
