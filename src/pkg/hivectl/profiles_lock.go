package hivectl

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// profilesLockFileName is the advisory cross-process lock serialising every
// ProfileStore commit. It is a separate file, not profiles.yml itself, because
// Save replaces profiles.yml by rename — a lock on the data file would stay
// attached to the orphaned inode and stop excluding anyone.
const profilesLockFileName = profilesFileName + ".lock"

// LockPath reports where the store's cross-process lock file lives.
func (s *ProfileStore) LockPath() string { return filepath.Join(s.dir, profilesLockFileName) }

// Revision identifies the profiles.yml content this set was loaded from or
// last committed to. It is "" for a set that never touched the file, which
// CommitExpecting reads as "profiles.yml must not exist yet".
//
// Surfaces that show a set and apply an edit later (the TUI, the Web UI) carry
// it with the staged change and pass it back to CommitExpecting, so an edit
// reviewed against an older file is refused rather than silently merged.
func (set *ProfileSet) Revision() string {
	if set == nil {
		return ""
	}
	return set.revision
}

func profilesRevision(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// lock takes a blocking exclusive flock on profiles.yml.lock and returns its
// release. flock is per open file description, so it excludes other processes
// and other ProfileStores in this one alike, and the kernel drops it when the
// holder dies however it dies: a lock file left on disk is simply unlocked.
func (s *ProfileStore) lock() (func(), error) {
	if err := os.MkdirAll(s.dir, profilesDirMode); err != nil {
		return nil, fmt.Errorf("create hive config dir %s: %w", s.dir, err)
	}
	path := s.LockPath()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, profilesFileMode)
	if err != nil {
		return nil, fmt.Errorf("open hive profiles lock %s: %w", path, err)
	}
	// OpenFile's mode only applies on create; pin it for a pre-existing file.
	if err := f.Chmod(profilesFileMode); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("restrict permissions on %s: %w", path, err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock hive profiles %s: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) // Close releases it too
		_ = f.Close()
	}, nil
}
