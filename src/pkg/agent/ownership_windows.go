//go:build windows

package agent

import "os"

func fileOwnership(os.FileInfo) (uint32, uint32, bool) {
	return 0, 0, false
}
