package agent

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hivecommons/hive/pkg/jev"
)

// installJevForAgent seeds the jev-decide skill into the agent's CLI home
// before launch when jev_mode is assist (hivecommons/hive#8939). The caveman
// analogue (installCavemanForAgent) runs an external installer; Jev's skill
// is a single embedded SKILL.md, so it is written directly — AS the agent
// user, for the same reason setupCodexHome does: the manager runs as dev and
// cannot chown, and a dev-owned file under a per-UID home is exactly the
// ownership drift #4596 fixed. With jev_mode off nothing is written, and a
// SKILL.md left by an earlier assist run is removed so the agent stops
// advertising a tool that would now only be refused.
func (m *Manager) installJevForAgent(agent *AgentProcess, backend string) {
	home := AgentHome(agent.Name, agent.UID, backend)
	if agent.UID == 0 {
		home = os.Getenv("HOME")
		if home == "" {
			home = "/root"
		}
	}
	dir := jevSkillDir(home, codexHomePath(agent.Name), backend)
	if !agent.Config.JevEnabled() {
		if dir == "" {
			return
		}
		removed, err := removeJevSkill(dir, m.agentExecUserSpec(agent))
		if err != nil {
			m.logger.Warn("jev skill removal failed; agent keeps a stale skill file", "agent", agent.Name, "backend", backend, "dir", dir, "error", err)
			return
		}
		if removed {
			m.logger.Info("removed jev skill (jev_mode off)", "agent", agent.Name, "backend", backend, "dir", dir)
		}
		return
	}
	if dir == "" {
		m.logger.Info("jev skill not supported for backend; agent keeps HIVE_JEV_MODE and can still run `hive jev decide`",
			"agent", agent.Name, "backend", backend)
		return
	}
	if err := installJevSkill(dir, m.agentExecUserSpec(agent)); err != nil {
		m.logger.Warn("jev skill install failed", "agent", agent.Name, "backend", backend, "dir", dir, "error", err)
		return
	}
	m.logger.Info("installed jev skill", "agent", agent.Name, "backend", backend, "dir", dir)
}

// jevSkillDir resolves where backend reads skills from: under CODEX_HOME for
// codex (the manager exports a per-agent one), under HOME for the rest. Empty
// when the backend has no known skills directory.
func jevSkillDir(home, codexHome, backend string) string {
	rel := jev.SkillRelDir(backend)
	if rel == "" {
		return ""
	}
	if backend == codexBackend {
		return filepath.Join(codexHome, filepath.FromSlash(rel))
	}
	return filepath.Join(home, filepath.FromSlash(rel))
}

// installJevSkill writes SKILL.md into dir, creating it. A non-empty userSpec
// runs both steps through su-exec as that user.
func installJevSkill(dir, userSpec string) error {
	path := filepath.Join(dir, jev.SkillFile)
	if userSpec == "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		return os.WriteFile(path, jev.SkillMarkdown(), 0o644)
	}
	if out, err := exec.Command("su-exec", userSpec, "mkdir", "-p", dir).CombinedOutput(); err != nil {
		return fmt.Errorf("mkdir %s as %s: %w: %s", dir, userSpec, err, string(out))
	}
	return writeFileAsUser(userSpec, path, jev.SkillMarkdown())
}

// removeJevSkill deletes SKILL.md from dir — only the file the hive wrote,
// never siblings a user may have added — and then the directory if that left
// it empty. A missing file is not an error (nothing was installed). A
// non-empty userSpec runs the removal through su-exec as that user, mirroring
// installJevSkill: per-UID homes are agent-owned 0700, so the manager cannot
// even stat them itself. Reports whether a file was removed.
func removeJevSkill(dir, userSpec string) (bool, error) {
	path := filepath.Join(dir, jev.SkillFile)
	if userSpec == "" {
		err := os.Remove(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		_ = os.Remove(dir) // fails when not empty; that is the point
		return err == nil, nil
	}
	script := `if [ -e "$1" ]; then rm -f -- "$1" && echo removed; fi; rmdir -- "$2" 2>/dev/null; exit 0`
	out, err := exec.Command("su-exec", userSpec, "sh", "-c", script, "sh", path, dir).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("removing %s as %s: %w: %s", path, userSpec, err, string(out))
	}
	return strings.TrimSpace(string(out)) == "removed", nil
}
