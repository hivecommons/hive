package sessionprune

// This file extends the janitor beyond CLI session-state directories (see
// prune.go) to the other class of hive-owned regenerable bulk that
// accumulates on the shared /data PVC: dated migration snapshots and tool
// caches left under agent homes (hivecommons/hive#9869). Like Prune, every
// decision here is conservative — age-bounded, symlink-safe, and limited to an
// explicit allow-list — because the sweep runs unattended on a volume that
// also holds credentials, beads, and live repo checkouts that must survive.

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultHomeRetentionDays bounds how long a piece of regenerable bulk under an
// agent home is kept after its last write. It is deliberately longer than
// DefaultRetentionDays (7): a session directory is cheap to lose a week after
// its last write, but a tool cache or a migration snapshot that is still being
// consulted is more expensive to cold-start, so the home sweep waits two weeks
// of inactivity before reclaiming anything.
const DefaultHomeRetentionDays = 14

// preSharedPrefix is the leading name of the dated staging snapshots a
// pre-shared / local-dir migration leaves behind under an agent home
// (e.g. ".local-pre-shared-20260815"). Nothing in this tree creates them
// today, so they are treated as a documented external class and reclaimed
// purely by age — never by trusting a creator to clean up after itself.
const preSharedPrefix = ".local-pre-shared-"

// regenerableCacheRelPaths are the only cache locations the home sweep will
// reclaim, each fully regenerable and never required for correctness. The list
// is intentionally small and explicit. Credentials (.claude, .config,
// .codex, .gemini), session-state, beads, and repo checkouts are NOT here and
// must never be swept — a cache cold-starts, those do not.
var regenerableCacheRelPaths = []string{
	".cache",
	filepath.Join(".npm", "_cacache"),
	filepath.Join(".copilot", "cache"),
}

// PruneAgentHomes reclaims aged, regenerable bulk under each agent home
// directly below root: dated .local-pre-shared-* migration snapshots and the
// well-known regenerable caches in regenerableCacheRelPaths. A target is
// removed only when the most recent write anywhere inside it is older than
// maxAge, mirroring Prune's newest-mtime rule so an idle-but-not-dead cache is
// not deleted out from under a running agent.
//
// A missing root is not an error: a spoke whose agents have never materialised
// a home tree has nothing to sweep, and the janitor must stay quiet there.
//
// maxAge <= 0 disables the sweep and is reported as a no-op, so operators can
// turn it off from config without a code change — exactly like Prune.
func PruneAgentHomes(root string, maxAge time.Duration, now time.Time, logger *slog.Logger) (Result, error) {
	var res Result

	if root == "" || maxAge <= 0 {
		return res, nil
	}

	agents, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return res, nil
		}
		return res, err
	}

	cutoff := now.Add(-maxAge)

	for _, agent := range agents {
		// Only descend into real agent-home directories. A loose file or a
		// symlink at the root is not a home and must be left untouched.
		if !agent.IsDir() {
			continue
		}
		home := filepath.Join(root, agent.Name())
		pruneHomeTargets(home, cutoff, &res, logger)
	}

	return res, nil
}

// pruneHomeTargets reclaims the aged pre-shared snapshots and regenerable
// caches for a single agent home. A home that cannot be read is skipped rather
// than failed: an unreadable home must never wedge the sweep across the rest of
// the fleet.
func pruneHomeTargets(home string, cutoff time.Time, res *Result, logger *slog.Logger) {
	entries, err := os.ReadDir(home)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logger.Debug("home prune: cannot read agent home, skipping", "home", home, "err", err)
		}
		return
	}

	var targets []string

	// (a) Dated pre-shared migration snapshots: .local-pre-shared-*. entry.IsDir
	// is false for a symlink, so a bridged or stray link by that name is never
	// matched here.
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), preSharedPrefix) {
			targets = append(targets, filepath.Join(home, e.Name()))
		}
	}

	// (b) Well-known regenerable caches, addressed by their fixed relative path.
	for _, rel := range regenerableCacheRelPaths {
		targets = append(targets, filepath.Join(home, rel))
	}

	for _, path := range targets {
		pruneHomeTarget(path, cutoff, res, logger)
	}
}

// pruneHomeTarget removes a single target directory when its newest write is
// older than cutoff. A missing target is silent (most homes have none of
// these), a symlink is never followed, and any stat/walk failure keeps the
// target — deleting on an unreadable path is how a transient NFS hiccup turns
// into data loss.
func pruneHomeTarget(path string, cutoff time.Time, res *Result, logger *slog.Logger) {
	info, err := os.Lstat(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logger.Debug("home prune: stat failed, keeping", "path", path, "err", err)
		}
		return
	}

	// Never follow a symlink. The per-agent ~/.cache is a bridge to the shared
	// /data/home/.cache on the interactive-home layout; reclaiming it through
	// one agent's home would both unlink the bridge and leave the shared cache
	// behind, the opposite of the intent.
	if info.Mode()&fs.ModeSymlink != 0 {
		return
	}
	if !info.IsDir() {
		return
	}

	res.Scanned++

	newest, err := newestModTime(path)
	if err != nil {
		logger.Debug("home prune: walk failed, keeping", "path", path, "err", err)
		return
	}
	if !newest.Before(cutoff) {
		return
	}

	if err := os.RemoveAll(path); err != nil {
		res.Failed++
		logger.Warn("home prune: remove failed", "path", path, "err", err)
		return
	}
	res.Removed++
}
