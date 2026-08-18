package dashboard

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	// Env knobs for the workspace sweep. Defaults preserve the historical
	// behavior (enabled, hourly sweep, 2h max age); operators can disable the
	// sweep or tune its cadence without a rebuild.
	workspaceCleanupEnabledEnv  = "HIVE_WORKSPACE_CLEANUP_ENABLED"
	workspaceCleanupIntervalEnv = "HIVE_WORKSPACE_CLEANUP_INTERVAL"
	workspaceMaxAgeEnv          = "HIVE_WORKSPACE_CLEANUP_MAX_AGE"

	// workspaceCleanupIntervalDefault is how often the sweep runs.
	workspaceCleanupIntervalDefault = 1 * time.Hour
	// workspaceMaxAgeDefault is how old an entry must be before it is
	// considered stale and removed.
	workspaceMaxAgeDefault = 2 * time.Hour
)

// workspaceCleanupEnabled reports whether the background sweep runs at all.
// On by default; an operator opts out with HIVE_WORKSPACE_CLEANUP_ENABLED=0
// (or false/no/off). Any other value — including unset — keeps it enabled.
func workspaceCleanupEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(workspaceCleanupEnabledEnv))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// workspaceCleanupInterval returns the sweep cadence, from
// HIVE_WORKSPACE_CLEANUP_INTERVAL (Go duration, e.g. "30m") or the default.
func workspaceCleanupInterval() time.Duration {
	return durationFromEnv(workspaceCleanupIntervalEnv, workspaceCleanupIntervalDefault)
}

// workspaceMaxAge returns the staleness threshold, from
// HIVE_WORKSPACE_CLEANUP_MAX_AGE (Go duration, e.g. "6h") or the default.
func workspaceMaxAge() time.Duration {
	return durationFromEnv(workspaceMaxAgeEnv, workspaceMaxAgeDefault)
}

// durationFromEnv parses a Go duration from the named env var, falling back
// to def when unset, unparseable, or non-positive.
func durationFromEnv(env string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(env))
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// agentWorkspaceRoot is the directory swept for stale agent workspace
// artifacts. A package var (not a const) so tests can point it at a temp
// dir; the production value never changes at runtime.
var agentWorkspaceRoot = "/data/agents"

// StartWorkspaceCleanup runs a background loop that periodically sweeps
// /data/agents/*/ for stale workspace artifacts and removes them.
func StartWorkspaceCleanup(ctx context.Context, logger *slog.Logger, audit *AuditLog) {
	if !workspaceCleanupEnabled() {
		logger.Info("workspace cleanup disabled", "env", workspaceCleanupEnabledEnv)
		return
	}

	interval := workspaceCleanupInterval()
	logger.Info("workspace cleanup enabled", "interval", interval, "max_age", workspaceMaxAge())

	sweepWorkspaces(logger, audit)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepWorkspaces(logger, audit)
		}
	}
}

func sweepWorkspaces(logger *slog.Logger, audit *AuditLog) {
	agentDirs, err := os.ReadDir(agentWorkspaceRoot)
	if err != nil {
		logger.Debug("workspace cleanup: cannot read agent root", "path", agentWorkspaceRoot, "error", err)
		return
	}

	removedDirs := 0
	removedFiles := 0
	failed := 0
	maxAge := workspaceMaxAge()
	now := time.Now()

	for _, agentEntry := range agentDirs {
		if !agentEntry.IsDir() {
			continue
		}
		agentName := agentEntry.Name()
		agentPath := filepath.Join(agentWorkspaceRoot, agentName)

		// GIT-CLONE GUARD: a workspace root that IS a git clone must never be
		// swept. On hosted spoke hive-hosted-hosted-available-vllmd-07 (tenant
		// hive z-aiops-unite/ui) the sec-check agent's workspace root was its
		// clone of the tenant repo; this sweep deleted every non-dot top-level
		// entry 73 times since Aug 11, leaving 940 unstaged working-tree
		// deletions (.git survived only because isSkippedEntry skips
		// dot-entries) and wiping the agent's in-flight work every ~2 idle
		// hours. The same sweep removed the quality agent's api-server-work
		// clone. The check is per-workspace root, for a .git DIRECTORY or a
		// .git FILE (linked worktrees use a file).
		//
		// Tradeoff: we skip the whole workspace rather than consulting the git
		// index to remove only untracked entries. Reading the index from this
		// code path would add a dependency and new failure modes (locked or
		// mid-rewrite index, detached worktrees); the sweep exists for
		// scratch-dir hygiene, and a git clone is not scratch.
		if isGitClone(agentPath) {
			logger.Info("workspace cleanup: workspace is a git clone; skipping sweep",
				"agent", agentName)
			continue
		}

		entries, err := os.ReadDir(agentPath)
		if err != nil {
			continue
		}

		agentRemovedDirs := 0
		agentRemovedFiles := 0
		for _, entry := range entries {
			name := entry.Name()
			if isSkippedEntry(name) {
				continue
			}

			entryPath := filepath.Join(agentPath, name)
			info, err := entry.Info()
			if err != nil {
				continue
			}
			age := now.Sub(info.ModTime())
			if age < maxAge {
				continue
			}

			if entry.IsDir() {
				if err := removeTree(entryPath); err != nil {
					logger.Warn("workspace cleanup: failed to remove stale dir",
						"agent", agentName, "dir", name, "age", age.Round(time.Minute), "error", err)
					failed++
					continue
				}
				logger.Info("workspace cleanup: removed stale dir",
					"agent", agentName, "dir", name, "age", age.Round(time.Minute))
				agentRemovedDirs++
			} else {
				if err := removeTree(entryPath); err != nil {
					logger.Warn("workspace cleanup: failed to remove stale file",
						"agent", agentName, "file", name, "error", err)
					failed++
					continue
				}
				// Log file removals too — during the vllmd-07 incident only
				// directory removals were logged, which hid the scale of the
				// working-tree destruction for a week.
				logger.Info("workspace cleanup: removed stale file",
					"agent", agentName, "file", name, "age", age.Round(time.Minute))
				agentRemovedFiles++
			}
		}
		if agentRemovedDirs > 0 || agentRemovedFiles > 0 {
			logger.Info("workspace cleanup: agent sweep summary",
				"agent", agentName, "removed_dirs", agentRemovedDirs, "removed_files", agentRemovedFiles)
		}
		removedDirs += agentRemovedDirs
		removedFiles += agentRemovedFiles
	}

	logger.Info("workspace cleanup complete",
		"removed_dirs", removedDirs, "removed_files", removedFiles, "failed", failed)

	if audit != nil && (removedDirs > 0 || removedFiles > 0 || failed > 0) {
		audit.Log("system", "workspace_cleanup",
			fmt.Sprintf("removed_dirs=%d removed_files=%d failed=%d", removedDirs, removedFiles, failed), "")
	}
}

