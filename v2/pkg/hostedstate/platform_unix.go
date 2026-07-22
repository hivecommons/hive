//go:build !windows

package hostedstate

import (
	"io/fs"
	"syscall"
)

func platformFileMetadata(_ string, info fs.FileInfo) (device uint64, hasDevice bool, links uint64, hasLinks bool, err error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false, 0, false, nil
	}
	return uint64(stat.Dev), true, uint64(stat.Nlink), true, nil
}

func permissionSafetyApplicable() bool { return true }
