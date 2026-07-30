//go:build !unix

package github

import "io/fs"

// fileOwnerUID has no meaningful value off unix (dev/Windows builds); return -1
// so the authorizer treats ownership as unverifiable. The production target is
// Linux, which uses fileowner_unix.go.
func fileOwnerUID(_ fs.FileInfo) int { return -1 }
