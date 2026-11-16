package sessionprune

// This file extends the janitor to the per-session scratch the Claude CLI
// keeps outside /data: <tmp>/claude-<uid>/<project>/<session>/, which holds
// task output, scratchpads and whatever the agent cloned or built there.
// Nothing removes it when a session ends, and it lives in the container's
// writable layer, not on the data volume, so it fills the node's root disk
// instead. On one Docker host two spokes reached 17 GiB and 22 GiB in a
// fortnight and filled the disk, which took the host's other services down.

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultTmpRetentionHours bounds how long a Claude CLI session scratch
// directory is kept after its last write. A day is longer than any agent
// session runs and short enough that a busy spoke cannot fill a disk between
// passes; a resumed session only loses its scratch, never its transcript,
// which lives under the agent home.
const DefaultTmpRetentionHours = 24

// claudeTmpPrefix is the per-user directory the Claude CLI creates in the
// temp directory: claude-<uid>.
const claudeTmpPrefix = "claude-"

// PruneClaudeTmp removes Claude CLI session scratch directories,
// <tmpRoot>/claude-*/<project>/<session>, whose newest write anywhere inside
// is older than maxAge. Only that exact depth is considered, and a project or
// user directory is left in place even when it is empty. Symlinks are never
// followed, a missing root is not an error, and maxAge <= 0 disables the sweep.
func PruneClaudeTmp(tmpRoot string, maxAge time.Duration, now time.Time, logger *slog.Logger) (Result, error) {
	var res Result
	if tmpRoot == "" || maxAge <= 0 {
		return res, nil
	}
	users, err := os.ReadDir(tmpRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return res, nil
		}
		return res, err
	}
	cutoff := now.Add(-maxAge)
	for _, user := range users {
		if !user.IsDir() || !strings.HasPrefix(user.Name(), claudeTmpPrefix) {
			continue
		}
		projects, err := os.ReadDir(filepath.Join(tmpRoot, user.Name()))
		if err != nil {
			logger.Debug("claude tmp prune: cannot read user dir, skipping", "dir", user.Name(), "err", err)
			continue
		}
		for _, project := range projects {
			if !project.IsDir() {
				continue
			}
			projectDir := filepath.Join(tmpRoot, user.Name(), project.Name())
			sessions, err := os.ReadDir(projectDir)
			if err != nil {
				logger.Debug("claude tmp prune: cannot read project dir, skipping", "dir", projectDir, "err", err)
				continue
			}
			for _, session := range sessions {
				if !session.IsDir() {
					continue
				}
				pruneHomeTarget(filepath.Join(projectDir, session.Name()), cutoff, &res, logger)
			}
		}
	}
	return res, nil
}
