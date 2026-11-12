// Package sharedfile replaces and reads files that live in directories other
// uids can write to: the shared CLI credential and config files under
// /data/home that the hive maintains for a fleet of per-UID agents.
//
// A plain "write <path>.tmp, chmod, rename" in such a directory acts on
// whatever is at those names when each call runs. Here every step acts on the
// inode this process created, by descriptor, and the name of that inode is
// unpredictable; the directory and the destination are checked to be what the
// caller expects (a directory this process owns, a regular file or nothing)
// right before the rename, and a symlink at any of those places is a refusal,
// never something to write through.
package sharedfile

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// ErrUnsafeLocation is wrapped by Replace when the destination directory or
// the destination itself is not in a state this package will write through:
// the directory is missing, a symlink, world-writable, or not owned by this
// process, or the destination exists and is not a regular file. Nothing is
// written when it is returned. Callers that poll should log it once, not per
// cycle.
var ErrUnsafeLocation = errors.New("refusing to replace a file at an unsafe location")

// ErrTooLarge is returned by ReadNoFollow for a file over the caller's cap.
var ErrTooLarge = errors.New("file is larger than the read cap")

// ErrNotRegular is returned by ReadNoFollow when the path is a symlink or any
// other non-regular file.
var ErrNotRegular = errors.New("not a regular file")

// createAttempts bounds the search for a free temporary name. A collision on
// a 64-bit random suffix is not expected; the retries make an EEXIST from a
// deliberately pre-created name a failure rather than a write-through.
const createAttempts = 8

// chownFile applies an owner to the inode behind an open descriptor. A seam:
// tests run as one uid and cannot change an owner for real.
var chownFile = func(f *os.File, uid, gid int) error { return f.Chown(uid, gid) }

// randRead fills the temporary-name suffix; a seam so tests can force a
// collision or a failure.
var randRead = rand.Read

// syncFile flushes the written temporary; a seam so tests can fail it.
var syncFile = func(f *os.File) error { return f.Sync() }

// Options tune Replace.
type Options struct {
	// PreserveOwner re-applies the previous destination's owner to the new
	// file, best-effort: an unprivileged process cannot chown, and the file it
	// wrote is already readable to the group by mode.
	PreserveOwner bool
}

// Replace atomically replaces target with data at mode. The data is written
// to a new file with a random name beside target (O_EXCL|O_NOFOLLOW, 0600),
// chmodded, optionally chowned, fsynced and verified by descriptor, and then
// renamed over target; the directory is fsynced afterwards. On any failure the
// temporary file is removed and target is untouched.
//
// Before the rename, the directory must be a real directory (not a symlink)
// owned by this process and not world-writable, and target must be absent or
// a regular file (not a symlink); otherwise ErrUnsafeLocation.
func Replace(target string, data []byte, mode fs.FileMode, opts Options) error {
	dir := filepath.Dir(target)
	prev, err := checkLocation(dir, target)
	if err != nil {
		return err
	}
	f, tmp, err := createTemp(target)
	if err != nil {
		return err
	}
	err = fill(f, data, mode, prev, opts)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// The location can have changed while the file was being written.
	if _, err := checkLocation(dir, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	syncDir(dir)
	return nil
}

// CheckLocation verifies target's directory and target as documented on
// Replace, and returns the current target's Lstat (nil when absent). Callers
// that do preparatory work beside target (a backup copy, say) use it to refuse
// before that work, not only at the final write.
func CheckLocation(target string) (os.FileInfo, error) {
	return checkLocation(filepath.Dir(target), target)
}

// checkLocation verifies dir and target as documented on Replace and returns
// the current target's stat (nil when absent).
func checkLocation(dir, target string) (os.FileInfo, error) {
	dinfo, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrUnsafeLocation, dir, err)
	}
	if !dinfo.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory (mode %s)", ErrUnsafeLocation, dir, dinfo.Mode())
	}
	if dinfo.Mode().Perm()&0o002 != 0 {
		return nil, fmt.Errorf("%w: %s is world-writable (mode %s)", ErrUnsafeLocation, dir, dinfo.Mode())
	}
	if st, ok := dinfo.Sys().(*syscall.Stat_t); ok {
		if euid := os.Geteuid(); euid != 0 && int(st.Uid) != euid {
			return nil, fmt.Errorf("%w: %s is owned by uid %d, this process is uid %d", ErrUnsafeLocation, dir, st.Uid, euid)
		}
	}
	tinfo, err := os.Lstat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrUnsafeLocation, target, err)
	}
	if !tinfo.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file (mode %s)", ErrUnsafeLocation, target, tinfo.Mode())
	}
	return tinfo, nil
}

