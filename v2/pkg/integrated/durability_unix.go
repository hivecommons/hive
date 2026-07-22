//go:build !windows

package integrated

import (
	"fmt"
	"os"
	"path/filepath"
)

func syncStateParentDirectory(path string) error {
	directory := filepath.Dir(path)
	file, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync state directory %s: %w", directory, err)
	}
	return nil
}

func durableReplaceStateFile(source, destination string) error {
	if err := os.Rename(source, destination); err != nil {
		return err
	}
	return syncStateParentDirectory(destination)
}

func durableRemoveStateFile(path string) error {
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			// A retry can observe a removal that completed before an earlier
			// directory sync failed. Sync even when the name is already absent
			// so a successful retry still proves the deletion is durable.
			return syncStateParentDirectory(path)
		}
		return err
	}
	return syncStateParentDirectory(path)
}
