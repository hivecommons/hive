package agent

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	plukRunDir = "/var/run/pluk"
)

// pluk runs from per-agent tmux servers. Agent UIDs differ, but they all share
// the node group, so setgid keeps new entries group-owned without opening the
// directory to unrelated container users.
var plukSharedDirMode = os.FileMode(0o770) | os.ModeSetgid

func ensurePlukRunDirs(runDir string) error {
	for _, name := range []string{"logs", "commands"} {
		dir := filepath.Join(runDir, name)
		if err := os.MkdirAll(dir, plukSharedDirMode); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
		if os.Geteuid() == 0 {
			if err := os.Chown(dir, DevUID, NodeGID); err != nil {
				return fmt.Errorf("chowning %s to %d:%d: %w", dir, DevUID, NodeGID, err)
			}
		}
		if err := os.Chmod(dir, plukSharedDirMode); err != nil {
			return fmt.Errorf("chmoding %s to %s: %w", dir, plukSharedDirMode, err)
		}
	}
	return nil
}
