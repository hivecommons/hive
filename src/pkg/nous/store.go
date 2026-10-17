package nous

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const storeVersion = 1

type stateFile struct {
	Version     int          `json:"version"`
	Experiments []Experiment `json:"experiments"`
}

// ExperimentStore persists a complete Hive-self experiment ledger, independently of the
// repo Experiment Lab. Methods serialize I/O on this instance; callers must
// serialize read-modify-write transactions and use a single writer per path.
type ExperimentStore struct {
	path string
	mu   sync.Mutex
}

// NewExperimentStore returns a ledger store backed by path, separate from the campaign Store.
func NewExperimentStore(path string) *ExperimentStore { return &ExperimentStore{path: path} }

func validateExperiments(experiments []Experiment) error {
	seen := map[string]bool{}
	for _, e := range experiments {
		if seen[e.ID] {
			return fmt.Errorf("duplicate experiment %q", e.ID)
		}
		if err := e.Validate(); err != nil {
			return fmt.Errorf("experiment %q: %w", e.ID, err)
		}
		seen[e.ID] = true
	}
	return nil
}

// Load treats only a missing file as an empty ledger. Corruption, unsupported
// versions and invalid records fail closed rather than silently dropping history.
func (s *ExperimentStore) Load() ([]Experiment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return []Experiment{}, nil
	}
	if err != nil {
		return nil, err
	}
	var state stateFile
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse nous ledger: %w", err)
	}
	if state.Version != storeVersion {
		return nil, fmt.Errorf("unsupported nous ledger version %d", state.Version)
	}
	if err := validateExperiments(state.Experiments); err != nil {
		return nil, err
	}
	if state.Experiments == nil {
		state.Experiments = []Experiment{}
	}
	return state.Experiments, nil
}

// Save validates before touching disk and atomically replaces the ledger with a
// private sibling temporary file. A failed write leaves the previous file intact.
func (s *ExperimentStore) Save(experiments []Experiment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateExperiments(experiments); err != nil {
		return err
	}
	if experiments == nil {
		experiments = []Experiment{}
	}
	data, err := json.MarshalIndent(stateFile{Version: storeVersion, Experiments: experiments}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".nous-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return err
	}
	return nil
}
