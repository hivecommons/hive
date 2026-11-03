package github

import (
	"os"
	"path/filepath"
	"syscall"
)

// reviewEvidenceLockName is the advisory cross-process lock file in a PR's
// evidence directory (#13911). It is a separate file because bundles are
// replaced by rename, which would orphan a lock held on the bundle itself.
const reviewEvidenceLockName = ".lock"

// lockReviewEvidenceDir takes a blocking exclusive flock on the lock file in
// the directory holding the bundle at path, so hub and agent processes do not
// each rewrite the bundle from a stale read and drop the other's verdict.
// reviewEvidenceMu only serializes goroutines in one process; callers take it
// first and call the returned unlock after the atomic rename.
//
// Best-effort: if the directory or lock file cannot be opened (for example a
// directory owned by another uid), the write proceeds unserialized across
// processes rather than refusing to record evidence.
func lockReviewEvidenceDir(path string) func() {
	dir := filepath.Dir(path)
	if err := mkdirEvidenceDirs(dir); err != nil {
		return func() {}
	}
	lockPath := filepath.Join(dir, reviewEvidenceLockName)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o664)
	if err != nil {
		if f, err = os.Open(lockPath); err != nil {
			return func() {}
		}
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close() // degrades to in-process serialization only
		return func() {}
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
}
