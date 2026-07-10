package repair

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const StateSchema = "hive.repair-worker-state.v1"

type Stage string

const (
	StagePrepared      Stage = "prepared"
	StageModelComplete Stage = "model_complete"
	StageValidated     Stage = "validated"
	StageCommitted     Stage = "committed"
	StagePushed        Stage = "pushed"
	StagePROpen        Stage = "pr_open"
)

type Attempt struct {
	Repository            string    `json:"repository"`
	RepositoryFingerprint string    `json:"repository_fingerprint"`
	Attempt               int       `json:"attempt"`
	Branch                string    `json:"branch"`
	Worktree              string    `json:"worktree"`
	Stage                 Stage     `json:"stage"`
	Provider              string    `json:"provider"`
	CommitSHA             string    `json:"commit_sha,omitempty"`
	PRNumber              int       `json:"pr_number,omitempty"`
	PRURL                 string    `json:"pr_url,omitempty"`
	ChangedFiles          []string  `json:"changed_files,omitempty"`
	StartedAt             time.Time `json:"started_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

type State struct {
	SchemaVersion string              `json:"schema_version"`
	Attempts      map[string]*Attempt `json:"attempts"`
}

type Store struct {
	mu   sync.Mutex
	path string
	data State
}

func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("repair state directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	store := &Store{path: filepath.Join(dir, "repair-worker-state.json"), data: State{SchemaVersion: StateSchema, Attempts: map[string]*Attempt{}}}
	data, err := os.ReadFile(store.path)
	if err == nil {
		if err := json.Unmarshal(data, &store.data); err != nil {
			return nil, fmt.Errorf("parse repair worker state: %w", err)
		}
		if store.data.SchemaVersion != StateSchema {
			return nil, fmt.Errorf("unsupported repair state schema %q", store.data.SchemaVersion)
		}
		if store.data.Attempts == nil {
			store.data.Attempts = map[string]*Attempt{}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return store, nil
}

func (s *Store) Get(fingerprint string) (Attempt, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt := s.data.Attempts[fingerprint]
	if attempt == nil {
		return Attempt{}, false
	}
	return *attempt, true
}

func (s *Store) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := State{SchemaVersion: s.data.SchemaVersion, Attempts: make(map[string]*Attempt, len(s.data.Attempts))}
	for key, attempt := range s.data.Attempts {
		if attempt != nil {
			copy := *attempt
			copy.ChangedFiles = append([]string(nil), attempt.ChangedFiles...)
			result.Attempts[key] = &copy
		}
	}
	return result
}

func (s *Store) Put(attempt Attempt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt.UpdatedAt = time.Now().UTC()
	copy := attempt
	s.data.Attempts[attempt.RepositoryFingerprint] = &copy
	return s.persistLocked()
}

func (s *Store) persistLocked() error {
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	temporary := s.path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, s.path); err != nil {
		return err
	}
	return nil
}
