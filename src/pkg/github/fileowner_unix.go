//go:build unix

package github

import (
	"io/fs"
	"syscall"
)

// fileOwnerUID returns the owning UID of a file from its FileInfo on unix. The
// container runs on Linux, where this is the agent's real UID — the trust anchor
// for "an agent can only speak for itself" in the PR-request watcher.
func fileOwnerUID(fi fs.FileInfo) int {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return -1
}
