package releasesentinel

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Store persists per-release records. Load returns an empty (non-nil) map
// when nothing has been saved yet.
type Store interface {
	Load() (map[string]*Record, error)
	Save(records map[string]*Record) error
}

// stateFileVersion is bumped when the on-disk shape changes incompatibly.
const stateFileVersion = 1

// stateDirPerm and stateFilePerm are the modes the state directory and file
// are created with: the file holds no secrets, but only the hive writes it.
const (
	stateDirPerm  os.FileMode = 0o755
	stateFilePerm os.FileMode = 0o644
)

type stateFile struct {
	Version  int                `json:"version"`
	Releases map[string]*Record `json:"releases"`
}

// FileStore is a JSON-file Store. Writes go to a sibling temp file that is
// renamed into place, so a crash mid-write leaves the
// previous state intact and a restart resumes from it.
type FileStore struct {
	path string
	mu   sync.Mutex
}

// NewFileStore returns a FileStore at path.
func NewFileStore(path string) *FileStore {
	return &FileStore{path: path}
}

// Path returns the file the store reads and writes.
func (f *FileStore) Path() string { return f.path }

// Load reads the state file. A missing file is an empty state, not an error;
// a corrupt one IS an error, so the sentinel stops rather than forgetting how
// many rounds a release already spent.
func (f *FileStore) Load() (map[string]*Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]*Record{}, nil
	}
	if err != nil {
		return nil, err
	}
	var sf stateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", f.path, err)
	}
	if sf.Version > stateFileVersion {
		return nil, fmt.Errorf("%s has state version %d, this hive understands up to %d", f.path, sf.Version, stateFileVersion)
	}
	out := make(map[string]*Record, len(sf.Releases))
	for name, r := range sf.Releases {
		if r != nil {
			out[name] = r
		}
	}
	return out, nil
}

// Save writes records atomically.
func (f *FileStore) Save(records map[string]*Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if records == nil {
		records = map[string]*Record{}
	}
	data, err := json.MarshalIndent(stateFile{Version: stateFileVersion, Releases: records}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, stateDirPerm); err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, data, stateFilePerm); err != nil {
		return err
	}
	if err := os.Rename(tmp, f.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
