package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// writeFileAtomic replaces the contents of path without ever exposing a
// truncated or partial file to a concurrent reader or to the next boot.
//
// Motivation (#17862): Save used O_WRONLY|O_TRUNC in place on the PVC runtime
// config. A SIGKILL (liveness probe) landing between the truncate and the
// write left a 0-byte /data/hive.yaml.runtime; the entrypoint's `-s` check
// then fell back to a months-stale /data/hive.yaml.bak and a hive booted
// with a three-month-old ACMM level and roster.
//
// Strategy: write data to a temp file in the TARGET's directory, fsync it,
// then rename it onto the target. Two constraints shape the details:
//
//   - path may be a symlink (hosted spokes link /etc/hive/hive.yaml to the
//     PVC runtime config, #17468). The rename must land on the link's
//     TARGET, not replace the link with a regular file, so the final path is
//     resolved with EvalSymlinks first. Pinned by TestSaveThroughSymlinkKeepsLink.
//   - path may be a single-file Docker bind mount, where rename over the
//     mount point fails (EBUSY) and would also swap the inode the host sees.
//     When the rename fails, fall back to the previous in-place write so
//     those deployments keep working exactly as before; they never had
//     atomicity and lose nothing here.
//
// mode is applied to the temp file before the rename, so a pre-existing
// world-readable inode does not carry its bits through (WriteFile's mode
// only applies on create). The fallback path chmods explicitly for the same
// reason.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("resolving %s: %w", path, err)
	}

	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(target)+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temp file %s: %w", tmpPath, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("writing temp file %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("syncing temp file %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("closing temp file %s: %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, target); err != nil {
		cleanup()
		// Bind-mounted single file (or any filesystem that refuses the
		// rename): keep the inode and write in place, as Save always did.
		if inPlaceErr := writeFileInPlace(target, data, mode); inPlaceErr != nil {
			return fmt.Errorf("renaming %s onto %s: %w (in-place fallback also failed: %v)", tmpPath, target, err, inPlaceErr)
		}
		return nil
	}
	syncDir(dir)
	return nil
}

// writeFileInPlace is the pre-#17862 O_TRUNC write, kept only as the fallback
// for targets that cannot be renamed over. It is NOT crash-safe.
func writeFileInPlace(target string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(target, mode)
}

// syncDir fsyncs a directory so the rename that just happened in it is
// durable. Best-effort: some filesystems (and all of Windows) refuse to open
// a directory for sync, and a failure here cannot un-do the rename.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
