package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const agentImportPendingSuffix = ".import-pending"

type importFile struct {
	path    string
	data    []byte
	mode    os.FileMode
	modTime time.Time
	written *contentSum
}

// agentImportFiles records the file replacements made before an imported
// agent is published. A config save can fail after writing one of its layers.
type agentImportFiles struct {
	sourcePath string
	files      map[string]*importFile
	order      []*importFile
}

func (f *agentImportFiles) backup(path string) (*importFile, error) {
	if previous, ok := f.files[path]; ok {
		return previous, nil
	}
	previous := &importFile{path: path}
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("cannot replace non-regular file %s", path)
		}
		previous.mode = info.Mode().Perm()
		previous.modTime = info.ModTime()
		previous.data, err = os.ReadFile(path)
		if err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f.files[path] = previous
	f.order = append(f.order, previous)
	return previous, nil
}

func (f *importFile) record(data []byte) {
	if f != nil {
		sum := sumBytes(data)
		f.written = &sum
	}
}

func (f *agentImportFiles) write(path string, data []byte, mode os.FileMode) error {
	if f == nil {
		return writeFileAtomic(path, data, mode)
	}
	target := ResolvedConfigPath(path)
	previous, err := f.backup(target)
	if err != nil {
		return err
	}
	return writeFileAtomicTracked(target, data, mode, previous.record)
}

// publishAgentOverlay makes the staged overlay visible to all config readers.
func (f *agentImportFiles) publishAgentOverlay(path string) error {
	previous := f.files[path]
	if previous.data == nil {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return ErrConfigChangedOnDisk
		}
	} else if current, ok := sumFile(path); !ok || current != sumBytes(previous.data) {
		return ErrConfigChangedOnDisk
	}
	stagedPath := path + agentImportPendingSuffix
	staged := f.files[stagedPath]
	if current, ok := sumFile(stagedPath); !ok || staged == nil || staged.written == nil || current != *staged.written {
		return ErrConfigChangedOnDisk
	}
	if err := os.Rename(stagedPath, path); err != nil {
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

func (f *agentImportFiles) rollback(saveErr error) error {
	for _, previous := range slices.Backward(f.order) {
		if previous.written == nil {
			continue
		}
		if current, ok := sumFile(previous.path); !ok || current != *previous.written {
			// A later edit belongs to the operator.
			continue
		}
		var err error
		if previous.data == nil {
			err = os.Remove(previous.path)
		} else {
			err = writeFileAtomic(previous.path, previous.data, previous.mode)
			if err == nil {
				// Reload gives a newer dashboard overlay precedence over the
				// primary config. Restore that ordering along with the bytes.
				err = os.Chtimes(previous.path, time.Time{}, previous.modTime)
				switch previous.path {
				case f.sourcePath, ResolvedConfigPath(RuntimeConfigFile), ResolvedConfigPath(DashboardOverlayFile):
					recordProcessWrite(previous.path, sumBytes(previous.data))
				}
			}
		}
		if err != nil {
			saveErr = errors.Join(saveErr, fmt.Errorf("restoring %s: %w", previous.path, err))
		}
	}
	return saveErr
}
