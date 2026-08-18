//go:build !windows

package repair

import (
	"fmt"
	"os"
	"path/filepath"
)

func syncParentDirectory(path string) error {
	directory := filepath.Dir(path)
	file, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync directory %s: %w", directory, err)
	}
	return nil
}

func durableRename(source, destination string) error {
	if err := os.Rename(source, destination); err != nil {
		return err
	}
	return syncParentDirectory(destination)
}
