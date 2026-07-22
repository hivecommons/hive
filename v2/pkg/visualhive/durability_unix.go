//go:build !windows

package visualhive

import (
	"fmt"
	"os"
	"path/filepath"
)

func syncLifecycleParentDirectory(path string) error {
	directory := filepath.Dir(path)
	file, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync lifecycle state directory %s: %w", directory, err)
	}
	return nil
}

func durableReplaceLifecycleFile(source, destination string) error {
	if err := os.Rename(source, destination); err != nil {
		return err
	}
	return syncLifecycleParentDirectory(destination)
}
