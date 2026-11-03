package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	DiskFullTrigger = "disk-full"
	// DataDiskPressureKey scopes the data_disk_pressure self-report
	// condition to the shared data volume rather than to each paused agent.
	DataDiskPressureKey = "data"

	copilotCachePruneAge      = 30 * time.Minute
	copilotDataFullThreshold  = 95
	copilotDiskFullReasonHead = "disk-full"
)

var statfsForPath = syscall.Statfs

func copilotCacheRoot() string {
	return filepath.Join(sharedAgentHome, ".cache", "copilot", "pkg")
}

func pruneCopilotCache(root string, cutoff time.Time) (int, error) {
	var removed int
	var errs []string
	pruneDirEntries := func(dir string, match func(string) bool) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !os.IsNotExist(err) {
				errs = append(errs, err.Error())
			}
			return
		}
		for _, entry := range entries {
			if !entry.IsDir() || !match(entry.Name()) {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			info, err := entry.Info()
			if err != nil {
				errs = append(errs, err.Error())
				continue
			}
			if !info.ModTime().Before(cutoff) {
				continue
			}
			if err := os.RemoveAll(path); err != nil {
				errs = append(errs, err.Error())
				continue
			}
			removed++
		}
	}
	pruneDirEntries(filepath.Join(root, "tmp"), func(string) bool { return true })
	pruneDirEntries(filepath.Join(root, "linux-x64"), func(name string) bool {
		return strings.HasPrefix(name, ".extracting-")
	})
	if len(errs) > 0 {
		return removed, errors.New(strings.Join(errs, "; "))
	}
	return removed, nil
}

func (m *Manager) runCopilotCacheJanitor() {
	removed, err := pruneCopilotCache(copilotCacheRoot(), time.Now().Add(-copilotCachePruneAge))
	if err != nil && m.logger != nil {
		m.logger.Warn("copilot cache janitor failed", "error", err)
	}
	if removed > 0 && m.logger != nil {
		m.logger.Info("copilot cache janitor pruned stale extraction directories", "removed", removed)
	}
}

func copilotDataRoot() string {
	return filepath.Dir(sharedAgentHome)
}

func copilotDiskFullCondition() (string, bool) {
	var st syscall.Statfs_t
	if err := statfsForPath(copilotDataRoot(), &st); err != nil {
		return "", false
	}
	total := st.Blocks
	if total == 0 {
		return "", false
	}
	used := total - st.Bavail
	pct := int((used * 100) / total)
	if pct < copilotDataFullThreshold {
		return "", false
	}
	return fmt.Sprintf("%s: %s is %d%% full (threshold %d%%); pausing Copilot relaunch until space is reclaimed",
		copilotDiskFullReasonHead, copilotDataRoot(), pct, copilotDataFullThreshold), true
}

func isCopilotDiskFullError(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "enospc") ||
		strings.Contains(lower, "edquot") ||
		strings.Contains(lower, "no space left on device") ||
		strings.Contains(lower, "disk quota exceeded") ||
		strings.Contains(lower, "tar_entry_error") ||
		strings.Contains(lower, "unknown system error -122")
}
