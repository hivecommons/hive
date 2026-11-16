package main

import (
	"log/slog"
	"os"
	"time"

	"github.com/hivecommons/hive/pkg/sessionprune"
)

// sessionPruneInterval is how often the janitor re-runs. Session directories
// accumulate at roughly 150-200/day on a busy spoke, so nothing is urgent here;
// this only needs to be frequent enough that a restart loop cannot outpace it.
const sessionPruneInterval = 6 * time.Hour

// agentHomesRoot is the parent of the per-agent trees the home sweep walks for
// dated pre-shared snapshots and regenerable caches (hivecommons/hive#9869). It
// is the fixed workspace root every spoke uses; nothing in config points at it.
const agentHomesRoot = "/data/agents"

// agentSessionTreesRoot is the parent of the per-agent interactive homes
// (/data/home/agents/<agent>), each holding its own .copilot/session-state
// (hivecommons/hive#13488).
const agentSessionTreesRoot = "/data/home/agents"

// claudeTmpRoot is where the Claude CLI keeps per-session scratch
// (claude-<uid>/<project>/<session>): the container's temp directory, in its
// writable layer rather than on /data.
var claudeTmpRoot = os.TempDir()

func runSessionPrune(logger *slog.Logger, dir string, maxAge time.Duration) {
	res, err := sessionprune.Prune(dir, maxAge, time.Now(), logger)
	if err != nil {
		logger.Warn("session prune failed", "dir", dir, "error", err)
		return
	}
	// Only speak up when something actually happened. A steady-state spoke
	// prunes nothing on most passes, and a line every 6 hours saying "removed 0"
	// is noise that trains operators to ignore the janitor.
	if res.Removed > 0 || res.Failed > 0 {
		logger.Info("session prune complete",
			"dir", dir,
			"scanned", res.Scanned,
			"removed", res.Removed,
			"failed", res.Failed,
			"retention_days", int(maxAge.Hours()/24))
	}
}

// runAgentHomePrune is the home-sweep counterpart to runSessionPrune: it
// reclaims dated .local-pre-shared-* snapshots and regenerable caches under
// each agent home below root, emitting one audit line per run that actually
// removed something. Like runSessionPrune it stays silent on a no-op pass so
// the janitor does not train operators to ignore it.
func runAgentHomePrune(logger *slog.Logger, root string, maxAge time.Duration) {
	res, err := sessionprune.PruneAgentHomes(root, maxAge, time.Now(), logger)
	if err != nil {
		logger.Warn("agent home prune failed", "root", root, "error", err)
		return
	}
	if res.Removed > 0 || res.Failed > 0 {
		logger.Info("agent home prune complete",
			"root", root,
			"scanned", res.Scanned,
			"removed", res.Removed,
			"failed", res.Failed,
			"retention_days", int(maxAge.Hours()/24))
	}
}

// runAgentSessionTreePrune prunes the per-agent Copilot session-state trees
// with the same retention as the shared tree.
func runAgentSessionTreePrune(logger *slog.Logger, root string, maxAge time.Duration) {
	res, err := sessionprune.PruneAgentSessionTrees(root, maxAge, time.Now(), logger)
	if err != nil {
		logger.Warn("agent session tree prune failed", "root", root, "error", err)
		return
	}
	if res.Removed > 0 || res.Failed > 0 {
		logger.Info("agent session tree prune complete",
			"root", root,
			"scanned", res.Scanned,
			"removed", res.Removed,
			"failed", res.Failed,
			"retention_days", int(maxAge.Hours()/24))
	}
}

// runClaudeTmpPrune reclaims idle Claude CLI session scratch under the temp
// directory, which nothing else bounds and which fills the host's root disk.
func runClaudeTmpPrune(logger *slog.Logger, root string, maxAge time.Duration) {
	res, err := sessionprune.PruneClaudeTmp(root, maxAge, time.Now(), logger)
	if err != nil {
		logger.Warn("claude tmp prune failed", "root", root, "error", err)
		return
	}
	if res.Removed > 0 || res.Failed > 0 {
		logger.Info("claude tmp prune complete",
			"root", root,
			"scanned", res.Scanned,
			"removed", res.Removed,
			"failed", res.Failed,
			"retention_hours", int(maxAge.Hours()))
	}
}