// removeTree removes a file or directory tree. The hive binary runs as an
// unprivileged user (dev/UID 1001) while agent workspaces are owned by
// per-agent UIDs (hive-scanner, hive-architect, etc.). Strategy:
//  1. Try os.RemoveAll directly (works when group-writable or same owner).
//  2. On EPERM, use su-exec (SUID binary) to run rm -rf as the file owner.
//  3. chmod the tree writable and retry (works when current user owns the files).
//  4. As last resort, shell out to rm -rf as the current user.
func removeTree(path string) error {
	err := os.RemoveAll(path)
	if err == nil {
		return nil
	}
	if !isPermError(err) {
		return err
	}

	// Use su-exec to remove as the file owner — su-exec has the SUID bit,
	// so the unprivileged dev user can switch to agent UIDs.
	ownerName, lookupErr := fileOwnerName(path)
	if lookupErr == nil && ownerName != "" {
		cmd := exec.Command("su-exec", ownerName, "rm", "-rf", path)
		if suErr := cmd.Run(); suErr == nil {
			return nil
		}
	}

	// chmod writable and retry — works when the current user owns the files
	// (e.g. same-user read-only dirs) but not cross-user.
	filepath.WalkDir(path, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		// AUDIT N17 (open across three audits): os.Chmod FOLLOWS symlinks. This
		// walk runs over an agent-writable workspace, so a symlink planted there
		// pointing at any path the cleanup user can chmod — a key file, a config,
		// a binary on a shared mount — gets that TARGET relaxed to 0o660/0o770,
		// not the link. Nothing here needs to chmod a symlink: the link's own mode
		// is irrelevant to RemoveAll (which unlinks it) and its target is by
		// definition outside the tree we are deleting. So skip links entirely.
		//
		// WalkDir does not follow symlinks itself, so d.Type() already reports the
		// link — but d can come from a cached ReadDir entry, and a path can be
		// swapped for a symlink between the readdir and this chmod (TOCTOU). The
		// Lstat is the authoritative, immediately-before check.
		info, lerr := os.Lstat(p)
		if lerr != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if info.IsDir() {
			os.Chmod(p, 0o770)
		} else {
			os.Chmod(p, 0o660)
		}
		return nil
	})
	if retryErr := os.RemoveAll(path); retryErr == nil {
		return nil
	}

	// Last resort: shell out to rm -rf as current user.
	cmd := exec.Command("rm", "-rf", path)
	if shellErr := cmd.Run(); shellErr != nil {
		return fmt.Errorf("os.RemoveAll: %w; su-exec rm (owner=%s, lookup=%v); rm -rf: %v",
			err, ownerName, lookupErr, shellErr)
	}
	return nil
}

// isGitClone reports whether dir is the root of a git checkout: it contains a
// .git entry, either a directory (normal clone) or a file (linked worktree's
// "gitdir:" pointer). Lstat so a symlinked .git also counts — any .git
// presence means the directory holds a working tree the sweep must not touch.
func isGitClone(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// fileOwnerName is defined per-platform in file_owner_unix.go and
// file_owner_windows.go (dd split it out for Windows support).

func isPermError(err error) bool {
	if os.IsPermission(err) {
		return true
	}
	for unwrapped := err; unwrapped != nil; {
		if pe, ok := unwrapped.(*os.PathError); ok {
			if pe.Err == syscall.EPERM || pe.Err == syscall.EACCES {
				return true
			}
			unwrapped = pe.Err
		} else {
			break
		}
	}
	return false
}

func isSkippedEntry(name string) bool {
	switch name {
	// Agent infrastructure dirs — never remove
	case ".cache", ".config", ".npm-cache", "bin":
		return true
	// Agent state files — managed by the hive binary
	case "beads.json", "stats.json":
		return true
	// IDE/editor config dirs — seeded by entrypoint, shared across sessions
	case ".agents", ".cursor", ".windsurf", ".clinerules", ".github", ".opencode":
		return true
	// Copilot session state — managed by the agent runtime
	case ".copilot-session":
		return true
	}
	// Skip hidden dotfiles/dotdirs that aren't workspace artifacts
	if strings.HasPrefix(name, ".") {
		return true
	}
	return false
}
