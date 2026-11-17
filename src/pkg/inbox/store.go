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
	// Intents are shares waiting for a transcript; persisted so a restart
	// resumes them.
	Intents map[string]*ShareIntent `json:"intents,omitempty"`
	// History is the append-only share history.
	History []*ShareRecord `json:"history,omitempty"`
	// DeletedAt is the tombstone of an item someone deleted from the Inbox
	// (delete.go): its source may still list it, the Inbox never does.
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	DeletedBy string     `json:"deleted_by,omitempty"`
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
		items[i].ShareIntents = cloneIntents(e.Intents)
		items[i].ShareHistory = cloneHistory(e.History)
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
	st.Note = res.Note
	if status == StatusFiled {
		st.URL = res.URL
		st.Children = append([]string(nil), res.Children...)
	}
	return *st, s.save()
}

// MarkSession records key as sent into session id: a delegated DestSession
// route whose note reads "session/<id> (<name>)". Sending again replaces it.
func (s *Store) MarkSession(key, id, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	note := DestSession + "/" + id
	if name != "" {
		note += " (" + name + ")"
	}
	s.entry(key).Routes[DestSession] = &RouteState{Status: StatusDelegated, Session: id, Note: note, UpdatedAt: s.now()}
	return s.save()
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

// SetIntent records (or replaces) a share waiting on a transcript.
func (s *Store) SetIntent(key string, in ShareIntent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	e := s.entry(key)
	if e.Intents == nil {
		e.Intents = map[string]*ShareIntent{}
	}
	c := in
	e.Intents[in.Destination] = &c
	return s.save()
}

// ClearIntent removes the intent for key/dest.
func (s *Store) ClearIntent(key, dest string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	if e := s.entries[key]; e != nil && e.Intents[dest] != nil {
		delete(e.Intents, dest)
		if len(e.Intents) == 0 {
			e.Intents = nil
		}
		_ = s.save()
	}
}

// FailIntent marks the intent failed with a reason; nothing was sent.
func (s *Store) FailIntent(key, dest, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	e := s.entries[key]
	if e == nil || e.Intents[dest] == nil {
		return nil
	}
	e.Intents[dest].Status, e.Intents[dest].Error, e.Intents[dest].UpdatedAt = IntentFailed, reason, s.now()
	return s.save()
}

// Intents returns a copy of key's share intents.
func (s *Store) Intents(key string) map[string]*ShareIntent {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	if e := s.entries[key]; e != nil {
		return cloneIntents(e.Intents)
	}
	return nil
}

// IntentKeys lists the items with a waiting share intent, sorted.
func (s *Store) IntentKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	var out []string
	for k, e := range s.entries {
		for _, in := range e.Intents {
			if in.Status == IntentWaiting {
				out = append(out, k)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// AppendShare adds a record to key's history (assigning its id) and persists
// it. The history is append-only: records are only updated in place.
func (s *Store) AppendShare(key string, rec ShareRecord) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	e := s.entry(key)
	rec.ID = fmt.Sprintf("sh-%d-%d", rec.StartedAt.UnixNano(), len(e.History))
	c := rec
	e.History = append(e.History, &c)
	if len(e.History) > maxShareRecords {
		e.History = e.History[len(e.History)-maxShareRecords:]
	}
	return rec.ID, s.save()
}

// UpdateShare applies fn to the record with id.
func (s *Store) UpdateShare(key, id string, fn func(*ShareRecord)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	e := s.entries[key]
	if e == nil {
		return nil
	}
	for _, r := range e.History {
		if r.ID == id {
			fn(r)
			return s.save()
		}
	}
	return nil
}

// FinishLatestShare applies fn to the newest unfinished record for dest.
func (s *Store) FinishLatestShare(key, dest string, fn func(*ShareRecord)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	e := s.entries[key]
	if e == nil {
		return nil
	}
	for i := len(e.History) - 1; i >= 0; i-- {
		if r := e.History[i]; r.Destination == dest && r.Status != ShareDone && r.Status != ShareFailed {
			fn(r)
			return s.save()
		}
	}
	return nil
}

// History returns a copy of key's share history, oldest first.
func (s *Store) History(key string) []*ShareRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	if e := s.entries[key]; e != nil {
		return cloneHistory(e.History)
	}
	return nil
}

// Forget drops every persisted state of one item (routes, intents, share
// history). It reports how many shares the history held. The copies those
// shares made elsewhere are not touched.
func (s *Store) Forget(key string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	e := s.entries[key]
	if e == nil {
		return 0, nil
	}
	n := len(e.History)
	delete(s.entries, key)
	return n, s.save()
}

// PendingRFCKeys lists the items with an RFC share still in progress.
func (s *Store) PendingRFCKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	var out []string
	for k, e := range s.entries {
		for _, r := range e.History {
			if r.Destination == ShareDestRFC && r.Status == ShareInProgress {
				out = append(out, k)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}