// createTemp creates a new 0600 file with a random name beside target. The
// name is "<target>.<16 hex>.tmp", so sweeps that match "<base>.*.tmp" still
// recognise a leftover.
func createTemp(target string) (*os.File, string, error) {
	for i := 0; i < createAttempts; i++ {
		var b [8]byte
		if _, err := randRead(b[:]); err != nil {
			return nil, "", err
		}
		tmp := target + "." + hex.EncodeToString(b[:]) + ".tmp"
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		return f, tmp, nil
	}
	return nil, "", fmt.Errorf("no free temporary name beside %s after %d attempts", target, createAttempts)
}

// fill writes data to f and sets its metadata, all through the descriptor,
// then verifies by fstat that the inode is a regular file with exactly the
// mode (and, when a chown succeeded, the owner) asked for.
func fill(f *os.File, data []byte, mode fs.FileMode, prev os.FileInfo, opts Options) error {
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil {
		return err
	}
	wantUID, wantGID := -1, -1
	if opts.PreserveOwner && prev != nil {
		if st, ok := prev.Sys().(*syscall.Stat_t); ok {
			if err := chownFile(f, int(st.Uid), int(st.Gid)); err == nil {
				wantUID, wantGID = int(st.Uid), int(st.Gid)
			}
		}
	}
	if err := syncFile(f); err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != mode.Perm() {
		return fmt.Errorf("%s: written file is mode %s, want regular %s", f.Name(), info.Mode(), mode.Perm())
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		if st.Nlink != 1 {
			return fmt.Errorf("%s: written file has %d links, want 1", f.Name(), st.Nlink)
		}
		if wantUID >= 0 && (int(st.Uid) != wantUID || int(st.Gid) != wantGID) {
			return fmt.Errorf("%s: written file is owned by %d:%d, want %d:%d", f.Name(), st.Uid, st.Gid, wantUID, wantGID)
		}
	}
	return nil
}

// syncDir makes the rename durable; best-effort, like every writer before it.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// openRegularNoFollow opens path without following a symlink and confirms the
// opened file is regular. ErrNotRegular for a symlink or any other kind.
func openRegularNoFollow(path string, flag int) (*os.File, os.FileInfo, error) {
	f, err := os.OpenFile(path, flag|syscall.O_NOFOLLOW, 0)
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) && (errors.Is(pe.Err, syscall.ELOOP) || errors.Is(pe.Err, syscall.EMLINK)) {
			return nil, nil, fmt.Errorf("%w: %s is a symlink", ErrNotRegular, path)
		}
		return nil, nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("%w: %s (mode %s)", ErrNotRegular, path, info.Mode())
	}
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

// Chmod sets mode on the regular file at path through an open descriptor, so
// a symlink planted at path is refused (ErrNotRegular) instead of followed.
// Returns the mode the file had before, and changed=false when it already
// matched (nothing is done in that case).
func Chmod(path string, mode fs.FileMode) (prev fs.FileMode, changed bool, err error) {
	f, info, err := openRegularNoFollow(path, os.O_RDONLY)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	prev = info.Mode().Perm()
	if prev == mode.Perm() {
		return prev, false, nil
	}
	if err := f.Chmod(mode.Perm()); err != nil {
		return prev, false, err
	}
	return prev, true, nil
}

// ReadNoFollow reads a regular file of at most max bytes without following a
// symlink at path. ErrNotRegular for a symlink or other non-regular file,
// ErrTooLarge over the cap; the open error (fs.ErrNotExist, fs.ErrPermission)
// otherwise.
func ReadNoFollow(path string, max int64) ([]byte, error) {
	f, info, err := openRegularNoFollow(path, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if info.Size() > max {
		return nil, fmt.Errorf("%w: %s is %d bytes, cap %d", ErrTooLarge, path, info.Size(), max)
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%w: %s grew past %d bytes while being read", ErrTooLarge, path, max)
	}
	return data, nil
}
