package agent

import (
	"log/slog"
	"os"
	"path/filepath"
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

	// DirPerms is the minimum permission bits required on directories (u+rwx, g+rwx).
	// Group access is essential because agents run as different users sharing the node group.
	DirPerms = 0o770

	// FilePerms is the minimum permission bits required on files (u+rw, g+rw).
	FilePerms = 0o660
)

// WatchedHomeDirs are the subdirectories under the shared home and data
// volume that tools (Copilot, Claude, etc.) or init containers frequently
// create with root ownership, locking out agent UIDs.
var WatchedHomeDirs = []string{
	"/data/home/.copilot",
	"/data/home/.claude",
	"/data/home/.cache",
	"/data/home/.config",
	"/data/home/.local",
	"/data/agents",
}

// GooseLogsDir is the rolling log directory goose 1.37 creates on startup.
// Goose panics if this directory doesn't exist, so the watcher ensures it
// is created at startup with correct permissions.
//
// A var (not const) so tests can point the permissions watcher at a writable
// temp tree (together with WatchedHomeDirs) to exercise ensureWatchedDirs /
// fixPermissions / fixEntry. Production value is unchanged.
var GooseLogsDir = "/data/home/.local/state/goose/logs/cli"

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
	allDirs := append([]string{GooseLogsDir}, WatchedHomeDirs...)
	for _, dir := range allDirs {
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
	uid, gid, ok := fileOwnership(fi)
	if !ok {
		return
	}

	// Fix group ownership if not in the node group — all agents share this group.
	// Also fix root-owned files (uid 0) to the dev user.
	needsChown := false
	newUID := int(uid)
	if uid == 0 {
		newUID = DevUID
		needsChown = true
	}
	if gid != uint32(NodeGID) {
		needsChown = true
	}
	if needsChown {
		if err := os.Chown(path, newUID, NodeGID); err != nil {
			logger.Warn("permissions watcher: chown failed",
				"path", path,
				"error", err,
			)
		} else {
			logger.Warn("permissions watcher: fixed ownership",
				"path", path,
				"was_uid", uid,
				"was_gid", gid,
				"new_uid", newUID,
				"new_gid", NodeGID,
			)
		}
	}

	// Only fix permissions on files we own or just chowned.
	// Skipping files owned by other users avoids "operation not permitted"
	// spam when agents create files as their own users.
	if newUID != DevUID && uid != uint32(DevUID) {
		return
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
