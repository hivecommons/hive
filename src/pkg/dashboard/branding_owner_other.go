//go:build !unix

package dashboard

import "io/fs"

// brandingFileOwnerUID has no meaningful value off unix (dev/Windows builds);
// return -1 so the branding guard treats ownership as unverifiable and falls
// back to the mode and size checks, which are portable.
func brandingFileOwnerUID(_ fs.FileInfo) int { return -1 }
