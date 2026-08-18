//go:build !windows

package hostedcontrol

import (
	"io/fs"
	"syscall"
)

func sourceFileMetadata(_ string, info fs.FileInfo) (device uint64, hasDevice bool, links uint64, hasLinks bool, err error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false, 0, false, nil
	}
	return uint64(stat.Dev), true, uint64(stat.Nlink), true, nil
}

func sourcePermissionsSafe(info fs.FileInfo) bool { return info.Mode().Perm()&0o077 == 0 }
func pathCaseInsensitive() bool                   { return false }
