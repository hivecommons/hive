package inbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// entryState is the persisted per-item state, keyed by Key(source, id).
type entryState struct {
	Routes    map[string]*RouteState `json:"routes,omitempty"`
	Dismissed bool                   `json:"dismissed,omitempty"`
}

// Store persists route states as one JSON file next to connector state.
type Store struct {
	Path string
	Now  func() time.Time

	mu      sync.Mutex
	entries map[string]*entryState
	loaded  bool
}

// NewStore returns a store backed by path (created on first save).
func NewStore(path string) *Store {
	return &Store{Path: path}
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Store) load() {
	if s.loaded {
		return
	}
	s.loaded = true
	s.entries = map[string]*entryState{}
	if s.Path == "" {
		return
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &s.entries)
	if s.entries == nil {
		s.entries = map[string]*entryState{}
	}
}

func (s *Store) save() error {
	if s.Path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}

func (s *Store) entry(key string) *entryState {
	e := s.entries[key]
	if e == nil {
		e = &entryState{Routes: map[string]*RouteState{}}
		s.entries[key] = e
	}
	if e.Routes == nil {
		e.Routes = map[string]*RouteState{}
	}
	return e
}

// Apply copies persisted state onto items and derives Processed.
func (s *Store) Apply(items []Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	for i := range items {
		e := s.entries[items[i].Key()]
		if e == nil {
			items[i].Processed = false
			continue
		}
		items[i].Dismissed = e.Dismissed
		items[i].Routes = cloneRoutes(e.Routes)
		items[i].Processed = e.Dismissed
		for _, r := range e.Routes {
			if r.Status == StatusFiled {
				items[i].Processed = true
			}
		}
	}
}

func cloneRoutes(in map[string]*RouteState) map[string]*RouteState {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*RouteState, len(in))
	for k, v := range in {
		c := *v
		c.Children = append([]string(nil), v.Children...)
		out[k] = &c
	}
	return out
}

// Route returns the current state for one destination, if any.
func (s *Store) Route(key, dest string) (RouteState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	e := s.entries[key]
	if e == nil || e.Routes[dest] == nil {
		return RouteState{}, false
	}
	return *e.Routes[dest], true
}

// ErrRouteBusy is returned when a route is queued or running already.
var ErrRouteBusy = fmt.Errorf("route already in progress")

// Begin transitions key/dest to queued. A queued or running route is
// rejected; filed and failed routes may be retried (filed = re-file).
func (s *Store) Begin(key, dest, repo string) (RouteState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	e := s.entry(key)
	if cur := e.Routes[dest]; cur != nil && (cur.Status == StatusQueued || cur.Status == StatusRunning) {
		return *cur, ErrRouteBusy
	}
	st := &RouteState{Status: StatusQueued, Repo: repo, UpdatedAt: s.now()}
	e.Routes[dest] = st
	return *st, s.save()
}

// Set replaces the status (and optional outcome) of key/dest.
func (s *Store) Set(key, dest, status string, res Result, errText string) (RouteState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	e := s.entry(key)
	st := e.Routes[dest]
	if st == nil {
		st = &RouteState{}
		e.Routes[dest] = st
	}
	st.Status = status
	st.UpdatedAt = s.now()
	st.Error = errText
	if status == StatusFiled {
		st.URL = res.URL
		st.Children = append([]string(nil), res.Children...)
	}
	return *st, s.save()
}

// Dismiss marks the item knowledge-only (or clears that mark).
func (s *Store) Dismiss(key string, dismissed bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	s.entry(key).Dismissed = dismissed
	return s.save()
}

// Known returns every key the store has seen, sorted (for tests/debugging).
func (s *Store) Known() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	out := make([]string, 0, len(s.entries))
	for k := range s.entries {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Seen reports whether the store has any record of key; Sweep uses it so
// auto-rules only fire on items that arrived after the rule existed.
func (s *Store) Seen(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	_, ok := s.entries[key]
	return ok
}

// MarkSeen records key without any route so a later Sweep skips it.
func (s *Store) MarkSeen(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	if _, ok := s.entries[key]; ok {
		return nil
	}
	s.entry(key)
	return s.save()
}
