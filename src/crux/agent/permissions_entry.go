package agent

import (
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
)

// pinEntry opens the entry Walk reported as path with openEntry and confirms
// the descriptor names the very inode Walk saw (same device and inode number,
// and still a directory or regular file), so the check-then-change in
// fixEntry happens on one inode and cannot be redirected by swapping the
// pathname in between — not even for another entry of the same kind, whose
// ownership differs from the one fi describes (CWE-59 / CWE-367). The
// returned chmod acts through the descriptor. ok is false, with no descriptor
// to close, when the entry cannot be opened (ELOOP, EACCES, ENOENT) or no
// longer matches fi.
func pinEntry(path string, fi os.FileInfo) (f *os.File, mode os.FileMode, chmod func(os.FileMode) error, ok bool) {
	walked, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, 0, nil, false
	}
	f, err := openEntry(path)
	if err != nil {
		return nil, 0, nil, false
	}
	ofi, err := f.Stat()
	if err != nil || ofi.Mode()&os.ModeSymlink != 0 || ofi.IsDir() != fi.IsDir() || (!ofi.IsDir() && !ofi.Mode().IsRegular()) {
		_ = f.Close()
		return nil, 0, nil, false
	}
	opened, ok := ofi.Sys().(*syscall.Stat_t)
	if !ok || opened.Dev != walked.Dev || opened.Ino != walked.Ino {
		_ = f.Close()
		return nil, 0, nil, false
	}
	return f, ofi.Mode(), func(m os.FileMode) error { return entryChmod(f, m) }, true
}

// fixModeFiles re-widens the per-agent mode files (ModeFileGlob) to be
// world-readable. This is the running-pod remediation for #3882/#3679: the
// Manager only rewrites a mode file on a level change or kick, so a pod whose
// files are 0600 (written by the #3172 build, or umask-narrowed) stays broken
// until then; this scan repairs them within one tick.
//
// SECURITY: /tmp is sticky and world-writable, so an agent can pre-create a
// name matching the glob. Only regular files owned by the hive uid are
// touched, the only change ever made is adding read bits, and the whole
// check-then-chmod happens on one open descriptor: O_NOFOLLOW refuses a
// planted symlink at open time, and f.Stat/f.Chmod act on that same inode, so
// the pathname cannot be swapped between the check and the chmod (CWE-59 /
// CWE-367 — the same descriptor discipline writeAgentStateFile follows, #3175).
func fixModeFiles(logger *slog.Logger) {
	for _, glob := range []string{ModeFileGlob, CapsFileGlob} {
		paths, err := filepath.Glob(glob)
		if err != nil {
			continue
		}
		for _, path := range paths {
			fixModeFile(path, logger)
		}
	}
}

// fixModeFile applies the fixModeFiles policy to a single path.
func fixModeFile(path string, logger *slog.Logger) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		// ELOOP for a planted symlink, EACCES for a file we do not own: skip.
		return
	}
	defer func() { _ = f.Close() }() // read-only fd; nothing to lose on close error
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return
	}
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return
	}
	perm := fi.Mode().Perm()
	if perm&modeFileReadBits == modeFileReadBits {
		permWarnDedupe.clear(dedupeKey("chmod mode file", path))
		return
	}
	newPerm := perm | modeFileReadBits
	if err := f.Chmod(newPerm); err != nil {
		if warn, _ := permWarnDedupe.shouldWarn(dedupeKey("chmod mode file", path), err.Error(), dedupeClass("chmod mode file", err)); warn {
			logger.Warn("permissions watcher: chmod mode file failed; agents cannot read their GitHub mode and gh/pushes will be blocked",
				"path", path,
				"old_mode", perm.String(),
				"want_mode", newPerm.String(),
				"error", err,
				"note", "identical repeats logged at debug until this changes",
			)
		} else {
			logger.Debug("permissions watcher: chmod mode file failed",
				"path", path,
				"error", err,
			)
		}
		return
	}
	permWarnDedupe.clear(dedupeKey("chmod mode file", path))
	logger.Info("permissions watcher: fixed mode file permissions",
		"path", path,
		"old_mode", perm.String(),
		"new_mode", newPerm.String(),
	)
}
