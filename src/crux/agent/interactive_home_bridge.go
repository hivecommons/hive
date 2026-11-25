package agent

import (
	"errors"
	"io/fs"
	"os"
)

// sharedBridgeTargetMode is the mode of a shared XDG bridge target: group-
// writable and setgid so every agent UID (all in the shared group) can create
// its CLI's state under it, the same shape as the goose sibling the permissions
// watcher pre-creates.
const sharedBridgeTargetMode = fs.FileMode(0o775) | fs.ModeSetgid

// ensureSharedBridgeTarget creates the shared directory a bridge points at.
// Without it the per-agent link dangles, and a CLI that creates its own
// subdirectories non-recursively (opencode's mkdir of .../opencode/log) dies at
// startup with ENOENT even though the link itself looks fine. Best-effort like
// the rest of provisioning: a permission error means this process does not own
// the shared tree, which is the expected steady state for a non-owner.
func (m *Manager) ensureSharedBridgeTarget(agentName, shared string) {
	if err := mkdirAllNoFollow(sharedAgentHome, shared, sharedBridgeTargetMode); err != nil {
		if errors.Is(err, fs.ErrPermission) {
			m.logger.Debug("shared XDG bridge target creation skipped (not owner)",
				"agent", agentName, "dir", shared, "error", err)
			return
		}
		m.logger.Warn("failed to create shared XDG bridge target",
			"agent", agentName, "dir", shared, "error", err)
		return
	}
	// mkdir is subject to the umask; make the group bit stick.
	if err := os.Chmod(shared, sharedBridgeTargetMode); err != nil && !errors.Is(err, fs.ErrPermission) {
		m.logger.Warn("failed to set mode on shared XDG bridge target",
			"agent", agentName, "dir", shared, "error", err)
	}
}
