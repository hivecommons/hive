package dashboard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

const (
	workspaceCleanupInterval = 1 * time.Hour
	workspaceMaxAge          = 2 * time.Hour
	agentWorkspaceRoot       = "/data/agents"
)

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
	failedDirs := 0
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
				if err := removeAsOwner(entryPath); err != nil {
					logger.Warn("workspace cleanup: failed to remove stale dir",
						"agent", agentName, "dir", name, "age", age.Round(time.Minute), "error", err)
					failedDirs++
					continue
				}
				logger.Info("workspace cleanup: removed stale dir",
					"agent", agentName, "dir", name, "age", age.Round(time.Minute))
				removedDirs++
			} else {
				if err := removeAsOwner(entryPath); err != nil {
					logger.Warn("workspace cleanup: failed to remove stale file",
						"agent", agentName, "file", name, "error", err)
					continue
				}
				removedFiles++
			}
		}
	}

	logger.Info("workspace cleanup complete",
		"removed_dirs", removedDirs, "removed_files", removedFiles, "failed_dirs", failedDirs)

	if audit != nil && (removedDirs > 0 || removedFiles > 0 || failedDirs > 0) {
		audit.Log("system", "workspace_cleanup",
			fmt.Sprintf("removed_dirs=%d removed_files=%d failed=%d", removedDirs, removedFiles, failedDirs), "")
	}
}

// removeAsOwner tries os.RemoveAll first; on permission error (CephFS
// root_squash), it detects the owning UID and retries rm -rf as that user.
func removeAsOwner(path string) error {
	err := os.RemoveAll(path)
	if err == nil {
		return nil
	}
	if !errors.Is(err, syscall.EPERM) && !errors.Is(err, os.ErrPermission) {
		return err
	}

	info, statErr := os.Stat(path)
	if statErr != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return err
	}
	ownerUID := strconv.FormatUint(uint64(stat.Uid), 10)

	cmd := exec.Command("su", "-s", "/bin/sh", "-c", "rm -rf "+path, ownerUID)
	if suErr := cmd.Run(); suErr != nil {
		return fmt.Errorf("os.RemoveAll: %w; su %s rm -rf: %v", err, ownerUID, suErr)
	}
	return nil
}

func isSkippedEntry(name string) bool {
	switch name {
	case ".cache", ".config", ".npm-cache", "bin", "beads.json":
		return true
	}
	return false
}
