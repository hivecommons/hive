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
// /data/agents/*/ for stale git repository clones and removes them.
// This prevents unbounded disk growth from scanner and other agents that
// clone repos for each task but never clean up.
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

	removed := 0
	now := time.Now()

	for _, agentEntry := range agentDirs {
		if !agentEntry.IsDir() {
			continue
		}
		agentName := agentEntry.Name()
		agentPath := filepath.Join(agentWorkspaceRoot, agentName)

		subdirs, err := os.ReadDir(agentPath)
		if err != nil {
			continue
		}

		for _, sub := range subdirs {
			if !sub.IsDir() {
				continue
			}
			dirName := sub.Name()
			if isSkippedDir(dirName) {
				continue
			}

			dirPath := filepath.Join(agentPath, dirName)
			gitPath := filepath.Join(dirPath, ".git")

			if _, err := os.Stat(gitPath); err != nil {
				continue
			}

			info, err := sub.Info()
			if err != nil {
				continue
			}
			age := now.Sub(info.ModTime())
			if age < workspaceMaxAge {
				continue
			}

			if err := os.RemoveAll(dirPath); err != nil {
				logger.Warn("workspace cleanup: failed to remove stale clone",
					"agent", agentName, "dir", dirName, "error", err)
				continue
			}

			logger.Info("workspace cleanup: removed stale clone",
				"agent", agentName, "dir", dirName, "age", age.Round(time.Minute))
			removed++
		}
	}

	logger.Info("workspace cleanup complete", "removed", removed)
}

func isSkippedDir(name string) bool {
	switch name {
	case ".cache", ".config", ".npm-cache", "bin":
		return true
	}
	return false
}
