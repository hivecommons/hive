// Package sessionprune removes aged CLI session-state directories.
//
// Every Copilot CLI invocation creates a new session directory, and every agent
// on a spoke shares one session-state directory (the entrypoint symlinks
// ~/.copilot to /data/home/.copilot for all of them). Nothing ever deleted
// them, so a long-lived spoke accumulated ~9,400 entries spanning three months
// on a shared NFS PVC. That is an unbounded leak, and because the directory is
// read on session start, it gets slower as it grows.
package sessionprune

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// DefaultRetentionDays is how long a session directory is kept after its last
// write. Sessions feed token accounting and post-hoc debugging, both of which
// look at recent activity, so a week is ample.
//
// Note for future edits: tokens.maxSessionAgeDays (30) bounds how far back the
// token scanners look. Retention shorter than that window necessarily truncates
// it, because the scanner cannot count sessions that no longer exist. Lowering
// this value trades dashboard token history for disk.
const DefaultRetentionDays = 7

// Result reports what a Prune pass did. Removed counts session directories
// deleted; Failed counts those that could not be deleted. A failure is logged
// and skipped rather than returned, so one unreadable directory cannot wedge
// the janitor and leave the PVC growing.
type Result struct {
	Scanned int
	Removed int
	Failed  int
}

// Prune deletes immediate subdirectories of dir whose most recent write is
// older than maxAge.
//
// A missing dir is not an error: a spoke that has never run a given CLI has no
// such directory, and the janitor must stay quiet on those.
//
// maxAge <= 0 disables pruning and is reported as a no-op, so operators can
// turn the janitor off from config without a code change.
func Prune(dir string, maxAge time.Duration, now time.Time, logger *slog.Logger) (Result, error) {
	var res Result

	if dir == "" || maxAge <= 0 {
		return res, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return res, nil
		}
		return res, err
	}

	cutoff := now.Add(-maxAge)

	for _, entry := range entries {
		// Only ever touch directories named like a session UUID. The
		// session-state directory is shared, so a stray file or an unrelated
		// sibling directory must survive a prune pass untouched.
		if !entry.IsDir() || !IsSessionDirName(entry.Name()) {
			continue
		}

		res.Scanned++
		path := filepath.Join(dir, entry.Name())

		newest, err := newestModTime(path)
		if err != nil {
			// Racing with a session being written, or a transient NFS error.
			// Keeping the directory is always the safe choice.
			logger.Debug("session prune: stat failed, keeping", "path", path, "err", err)
			continue
		}
		if !newest.Before(cutoff) {
			continue
		}

		if err := os.RemoveAll(path); err != nil {
			res.Failed++
			logger.Warn("session prune: remove failed", "path", path, "err", err)
			continue
		}
		res.Removed++
	}

	return res, nil
}

// newestModTime returns the most recent modification time anywhere inside a
// session directory, including the directory itself.
//
// The directory's own mtime is not sufficient. A directory's mtime changes only
// when an entry is added or removed, not when an existing file is written, so a
// session that has been appending to events.jsonl for days can still carry a
// stale directory mtime. Deciding on that alone would delete live sessions.
func newestModTime(dir string) (time.Time, error) {
	var newest time.Time

	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if mt := info.ModTime(); mt.After(newest) {
			newest = mt
		}
		return nil
	})
	if err != nil {
		return time.Time{}, err
	}

	return newest, nil
}

// IsSessionDirName reports whether name is a canonical 8-4-4-4-12 hex UUID, the
// form both the Copilot CLI and the Claude CLI use for session directories.
func IsSessionDirName(name string) bool {
	const uuidLen = 36
	if len(name) != uuidLen {
		return false
	}
	for i := 0; i < uuidLen; i++ {
		c := name[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}
