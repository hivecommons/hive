//go:build windows

package visualhive

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func syncLifecycleParentDirectory(string) error {
	// File.Sync flushes data and metadata on Windows. Portable directory fsync
	// is unavailable for directory handles.
	return nil
}

func durableReplaceLifecycleFile(source, destination string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return fmt.Errorf("durably replace lifecycle state file %s: %w", destination, err)
	}
	return nil
}
