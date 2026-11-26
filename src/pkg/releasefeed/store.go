package releasefeed

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// stateFileVersion is bumped when the on-disk shape changes incompatibly.
const stateFileVersion = 1

// The state file holds versions and validators, never credentials.
const (
	stateDirPerm  os.FileMode = 0o755
	stateFilePerm os.FileMode = 0o644
)

// AgentState is what the watcher remembers for one subscribed agent.
type AgentState struct {
	// Seen maps a Source.Key to the version the agent was last told about
	// (or the baseline recorded the first time the feed answered).
	Seen map[string]string `json:"seen,omitempty"`
	// Pending are new versions not yet delivered in a kick: waiting out the
	// cooldown, or refused by a gate (agent paused, governor budget).
	Pending []Release `json:"pending,omitempty"`
	// LastKick is when the last release-feed kick for this agent went out.
	LastKick time.Time `json:"last_kick,omitzero"`
}

// State is the persisted watcher state.
type State struct {
	Version int                    `json:"version"`
	Sources map[string]SourceState `json:"sources,omitempty"`
	Agents  map[string]*AgentState `json:"agents,omitempty"`
	// IndexFieldsSeparated marks migrated state without changing the file
	// version, so a previous binary can still read the state on rollback.
	IndexFieldsSeparated bool `json:"index_fields_separated,omitempty"`
}

func newState() *State {
	return &State{
		Version: stateFileVersion, IndexFieldsSeparated: true,
		Sources: map[string]SourceState{}, Agents: map[string]*AgentState{},
	}
}

func (s *State) agent(name string) *AgentState {
	a := s.Agents[name]
	if a == nil {
		a = &AgentState{}
		s.Agents[name] = a
	}
	if a.Seen == nil {
		a.Seen = map[string]string{}
	}
	return a
}

// separateIndexFields drops legacy index state because the keys did not record
// which version field produced the saved versions and validators.
func (s *State) separateIndexFields() {
	isIndex := func(key string) bool { return strings.HasPrefix(key, KindIndex+":") }
	maps.DeleteFunc(s.Sources, func(key string, _ SourceState) bool { return isIndex(key) })
	for _, agent := range s.Agents {
		if agent == nil {
			continue
		}
		maps.DeleteFunc(agent.Seen, func(key, _ string) bool { return isIndex(key) })
		agent.Pending = slices.DeleteFunc(agent.Pending, func(r Release) bool { return isIndex(r.Source) })
	}
	s.IndexFieldsSeparated = true
}

// Store persists State.
type Store interface {
	Load() (*State, error)
	Save(*State) error
}

// FileStore is a JSON-file Store written atomically (temp file + rename).
type FileStore struct {
	path string
	mu   sync.Mutex
}

// NewFileStore returns a FileStore at path.
func NewFileStore(path string) *FileStore { return &FileStore{path: path} }

// Load reads the state; a missing file is an empty state.
func (f *FileStore) Load() (*State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return newState(), nil
	}
	if err != nil {
		return nil, err
	}
	st := &State{Version: stateFileVersion}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", f.path, err)
	}
	if st.Version > stateFileVersion {
		return nil, fmt.Errorf("%s has state version %d, this hive understands up to %d", f.path, st.Version, stateFileVersion)
	}
	if st.Sources == nil {
		st.Sources = map[string]SourceState{}
	}
	if st.Agents == nil {
		st.Agents = map[string]*AgentState{}
	}
	return st, nil
}

// Save writes st atomically.
func (f *FileStore) Save(st *State) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	st.Version = stateFileVersion
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.path), stateDirPerm); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.path), filepath.Base(f.path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Chmod(name, stateFilePerm); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, f.path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}
