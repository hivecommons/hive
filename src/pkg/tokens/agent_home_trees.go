package tokens

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// perAgentHomesDirName is the directory under the shared HOME root that holds
// the per-agent HOMEs (crux/agent interactiveHomeRootName): each per-UID agent
// runs with HOME=<shared-home>/agents/<name>.
const perAgentHomesDirName = "agents"

// claudeProjectsRel is Claude Code's session+memory tree under a HOME.
const claudeProjectsRel = ".claude/projects"

// bobHomeRel is bobshell's state tree under a HOME.
const bobHomeRel = ".bob"

// SessionTree is one CLI session-state directory to scan plus the agent that
// owns it ("" for the legacy shared directory, where ownership is inferred
// per file).
type SessionTree struct {
	Dir   string
	Agent string
}

// perAgentSessionTrees returns sharedDir plus every per-agent tree beside it.
// rel is the CLI's state path under a HOME (".claude/projects"); the per-agent
// trees are derived only when sharedDir ends in rel, so an operator pointing
// the scanner at an arbitrary directory gets exactly that directory.
//
// Since hivecommons/hive#9444 (.copilot) and hivecommons/hive#17031
// (.claude / .gemini / .bob) each per-UID agent's CLI state is a REAL
// directory under its own HOME rather than a symlink into the shared one, so
// a scan of the shared path alone would miss every new session.
func perAgentSessionTrees(sharedDir, rel string) []SessionTree {
	if sharedDir == "" {
		return nil
	}
	trees := []SessionTree{{Dir: sharedDir}}
	rel = filepath.FromSlash(rel)
	clean := filepath.Clean(sharedDir)
	if !strings.HasSuffix(clean, string(filepath.Separator)+rel) {
		return trees
	}
	home := filepath.Clean(strings.TrimSuffix(clean, string(filepath.Separator)+rel))
	matches, err := filepath.Glob(filepath.Join(home, perAgentHomesDirName, "*", rel))
	if err != nil {
		return trees
	}
	sort.Strings(matches)
	// The CLI's top-level state dir under the agent HOME (".claude" of
	// ".claude/projects"): when it is still a legacy symlink bridge into the
	// shared home, the match IS the shared tree and scanning it again would
	// double-count every shared session under the wrong owner.
	top := rel
	if i := strings.Index(rel, string(filepath.Separator)); i >= 0 {
		top = rel[:i]
	}
	for _, m := range matches {
		agentHome := strings.TrimSuffix(m, string(filepath.Separator)+rel)
		agent := filepath.Base(agentHome)
		if agent == "" || agent == "." || strings.HasPrefix(agent, ".") {
			continue
		}
		if info, err := os.Lstat(filepath.Join(agentHome, top)); err != nil || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		trees = append(trees, SessionTree{Dir: m, Agent: agent})
	}
	return trees
}

// ClaudeSessionTrees returns the configured claude_sessions_dir plus every
// per-agent <home>/agents/<name>/.claude/projects beside it
// (hivecommons/hive#17031).
func ClaudeSessionTrees(sharedDir string) []SessionTree {
	return perAgentSessionTrees(sharedDir, claudeProjectsRel)
}

// BobSessionTrees returns the configured bob_sessions_dir plus every
// per-agent <home>/agents/<name>/.bob beside it (hivecommons/hive#17031).
func BobSessionTrees(sharedDir string) []SessionTree {
	return perAgentSessionTrees(sharedDir, bobHomeRel)
}
