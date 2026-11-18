package pauseprov

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// MaxAgents bounds the agents whose last resume is kept.
const MaxAgents = 256

// IntentTTL is how long a declared resume actor waits for its transition.
const IntentTTL = 2 * time.Minute

// ResumeRecord is the last resume of one agent, persisted.
type ResumeRecord struct {
	Trigger string    `json:"trigger,omitempty"`
	By      string    `json:"by,omitempty"`
	Reason  string    `json:"reason,omitempty"`
	At      time.Time `json:"at"`
}

type fileShape struct {
	Version int                     `json:"version"`
	Resumes map[string]ResumeRecord `json:"resumes"`
}

type intent struct {
	by string
	at time.Time
}

// Store keeps who or what last resumed each agent. A nil *Store is usable and
// records nothing.
type Store struct {
	path   string
	logger *slog.Logger
	now    func() time.Time

	mu      sync.Mutex
	resumes map[string]ResumeRecord
	intents map[string]intent
}

// Open loads the store at path ("" keeps it in memory). A missing file is an
// empty store; an unreadable or corrupt one is returned empty with the error.
func Open(path string, logger *slog.Logger) (*Store, error) {
	s := &Store{path: path, logger: logger, now: time.Now, resumes: map[string]ResumeRecord{}, intents: map[string]intent{}}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("reading pause provenance %s: %w", path, err)
	}
	var f fileShape
	if err := json.Unmarshal(b, &f); err != nil {
		return s, fmt.Errorf("parsing pause provenance %s: %w", path, err)
	}
	for a, r := range f.Resumes {
		if a != "" && len(s.resumes) < MaxAgents {
			s.resumes[a] = r
		}
	}
	return s, nil
}

// SetClock replaces the clock; for tests.
func (s *Store) SetClock(now func() time.Time) {
	if s != nil && now != nil {
		s.now = now
	}
}

// IntendResume declares the person about to resume agent, so the transition
// that follows is attributed to them: the runtime's resume event carries a
// trigger and reason but not the acting user.
func (s *Store) IntendResume(agent, by string) {
	if s == nil || agent == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.intents[agent] = intent{by: by, at: s.now()}
}

// ObserveResume records a resume transition. The acting user is the declared
// intent when it is fresh and the trigger is the dashboard's.
func (s *Store) ObserveResume(agent string, e Event) {
	if s == nil || agent == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	by := e.By
	if in, ok := s.intents[agent]; ok {
		delete(s.intents, agent)
		if by == "" && e.Trigger == triggerDashboard && s.now().Sub(in.at) <= IntentTTL {
			by = in.by
		}
	}
	at := e.At
	if at.IsZero() {
		at = s.now()
	}
	s.resumes[agent] = ResumeRecord{Trigger: e.Trigger, By: quote(by), Reason: quote(e.Reason), At: at}
	s.trimLocked()
	s.saveLocked()
	if s.logger != nil {
		// Ids and kinds only: never the free-text reason.
		s.logger.Info("pause provenance: resume recorded", "agent", agent, "trigger", e.Trigger, "actor_kind", Classify(e.Trigger, by).Kind)
	}
}

// LastResume returns the last resume of agent, if the hive saw one.
func (s *Store) LastResume(agent string) *Event {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.resumes[agent]
	if !ok {
		return nil
	}
	return &Event{Trigger: r.Trigger, By: r.By, Reason: r.Reason, At: r.At}
}

func (s *Store) trimLocked() {
	if len(s.resumes) <= MaxAgents {
		return
	}
	type ent struct {
		name string
		at   time.Time
	}
	var all []ent
	for n, r := range s.resumes {
		all = append(all, ent{n, r.At})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.After(all[j].at) })
	for _, e := range all[MaxAgents:] {
		delete(s.resumes, e.name)
	}
}

func (s *Store) saveLocked() {
	if s.path == "" {
		return
	}
	b, err := json.Marshal(fileShape{Version: 1, Resumes: s.resumes})
	if err == nil {
		err = os.MkdirAll(filepath.Dir(s.path), 0o750)
	}
	if err == nil {
		tmp := s.path + ".tmp"
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, s.path)
		}
	}
	if err != nil && s.logger != nil {
		s.logger.Warn("pause provenance: saving failed", "error", err)
	}
}

var defaultStore struct {
	mu sync.RWMutex
	s  *Store
}

// SetDefault installs the process's store, read by the status builders.
func SetDefault(s *Store) {
	defaultStore.mu.Lock()
	defaultStore.s = s
	defaultStore.mu.Unlock()
}

// Default is the process's store, or nil.
func Default() *Store {
	defaultStore.mu.RLock()
	defer defaultStore.mu.RUnlock()
	return defaultStore.s
}
