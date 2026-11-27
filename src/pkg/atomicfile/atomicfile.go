// Package atomicfile replaces a file's contents so that a concurrent reader,
// or the next boot after a crash, sees either the old bytes or the new bytes
// and never a truncated or partial file.
//
// It is a stdlib-only leaf package so any package in the module can use it.
// Before it existed each store hand-rolled the idiom and the copies drifted:
// some staged through a fixed path+".tmp" that two writers could rename out
// from under each other (#16629), and some skipped the fsync whose absence
// left a 0-byte runtime config after a SIGKILL (#17862).
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// tempFile is the subset of *os.File that WriteFile uses. It exists so tests
// can inject chmod, write, sync and close failures that a real file on a
// healthy filesystem never produces.
type tempFile interface {
	Name() string
	Chmod(os.FileMode) error
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

// createTemp is os.CreateTemp behind a seam for tests.
var createTemp = func(dir, pattern string) (tempFile, error) {
	return os.CreateTemp(dir, pattern)
}

// WriteFile writes data to path through a uniquely named temp file in the
// same directory: mode is set on the temp file, its data is fsynced, it is
// renamed over path, and the directory is fsynced (best effort) so the
// rename itself is durable.
//
// mode is applied exactly (not filtered by the umask), including when path
// already exists with other permission bits. The parent directory must
// already exist; callers that create it keep their own MkdirAll so they keep
// choosing its mode.
//
// On any failure the temp file is removed and path is left untouched. A
// failed rename is returned as the *os.LinkError from os.Rename (wrapped), so
// a caller that needs a fallback for targets that cannot be renamed over can
// detect it with errors.As.
func WriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := createTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	fail := func(step string, err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("%s temp file %s: %w", step, tmpPath, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return fail("chmod", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail("writing", err)
	}
	if err := tmp.Sync(); err != nil {
		return fail("syncing", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("closing temp file %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("renaming onto %s: %w", path, err)
	}
	SyncDir(dir)
	return nil
}

// SyncDir fsyncs a directory so a rename that just happened in it is durable.
// Best effort: some filesystems (and all of Windows) refuse to open a
// directory for sync, and a failure here cannot undo the rename.
func SyncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
