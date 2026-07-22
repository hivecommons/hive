//go:build windows

package integrated

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func syncStateParentDirectory(string) error {
	// FlushFileBuffers on the newly created audit file (os.File.Sync) flushes
	// both data and file metadata on Windows. Directory handles do not support
	// the portable fsync operation used on Unix.
	return nil
}

func durableReplaceStateFile(source, destination string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return fmt.Errorf("durably replace state file %s: %w", destination, err)
	}
	return nil
}

func durableRemoveStateFile(path string) error {
	// Windows cannot FlushFileBuffers on a directory handle. A write-through
	// rename durably removes the live state name first; deleting the inert
	// tombstone afterward is cleanup that may safely be retried after a crash.
	tombstone := path + ".deleted"
	from, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(tombstone)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return fmt.Errorf("durably remove state file %s: %w", path, err)
		}
	}
	if err := os.Remove(tombstone); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove durable state tombstone %s: %w", tombstone, err)
	}
	return nil
}
