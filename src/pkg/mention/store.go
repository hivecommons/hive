package mention

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Store struct {
	mu    sync.Mutex
	path  string
	state storeState
}

type storeState struct {
	Watermarks map[string]time.Time `json:"watermarks"`
	Seen       map[string]bool      `json:"seen"`
}

func NewStore(path string) (*Store, error) {
	s := &Store{path: path, state: storeState{Watermarks: map[string]time.Time{}, Seen: map[string]bool{}}}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &s.state); err != nil {
			return nil, err
		}
	}
	if s.state.Watermarks == nil {
		s.state.Watermarks = map[string]time.Time{}
	}
	if s.state.Seen == nil {
		s.state.Seen = map[string]bool{}
	}
	return s, nil
}

func (s *Store) Watermark(repo string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Watermarks[repo]
}

func (s *Store) Seen(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return id != "" && s.state.Seen[id]
}

func (s *Store) Mark(repo, id string, t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != "" {
		s.state.Seen[id] = true
	}
	if t.After(s.state.Watermarks[repo]) {
		s.state.Watermarks[repo] = t
	}
	return s.saveLocked()
}

func (s *Store) Advance(repo string, t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.After(s.state.Watermarks[repo]) {
		s.state.Watermarks[repo] = t
	}
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o600)
}
