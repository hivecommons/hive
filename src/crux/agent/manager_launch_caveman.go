package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// installCavemanForAgent runs the backend-specific caveman installer before
// the agent CLI starts. Auto-activating backends (claude, copilot, gemini)
// get caveman wired in so it's active from message one. Per-session backends
// (goose, codex, aider) get the skill pre-installed; activation happens via
// /caveman command sent after launch.
func (m *Manager) installCavemanForAgent(agent *AgentProcess, backend string) {
	mode := agent.Config.CavemanMode
	if mode == "" {
		return
	}

	// AgentHome so caveman's ~/.claude / ~/.copilot writes land in the same
	// HOME the agent's CLI runs with (per-agent layout, bridged to shared).
	home := AgentHome(agent.Name, agent.UID, backend)
	if agent.UID == 0 {
		home = os.Getenv("HOME")
		if home == "" {
			home = "/root"
		}
	}

	m.logger.Info("installing caveman", "agent", agent.Name, "backend", backend, "mode", mode)

	// The installer must run AS THE AGENT USER, not as the hive user. Caveman
	// spawns the backend's own CLI (`claude plugin list` / `plugin install`,
	// caveman bin/install.js), and Claude Code rewrites $HOME/.claude.json
	// wholesale on every invocation. Run as the hive user with the agent's
	// HOME, that CLI cannot read the agent-owned 0600 session file, treats
	// the home as a fresh install, and replaces the signed-in session with a
	// blank skeleton — the agent drops to the login menu on its next start.
	// This is the #4596 wholesale-rewrite clobber reintroduced from inside
	// the manager, through the per-agent home layout that #4619 built to end
	// it. su-exec here matches every other command that touches the per-agent
	// HOME (tmuxCmd, setupCodexHome).
	userSpec := ""
	if agent.UID > 0 {
		userSpec = m.agentExecUserSpec(agent)
	}
	argv := cavemanInstallArgv(backend, mode, userSpec)
	if argv == nil {
		m.logger.Info("caveman not supported for backend", "backend", backend)
		return
	}

	agentDir := filepath.Join("/data/agents", agent.Name)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = agentDir
	npmCache := cavemanNpmCachePath(agentDir, agent.UID)
	// Under su-exec the installer is an agent-UID process: give it the minimal
	// helper environment, not the manager's (which holds hub-only secrets).
	// Without su-exec (UID 0, shared-home setups) it runs as hive itself and
	// keeps the full environment as before.
	if userSpec != "" {
		cmd.Env = agentSubprocessEnv("HOME="+home, "npm_config_cache="+npmCache)
	} else {
		cmd.Env = append(os.Environ(), "HOME="+home, "npm_config_cache="+npmCache)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		m.logger.Warn("caveman install failed", "agent", agent.Name, "error", err, "output", string(out))
	}
}

// cavemanInstallArgv builds the argv for a backend's caveman installer, or
// nil for backends caveman does not support. A non-empty userSpec prefixes
// the command with su-exec so the whole install — including the backend CLIs
// caveman spawns — runs as the agent user (see installCavemanForAgent for
// why that is load-bearing).
func cavemanInstallArgv(backend, mode, userSpec string) []string {
	// Pinned: unpinned HEAD broke every install on 2026-07-27 when upstream
	// removed the --mode flag. Bump deliberately, after checking `--help`.
	// v1.9.1 IS commit 0d95a81d35a9 — the SHA form is deliberately not used
	// because the `skills` CLI clones with `git clone --branch <ref>`, and
	// git cannot clone a bare SHA as a branch ("Could not find remote branch
	// 0d95a81d35a9"), which broke every install on a live hive. Tags clone
	// cleanly; bump the tag, never a raw SHA.
	const cavemanRef = "github:JuliusBrussee/caveman#v1.9.1"
	const skillsRef = "JuliusBrussee/caveman#v1.9.1"
	// Upstream replaced `--mode full|minimal` with `--all` / `--minimal`
	// (--all = hooks + init).
	modeFlag := "--all"
	if mode == "minimal" {
		modeFlag = "--minimal"
	}

	var argv []string
	switch backend {
	case "claude":
		argv = []string{"npx", "-y", cavemanRef, "--", "--only", "claude", modeFlag}
	case "copilot":
		argv = []string{"npx", "-y", cavemanRef, "--", "--only", "copilot", "--with-init", modeFlag}
	case "gemini":
		argv = []string{"npx", "-y", cavemanRef, "--", "--only", "gemini", modeFlag}
	case "goose":
		argv = []string{"npx", "-y", "skills", "add", skillsRef, "-a", "goose", "-y"}
	case "codex":
		argv = []string{"npx", "-y", "skills", "add", skillsRef, "-a", "codex", "-y"}
	case "aider":
		argv = []string{"npx", "-y", "skills", "add", skillsRef, "-a", "aider-desk", "-y"}
	default:
		return nil
	}
	if userSpec != "" {
		argv = append([]string{"su-exec", userSpec}, argv...)
	}
	return argv
}

// cavemanNpmCachePath returns the npm cache dir for an agent's caveman
// install. The shared npm cache under /data/home accumulates
// content-addressed entries owned by whichever UID wrote them first; npx
// then fails with EACCES on those shards and the agent launches without its
// proxy. A per-agent cache can never collide across UIDs.
//
// Caches written before the install ran as the agent user are owned by the
// hive user, and npm EACCESes on them the same way — so a cache owned by
// another UID is removed (best-effort; the hive user owns the old one and
// can), and one that cannot be removed is sidestepped with a per-UID path
// rather than allowed to fail the install.
func cavemanNpmCachePath(agentDir string, uid int) string {
	cache := filepath.Join(agentDir, ".npm-caveman-cache")
	if uid <= 0 {
		return cache
	}
	info, err := os.Stat(cache)
	if err != nil {
		return cache
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) == uid {
		return cache
	}
	if err := os.RemoveAll(cache); err == nil {
		return cache
	}
	return fmt.Sprintf("%s-u%d", cache, uid)
}
