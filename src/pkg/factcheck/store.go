package factcheck

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// DirEnvVar overrides the directory the loop state lives in.
	DirEnvVar = "HIVE_FACTCHECK_DIR"
	// DefaultDir sits on the /data volume so state survives pod rolls.
	DefaultDir = "/data/factcheck"

	stateFile = "state.json"
	// stateRetention is how long an untouched PR's record is kept.
	stateRetention = 30 * 24 * time.Hour
	dirPerm        = 0o755
	filePerm       = 0o644
)

// Dir is the state directory.
func Dir() string {
	if d := strings.TrimSpace(os.Getenv(DirEnvVar)); d != "" {
		return d
	}
	return DefaultDir
}

// Store persists State per PR key ("owner/repo#n"). A store that cannot read or
// write its file keeps working in memory, which is safe: the round budget then
// resets on restart, and the marked reviews on the PR still bound the loop.
type Store struct {
	mu   sync.Mutex
	path string
	PRs  map[string]*State `json:"prs"`
	// Touched is when each record last changed, for retention.
	Touched map[string]time.Time `json:"touched"`
}

// Open loads the store from dir. A missing file is an empty store; an
// unreadable one is returned with the error and is still usable in memory.
func Open(dir string) (*Store, error) {
	s := &Store{path: filepath.Join(dir, stateFile), PRs: map[string]*State{}, Touched: map[string]time.Time{}}
	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return s, err
	}
	var disk struct {
		PRs     map[string]*State    `json:"prs"`
		Touched map[string]time.Time `json:"touched"`
	}
	if err := json.Unmarshal(b, &disk); err != nil {
		return s, err
	}
	if disk.PRs != nil {
		s.PRs = disk.PRs
	}
	if disk.Touched != nil {
		s.Touched = disk.Touched
	}
	return s, nil
}

// Get returns a copy of the PR's state (zero when unseen).
func (s *Store) Get(key string) State {
	if s == nil {
		return State{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.PRs[key]; st != nil {
		return *st
	}
	return State{}
}

// Update applies fn to the PR's state and persists. The error is the save
// error; the in-memory change is kept either way.
func (s *Store) Update(key string, now time.Time, fn func(*State)) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.PRs[key]
	if st == nil {
		st = &State{}
		s.PRs[key] = st
	}
	fn(st)
	s.Touched[key] = now
	for k, at := range s.Touched {
		if now.Sub(at) > stateRetention {
			delete(s.Touched, k)
			delete(s.PRs, k)
		}
	}
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), dirPerm); err != nil {
		return err
	}
	b, err := json.MarshalIndent(struct {
		PRs     map[string]*State    `json:"prs"`
		Touched map[string]time.Time `json:"touched"`
	}{s.PRs, s.Touched}, "", " ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, filePerm); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
