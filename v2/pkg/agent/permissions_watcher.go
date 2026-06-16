package agent

import (
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Permission watcher constants — no magic numbers.
const (
	// PermissionFixInterval is how often the watcher scans for wrong ownership.
	PermissionFixInterval = 10 * time.Second

	// DevUID is the uid of the "dev" user that agents run as.
	DevUID = 1001

	// NodeGID is the gid of the "node" group shared by all agent users.
	NodeGID = 1000

	// DirPerms is the minimum permission bits required on directories (u+rwx).
	DirPerms = 0o700

	// FilePerms is the minimum permission bits required on files (u+rw).
	FilePerms = 0o600
)

// WatchedHomeDirs are the subdirectories under the shared home that tools
// (Copilot, Claude, etc.) frequently create with root ownership.
var WatchedHomeDirs = []string{
	"/data/home/.copilot",
	"/data/home/.cache",
	"/data/home/.config",
	"/data/home/.local",
}

// StartPermissionsWatcher runs a background goroutine that periodically
// scans WatchedHomeDirs and fixes files/directories that were created
// with wrong ownership (e.g., root-owned by Copilot CLI).
//
// It never blocks or panics. Call it once at startup:
//
//	go agent.StartPermissionsWatcher(logger)
func StartPermissionsWatcher(logger *slog.Logger) {
	logger.Info("permissions watcher started",
		"interval", PermissionFixInterval,
		"watched_dirs", WatchedHomeDirs,
		"target_uid", DevUID,
		"target_gid", NodeGID,
	)

	// Ensure watched directories exist with correct ownership on first run.
	ensureWatchedDirs(logger)

	ticker := time.NewTicker(PermissionFixInterval)
	defer ticker.Stop()

	for range ticker.C {
		fixPermissions(logger)
	}
}

// ensureWatchedDirs creates each watched directory if it does not exist
// and sets correct ownership.
func ensureWatchedDirs(logger *slog.Logger) {
	for _, dir := range WatchedHomeDirs {
		if err := os.MkdirAll(dir, DirPerms|0o070); err != nil {
			logger.Warn("permissions watcher: failed to create directory",
				"path", dir,
				"error", err,
			)
			continue
		}
		// Always set ownership on the top-level dir at startup.
		if err := os.Chown(dir, DevUID, NodeGID); err != nil {
			logger.Warn("permissions watcher: failed to chown directory",
				"path", dir,
				"error", err,
			)
		}
	}
}

// fixPermissions walks each watched directory and fixes ownership/mode
// on any file or directory that is wrong. It only logs when it actually
// changes something.
func fixPermissions(logger *slog.Logger) {
	for _, root := range WatchedHomeDirs {
		info, err := os.Stat(root)
		if err != nil {
			// Directory doesn't exist yet — create it.
			if os.IsNotExist(err) {
				ensureWatchedDirs(logger)
			}
			continue
		}
		if !info.IsDir() {
			continue
		}

		err = filepath.Walk(root, func(path string, fi os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return nil // skip unreadable entries, don't abort walk
			}
			fixEntry(path, fi, logger)
			return nil
		})
		if err != nil {
			logger.Warn("permissions watcher: walk error",
				"root", root,
				"error", err,
			)
		}
	}
}

// fixEntry checks a single file or directory and corrects ownership/mode
// if needed.
func fixEntry(path string, fi os.FileInfo, logger *slog.Logger) {
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}

	// Fix ownership if owned by root (uid 0).
	if stat.Uid == 0 {
		if err := os.Chown(path, DevUID, NodeGID); err != nil {
			logger.Warn("permissions watcher: chown failed",
				"path", path,
				"error", err,
			)
		} else {
			logger.Warn("permissions watcher: fixed root-owned entry",
				"path", path,
				"was_uid", stat.Uid,
				"new_uid", DevUID,
			)
		}
	}

	mode := fi.Mode()
	if fi.IsDir() {
		// Directories need u+rwx to be usable.
		if mode.Perm()&DirPerms != DirPerms {
			newMode := mode.Perm() | DirPerms
			if err := os.Chmod(path, newMode); err != nil {
				logger.Warn("permissions watcher: chmod dir failed",
					"path", path,
					"error", err,
				)
			} else {
				logger.Warn("permissions watcher: fixed directory permissions",
					"path", path,
					"old_mode", mode.Perm().String(),
					"new_mode", newMode.String(),
				)
			}
		}
	} else {
		// Regular files need u+rw to be readable/writable.
		if mode.Perm()&FilePerms != FilePerms {
			newMode := mode.Perm() | FilePerms
			if err := os.Chmod(path, newMode); err != nil {
				logger.Warn("permissions watcher: chmod file failed",
					"path", path,
					"error", err,
				)
			} else {
				logger.Warn("permissions watcher: fixed file permissions",
					"path", path,
					"old_mode", mode.Perm().String(),
					"new_mode", newMode.String(),
				)
			}
		}
	}
}
