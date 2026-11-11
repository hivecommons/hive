package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// sharedClaudeHomeForced reports whether the operator asked for every agent's
// ~/.claude to stay ONE whole-directory link to the shared /data/home/.claude
// (HIVE_SHARED_CLAUDE_HOME=1), the layout from before hivecommons/hive#17031.
//
// It exists for the Claude login. Claude Code saves a refreshed OAuth token by
// renaming a temp file over .credentials.json. In a per-agent ~/.claude that
// rename replaces the credential link with a private file, so the refresh
// (which also rotates the grant) never reaches the shared login and the rest
// of the fleet is left on a dead one. With the whole directory linked, the
// rename happens inside the shared directory and every agent reads the new
// token, exactly as before #17031. The price is that Claude agents share one
// CLAUDE.md, history.jsonl and todos/ again, and can read each other's
// transcripts and auto-memory.
func sharedClaudeHomeForced() bool {
	return os.Getenv("HIVE_SHARED_CLAUDE_HOME") == "1"
}

// claudeHomeAsideInfix names a per-agent ~/.claude that bridgeSharedClaudeHome
// moved aside: <home>/.claude.per-agent-<UTC timestamp>.
const claudeHomeAsideInfix = ".per-agent-"

// bridgeSharedClaudeHome makes $HOME/.claude a whole-directory link to the
// shared /data/home/.claude (HIVE_SHARED_CLAUDE_HOME=1, see
// sharedClaudeHomeForced). It reports whether the link is in place; on false
// the caller provisions the per-agent directory as usual, so a home this
// cannot convert is never left without a .claude.
//
// A per-agent directory found at the path is moved aside to
// .claude.per-agent-<UTC timestamp> in the same home, never deleted: it holds
// the agent's transcripts and auto-memory from the per-agent layout. Nothing
// under the shared tree is touched. A link is only created when the shared
// directory exists, because the CLI's own mkdir -p fails through a dangling
// directory link.
func (m *Manager) bridgeSharedClaudeHome(agentName, home string) bool {
	dir := filepath.Join(home, claudeHomeDirName)
	shared := filepath.Join(sharedAgentHome, claudeHomeDirName)
	if info, err := os.Stat(shared); err != nil || !info.IsDir() {
		m.logger.Warn("HIVE_SHARED_CLAUDE_HOME=1 but the shared Claude directory is missing; keeping the per-agent directory",
			"agent", agentName, "shared", shared, "error", err)
		return false
	}
	aside := ""
	info, err := os.Lstat(dir)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		if existing, rerr := os.Readlink(dir); rerr == nil && existing == shared {
			return true
		}
		if rerr := os.Remove(dir); rerr != nil {
			m.logger.Warn("HIVE_SHARED_CLAUDE_HOME=1 but a stale .claude link could not be replaced",
				"agent", agentName, "link", dir, "error", rerr)
			return false
		}
	case err == nil && info.IsDir():
		moved, rerr := moveAsidePerAgentClaudeHome(dir, claudeBridgeNow())
		if rerr != nil {
			m.logger.Warn("HIVE_SHARED_CLAUDE_HOME=1 but the per-agent Claude directory could not be moved aside; keeping it",
				"agent", agentName, "dir", dir, "error", rerr)
			return false
		}
		aside = moved
	case err == nil:
		m.logger.Warn("HIVE_SHARED_CLAUDE_HOME=1 but .claude is neither a directory nor a link; leaving it",
			"agent", agentName, "path", dir)
		return false
	case !os.IsNotExist(err):
		m.logger.Warn("HIVE_SHARED_CLAUDE_HOME=1 but .claude could not be inspected",
			"agent", agentName, "path", dir, "error", err)
		return false
	}
	if err := os.Symlink(shared, dir); err != nil {
		if existing, rerr := os.Readlink(dir); rerr == nil && existing == shared {
			// Another launch of the same agent linked it first.
			return true
		}
		m.logger.Warn("HIVE_SHARED_CLAUDE_HOME=1 but the shared Claude directory could not be linked",
			"agent", agentName, "link", dir, "shared", shared, "error", err)
		if aside != "" {
			// Put the per-agent directory back so the agent is not left
			// without a .claude.
			if rerr := os.Rename(aside, dir); rerr != nil {
				m.logger.Error("the per-agent Claude directory could not be put back",
					"agent", agentName, "from", aside, "to", dir, "error", rerr)
			}
		}
		return false
	}
	if aside != "" {
		m.logger.Info("linked ~/.claude to the shared Claude directory (HIVE_SHARED_CLAUDE_HOME=1); the per-agent directory was moved aside, not deleted",
			"agent", agentName, "link", dir, "shared", shared, "moved_to", aside)
	} else {
		m.logger.Info("linked ~/.claude to the shared Claude directory (HIVE_SHARED_CLAUDE_HOME=1)",
			"agent", agentName, "link", dir, "shared", shared)
	}
	return true
}

// moveAsidePerAgentClaudeHome renames dir to a unique
// <dir>.per-agent-<UTC timestamp> beside it and returns the new path.
func moveAsidePerAgentClaudeHome(dir string, now time.Time) (string, error) {
	base := dir + claudeHomeAsideInfix + now.UTC().Format("20060102T150405Z")
	for i := 0; i < 100; i++ {
		dst := base
		if i > 0 {
			dst = fmt.Sprintf("%s-%d", base, i)
		}
		_, err := os.Lstat(dst)
		if err == nil {
			continue
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		if err := os.Rename(dir, dst); err != nil {
			return "", err
		}
		return dst, nil
	}
	return "", fmt.Errorf("no free name to move %s aside", dir)
}
