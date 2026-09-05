//go:build unix

package dashboard

import (
	"io/fs"
	"syscall"
)

// brandingFileOwnerUID returns the owning UID of a branding file on unix, which
// is the production target. -1 means "unknown", and the ownership guard treats
// that as unverifiable rather than as a refusal.
func brandingFileOwnerUID(fi fs.FileInfo) int {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st != nil {
		return int(st.Uid)
	}
	return -1
}
