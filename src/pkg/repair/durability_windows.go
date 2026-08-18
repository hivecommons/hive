//go:build windows

package repair

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// Windows does not permit FlushFileBuffers on a directory handle. File.Sync
// durably flushes newly created append-only files, while durableRename uses
// MOVEFILE_WRITE_THROUGH to flush an atomic replacement before returning.
func syncParentDirectory(_ string) error { return nil }

func durableRename(source, destination string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return fmt.Errorf("durably replace %s: %w", destination, err)
	}
	return nil
}
