package dashboard

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	workspaceCleanupInterval = 1 * time.Hour
	workspaceMaxAge          = 2 * time.Hour
)

// agentWorkspaceRoot is the directory swept for stale agent workspace
// artifacts. A package var (not a const) so tests can point it at a temp
// dir; the production value never changes at runtime.
var agentWorkspaceRoot = "/data/agents"

// StartWorkspaceCleanup runs a background loop that periodically sweeps
// /data/agents/*/ for stale workspace artifacts and removes them.
func StartWorkspaceCleanup(ctx context.Context, logger *slog.Logger, audit *AuditLog) {
	logger.Info("workspace cleanup enabled", "interval", workspaceCleanupInterval, "max_age", workspaceMaxAge)

	sweepWorkspaces(logger, audit)

	ticker := time.NewTicker(workspaceCleanupInterval)
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
	now := time.Now()

	for _, agentEntry := range agentDirs {
		if !agentEntry.IsDir() {
			continue
		}
		agentName := agentEntry.Name()
		agentPath := filepath.Join(agentWorkspaceRoot, agentName)

		entries, err := os.ReadDir(agentPath)
		if err != nil {
			continue
		}

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
			if age < workspaceMaxAge {
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
				removedDirs++
			} else {
				if err := removeTree(entryPath); err != nil {
					logger.Warn("workspace cleanup: failed to remove stale file",
						"agent", agentName, "file", name, "error", err)
					failed++
					continue
				}
				removedFiles++
			}
		}
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
		if d.IsDir() {
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

// fileOwnerName returns the username that owns the given path.
func fileOwnerName(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("not a unix stat")
	}
	u, err := user.LookupId(strconv.FormatUint(uint64(stat.Uid), 10))
	if err != nil {
		return "", err
	}
	return u.Username, nil
}

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
