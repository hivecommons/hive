package dashboard

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

const (
	workspaceCleanupInterval = 1 * time.Hour
	workspaceMaxAge          = 2 * time.Hour
	agentWorkspaceRoot       = "/data/agents"
)

// StartWorkspaceCleanup runs a background loop that periodically sweeps
// /data/agents/*/ for stale workspace artifacts and removes them.
// This prevents unbounded disk growth from agents that create repo clones,
// Go caches, working directories, and temporary files during tasks.
func StartWorkspaceCleanup(ctx context.Context, logger *slog.Logger) {
	logger.Info("workspace cleanup enabled", "interval", workspaceCleanupInterval, "max_age", workspaceMaxAge)

	sweepWorkspaces(logger)

	ticker := time.NewTicker(workspaceCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepWorkspaces(logger)
		}
	}
}

func sweepWorkspaces(logger *slog.Logger) {
	agentDirs, err := os.ReadDir(agentWorkspaceRoot)
	if err != nil {
		logger.Debug("workspace cleanup: cannot read agent root", "path", agentWorkspaceRoot, "error", err)
		return
	}

	removedDirs := 0
	removedFiles := 0
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
				if err := os.RemoveAll(entryPath); err != nil {
					logger.Warn("workspace cleanup: failed to remove stale dir",
						"agent", agentName, "dir", name, "age", age.Round(time.Minute), "error", err)
					continue
				}
				logger.Info("workspace cleanup: removed stale dir",
					"agent", agentName, "dir", name, "age", age.Round(time.Minute))
				removedDirs++
			} else {
				if err := os.Remove(entryPath); err != nil {
					logger.Warn("workspace cleanup: failed to remove stale file",
						"agent", agentName, "file", name, "error", err)
					continue
				}
				removedFiles++
			}
		}
	}

	logger.Info("workspace cleanup complete", "removed_dirs", removedDirs, "removed_files", removedFiles)
}

func isSkippedEntry(name string) bool {
	switch name {
	case ".cache", ".config", ".npm-cache", "bin", "beads.json":
		return true
	}
	return false
}
